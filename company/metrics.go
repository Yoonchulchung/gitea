// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// Every request to every app passes through the proxy, so metrics are
// collected there rather than by parsing logs — there is no nginx to parse
// and no journald to read (docs/company/app-platform.md).
//
// The hot path must stay cheap: recording a request takes one mutex on a
// small per-app struct and appends nothing. Aggregation into buckets happens
// in memory, and buckets reach disk only on a timer, from a background
// goroutine. No request ever writes a file.
//
// Metrics are observability, so every failure here is fail-open: a broken
// metrics file must never stop an app from serving
// (docs/company/app-platform-impl.md §4).

const (
	metricsBucketSeconds = 300 // 5 minutes
	metricsRetentionDays = 30
	metricsFlushInterval = time.Minute

	// Response times are kept as a bounded sample per bucket rather than
	// every observation: p95 from a few hundred samples is accurate enough
	// to spot a regression, and an unbounded slice would grow with traffic.
	metricsSampleCap = 512
)

// MetricsBucket is one five-minute window for one app.
type MetricsBucket struct {
	T   int64         `json:"t"` // unix seconds, bucket start
	Req RequestCounts `json:"req"`
	// Users is the number of distinct visitors: accounts for apps that
	// require sign-in, client addresses for the ones that don't. Those are
	// not comparable, which the admin screen has to say out loud.
	Users   int          `json:"users"`
	RT      ResponseTime `json:"rt"`
	Mem     MinMaxAvg    `json:"mem"` // bytes, VmRSS summed over the process tree
	CPU     MinMaxAvg    `json:"cpu"` // percent of one core
	Threads MinMaxAvg    `json:"threads"`
	// ResourceSamples is how many times memory, CPU and threads were actually
	// read in this window. Zero means they were never measured — not that
	// they measured zero. A bucket exists as soon as a request arrives, so
	// without this a host that cannot sample (no /proc) produced buckets full
	// of zeroes that every screen then displayed as "this app uses no
	// memory".
	ResourceSamples int `json:"resourceSamples,omitempty"`
	FDs             int `json:"fds"`    // max seen
	DiskMB          int `json:"diskMB"` // release tree + logs
	Restarts        int `json:"restarts"`
	Blocked         int `json:"blocked"` // downloads refused by policy
}

type RequestCounts struct {
	Total int `json:"total"`
	S2xx  int `json:"2xx"`
	S3xx  int `json:"3xx"`
	S4xx  int `json:"4xx"`
	S5xx  int `json:"5xx"`
}

type ResponseTime struct {
	Avg int `json:"avg"` // milliseconds
	P95 int `json:"p95"`
}

// MinMaxAvg carries the maximum as well as the average on purpose: an
// average hides the spike, and a spike is exactly what justifies raising a
// limit. "200MB average, 510MB peak against a 512MB cap" is the sentence an
// admin needs; the average alone says nothing is wrong.
type MinMaxAvg struct {
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// liveBucket accumulates the current window for one app.
type liveBucket struct {
	mu       sync.Mutex
	start    int64
	counts   RequestCounts
	visitors map[string]struct{}
	samples  []int // response times, ms, capped
	rtSum    int64
	rtCount  int64
	blocked  int
	restarts int

	memSum, memMax       float64
	cpuSum, cpuMax       float64
	threadSum, threadMax float64
	resourceSamples      int
	fdMax                int
	diskMB               int
}

var (
	liveMu      sync.Mutex
	liveBuckets = map[string]*liveBucket{}
)

func liveBucketFor(owner, repo string) *liveBucket {
	key := appKey(owner, repo)
	liveMu.Lock()
	defer liveMu.Unlock()
	b, ok := liveBuckets[key]
	if !ok {
		b = &liveBucket{start: bucketStart(time.Now()), visitors: map[string]struct{}{}}
		liveBuckets[key] = b
	}
	return b
}

func bucketStart(t time.Time) int64 {
	return t.Unix() - t.Unix()%metricsBucketSeconds
}

// RecordRequest is the hot path: called once per proxied request.
func RecordRequest(owner, repo string, status int, elapsed time.Duration, visitor string) {
	b := liveBucketFor(owner, repo)
	b.mu.Lock()
	defer b.mu.Unlock()

	b.counts.Total++
	switch {
	case status >= 500:
		b.counts.S5xx++
	case status >= 400:
		b.counts.S4xx++
	case status >= 300:
		b.counts.S3xx++
	default:
		b.counts.S2xx++
	}
	if visitor != "" {
		b.visitors[visitor] = struct{}{}
	}
	ms := int(elapsed.Milliseconds())
	b.rtSum += int64(ms)
	b.rtCount++
	if len(b.samples) < metricsSampleCap {
		b.samples = append(b.samples, ms)
	}
}

// RecordBlockedDownload notes a response the download policy refused. The
// count matters on its own: an app repeatedly trying to hand out files is a
// signal, whether or not it succeeded.
func RecordBlockedDownload(owner, repo string) {
	b := liveBucketFor(owner, repo)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blocked++
}

// RecordRestart notes that an app's process was replaced.
func RecordRestart(owner, repo string) {
	b := liveBucketFor(owner, repo)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.restarts++
}

// RecordResourceSample adds one poll of an app's process tree.
func RecordResourceSample(owner, repo string, rssBytes, cpuPercent, threads float64, fds, diskMB int) {
	b := liveBucketFor(owner, repo)
	b.mu.Lock()
	defer b.mu.Unlock()

	b.resourceSamples++
	b.memSum += rssBytes
	b.memMax = max(b.memMax, rssBytes)
	b.cpuSum += cpuPercent
	b.cpuMax = max(b.cpuMax, cpuPercent)
	b.threadSum += threads
	b.threadMax = max(b.threadMax, threads)
	b.fdMax = max(b.fdMax, fds)
	if diskMB > 0 {
		b.diskMB = diskMB
	}
}

// snapshotAndReset turns the accumulated window into a bucket and starts a
// new one. Returns false if nothing happened in this window, so idle apps
// don't fill the file with empty buckets.
func (b *liveBucket) snapshotAndReset(now int64) (MetricsBucket, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	empty := b.counts.Total == 0 && b.resourceSamples == 0 && b.restarts == 0 && b.blocked == 0
	bucket := MetricsBucket{
		T:        b.start,
		Req:      b.counts,
		Users:    len(b.visitors),
		Restarts: b.restarts,
		Blocked:  b.blocked,
		FDs:      b.fdMax,
		DiskMB:   b.diskMB,
	}
	if b.rtCount > 0 {
		bucket.RT = ResponseTime{Avg: int(b.rtSum / b.rtCount), P95: percentile(b.samples, 95)}
	}
	bucket.ResourceSamples = b.resourceSamples
	if b.resourceSamples > 0 {
		n := float64(b.resourceSamples)
		bucket.Mem = MinMaxAvg{Avg: b.memSum / n, Max: b.memMax}
		bucket.CPU = MinMaxAvg{Avg: b.cpuSum / n, Max: b.cpuMax}
		bucket.Threads = MinMaxAvg{Avg: b.threadSum / n, Max: b.threadMax}
	}

	b.start = now
	b.counts = RequestCounts{}
	b.visitors = map[string]struct{}{}
	b.samples = b.samples[:0]
	b.rtSum, b.rtCount = 0, 0
	b.blocked, b.restarts = 0, 0
	b.memSum, b.memMax = 0, 0
	b.cpuSum, b.cpuMax = 0, 0
	b.threadSum, b.threadMax = 0, 0
	b.resourceSamples, b.fdMax = 0, 0

	return bucket, !empty
}

// percentile returns the p-th percentile of samples, in place-sorted.
func percentile(samples []int, p int) int {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	sort.Ints(sorted)
	idx := (len(sorted)*p + 99) / 100 // ceil
	return sorted[min(idx, len(sorted))-1]
}

func metricsFile(owner, repo string) string {
	return filepath.Join(setting.AppDataPath, "company-metrics", appKey(owner, repo)+".json")
}

type metricsFileContent struct {
	Owner   string          `json:"owner"`
	Repo    string          `json:"repo"`
	Buckets []MetricsBucket `json:"buckets"`
}

// FlushMetrics writes every app's finished bucket to disk. Runs on a timer
// from a background goroutine — never from a request.
func FlushMetrics() {
	now := bucketStart(time.Now())

	liveMu.Lock()
	keys := make([]string, 0, len(liveBuckets))
	live := make([]*liveBucket, 0, len(liveBuckets))
	for k, b := range liveBuckets {
		keys = append(keys, k)
		live = append(live, b)
	}
	liveMu.Unlock()

	refs := map[string]AppRef{}
	appRegistry.Range(func(_, v any) bool {
		if ref, ok := v.(AppRef); ok {
			refs[appKey(ref.Owner, ref.Repo)] = ref
		}
		return true
	})

	for i, key := range keys {
		b := live[i]
		b.mu.Lock()
		due := b.start < now
		b.mu.Unlock()
		if !due {
			continue // still accumulating; nothing to write yet
		}
		bucket, worth := b.snapshotAndReset(now)
		ref, ok := refs[key]
		if !worth || !ok {
			continue
		}
		if err := appendMetricsBucket(ref.Owner, ref.Repo, bucket); err != nil {
			log.Error("company: writing metrics for %s/%s: %v", ref.Owner, ref.Repo, err)
		}
	}
}

// metricsWriteMu serializes the read-modify-write of a metrics file. Only
// the flusher writes these, but it can run concurrently with an admin page
// reading one, so the write still has to be atomic.
var metricsWriteMu sync.Mutex

func appendMetricsBucket(owner, repo string, bucket MetricsBucket) error {
	metricsWriteMu.Lock()
	defer metricsWriteMu.Unlock()

	file := metricsFile(owner, repo)
	content := metricsFileContent{Owner: owner, Repo: repo}
	if body, err := os.ReadFile(file); err == nil {
		if err := json.Unmarshal(body, &content); err != nil {
			// A corrupt metrics file loses history, which is regrettable but
			// not worth failing on: starting fresh is better than an app whose
			// metrics never record again.
			log.Error("company: metrics for %s/%s were unreadable, starting over: %v", owner, repo, err)
			content = metricsFileContent{Owner: owner, Repo: repo}
		}
	}

	content.Buckets = append(content.Buckets, bucket)
	cutoff := time.Now().AddDate(0, 0, -metricsRetentionDays).Unix()
	kept := content.Buckets[:0]
	for _, b := range content.Buckets {
		if b.T >= cutoff {
			kept = append(kept, b)
		}
	}
	content.Buckets = kept

	body, err := json.Marshal(content)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// LoadMetrics returns an app's buckets from since onwards, oldest first.
//
// The admin dashboard asks for a window ("last 24 hours"), never the whole
// file: 30 days is ~8,600 buckets per app and parsing all of them on every
// page load would make the dashboard the slowest page on the instance.
func LoadMetrics(owner, repo string, since time.Time) []MetricsBucket {
	body, err := os.ReadFile(metricsFile(owner, repo))
	if err != nil {
		return nil // no traffic yet is the normal case, not an error
	}
	var content metricsFileContent
	if err := json.Unmarshal(body, &content); err != nil {
		log.Error("company: metrics for %s/%s are unreadable: %v", owner, repo, err)
		return nil
	}
	cutoff := since.Unix()
	out := make([]MetricsBucket, 0, len(content.Buckets))
	for _, b := range content.Buckets {
		if b.T >= cutoff {
			out = append(out, b)
		}
	}
	return out
}

// CurrentMemoryMB is the most recent memory reading for an app, straight
// from the in-flight bucket.
//
// Read from memory rather than the metrics file on purpose: this feeds the
// repository home page's sidebar panel, which is the most-visited page in
// the instance, and a file read per view would be the wrong trade for a
// number that is only ever shown next to a limit.
func CurrentMemoryMB(owner, repo string) int {
	liveMu.Lock()
	b, ok := liveBuckets[appKey(owner, repo)]
	liveMu.Unlock()
	if !ok {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return int(b.memMax) >> 20
}

// MetricsSummary is the flattened view the dashboard tiles show.
type MetricsSummary struct {
	Requests  int
	Users     int
	ErrorRate float64 // percent
	P95       int     // milliseconds
	MemMaxMB  int
	Blocked   int
	Restarts  int
	// ResourcesMeasured is false when nothing in the window sampled memory or
	// CPU. The figures above are then absent rather than zero, and the screen
	// has to say so — an admin sizing a limit reads MemMaxMB as evidence.
	ResourcesMeasured bool
}

// SummarizeMetrics folds buckets into the headline numbers.
func SummarizeMetrics(buckets []MetricsBucket) MetricsSummary {
	var s MetricsSummary
	var errors, p95Sum, p95Count int
	for _, b := range buckets {
		s.Requests += b.Req.Total
		// Distinct-visitor counts cannot be summed across buckets without
		// double-counting someone who came back. The maximum in any one
		// window is a defensible lower bound; the exact figure would need
		// every visitor key kept for the whole window, which is precisely the
		// unbounded memory this design avoids.
		s.Users = max(s.Users, b.Users)
		errors += b.Req.S5xx
		s.Blocked += b.Blocked
		s.Restarts += b.Restarts
		if b.ResourceSamples > 0 {
			s.ResourcesMeasured = true
			s.MemMaxMB = max(s.MemMaxMB, int(b.Mem.Max)>>20)
		}
		if b.RT.P95 > 0 {
			p95Sum += b.RT.P95
			p95Count++
		}
	}
	if s.Requests > 0 {
		s.ErrorRate = float64(errors) * 100 / float64(s.Requests)
	}
	if p95Count > 0 {
		s.P95 = p95Sum / p95Count
	}
	return s
}
