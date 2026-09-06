// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetMetrics(t *testing.T) {
	t.Helper()
	liveMu.Lock()
	liveBuckets = map[string]*liveBucket{}
	liveMu.Unlock()
}

func TestRecordRequestBuckets(t *testing.T) {
	withTempAppData(t)
	resetMetrics(t)

	RecordRequest("PO", "app", 200, 10*time.Millisecond, "u:1")
	RecordRequest("PO", "app", 200, 30*time.Millisecond, "u:1") // same visitor
	RecordRequest("PO", "app", 404, 20*time.Millisecond, "u:2")
	RecordRequest("PO", "app", 500, 90*time.Millisecond, "a:10.0.0.1")
	RecordBlockedDownload("PO", "app")

	bucket, worth := liveBucketFor("PO", "app").snapshotAndReset(bucketStart(time.Now()))
	require.True(t, worth)

	assert.Equal(t, 4, bucket.Req.Total)
	assert.Equal(t, 2, bucket.Req.S2xx)
	assert.Equal(t, 1, bucket.Req.S4xx)
	assert.Equal(t, 1, bucket.Req.S5xx)
	assert.Equal(t, 3, bucket.Users, "the same visitor twice is one person")
	assert.Equal(t, 37, bucket.RT.Avg)
	assert.Equal(t, 1, bucket.Blocked)

	t.Run("the window resets", func(t *testing.T) {
		_, worth := liveBucketFor("PO", "app").snapshotAndReset(bucketStart(time.Now()))
		assert.False(t, worth, "an idle window writes nothing rather than an empty bucket")
	})
}

// The maximum is the point: an average hides the spike, and the spike is
// what justifies raising a limit.
func TestResourceSamplesKeepMaximum(t *testing.T) {
	withTempAppData(t)
	resetMetrics(t)

	RecordResourceSample("PO", "app", 100<<20, 10, 4, 30, 512)
	RecordResourceSample("PO", "app", 500<<20, 90, 8, 40, 512)

	assert.Equal(t, 500, CurrentMemoryMB("PO", "app"))

	bucket, _ := liveBucketFor("PO", "app").snapshotAndReset(bucketStart(time.Now()))
	assert.InDelta(t, float64(300<<20), bucket.Mem.Avg, 1)
	assert.InDelta(t, float64(500<<20), bucket.Mem.Max, 1)
	assert.InDelta(t, 50.0, bucket.CPU.Avg, 0.01)
	assert.InDelta(t, 90.0, bucket.CPU.Max, 0.01)
	assert.Equal(t, 40, bucket.FDs)
	assert.Equal(t, 512, bucket.DiskMB)
}

func TestPercentile(t *testing.T) {
	assert.Equal(t, 0, percentile(nil, 95))
	assert.Equal(t, 5, percentile([]int{5}, 95))
	// 100 samples 1..100: the 95th percentile is 95.
	samples := make([]int, 100)
	for i := range samples {
		samples[i] = i + 1
	}
	assert.Equal(t, 95, percentile(samples, 95))
	assert.Equal(t, 50, percentile(samples, 50))
	// Order in must not change the answer.
	assert.Equal(t, 30, percentile([]int{30, 10, 20}, 95))
}

func TestSummarizeMetrics(t *testing.T) {
	summary := SummarizeMetrics([]MetricsBucket{
		{Req: RequestCounts{Total: 100, S5xx: 2}, Users: 5, RT: ResponseTime{P95: 100}, Mem: MinMaxAvg{Max: 300 << 20}, ResourceSamples: 1},
		{Req: RequestCounts{Total: 100, S5xx: 0}, Users: 9, RT: ResponseTime{P95: 200}, Mem: MinMaxAvg{Max: 510 << 20}, ResourceSamples: 1},
	})
	assert.Equal(t, 200, summary.Requests)
	assert.InDelta(t, 1.0, summary.ErrorRate, 0.01)
	assert.Equal(t, 150, summary.P95)
	assert.Equal(t, 510, summary.MemMaxMB, "the peak is what gets compared against the limit")
	// Distinct visitors cannot be summed across windows without counting
	// someone who came back twice, so the busiest window is the honest
	// figure — see the note SummarizeMetrics carries.
	assert.Equal(t, 9, summary.Users)
}

func TestMetricsFileRoundTrip(t *testing.T) {
	withTempAppData(t)
	RegisterApp("PO", "app")
	t.Cleanup(func() { UnregisterApp("PO", "app") })

	now := time.Now().Unix()
	require.NoError(t, appendMetricsBucket("PO", "app", MetricsBucket{T: now - 60, Req: RequestCounts{Total: 3}}))
	require.NoError(t, appendMetricsBucket("PO", "app", MetricsBucket{T: now, Req: RequestCounts{Total: 7}}))

	all := LoadMetrics("PO", "app", time.Now().Add(-time.Hour))
	require.Len(t, all, 2)
	assert.Equal(t, 3, all[0].Req.Total)

	// The dashboard asks for a window, never the whole file: 30 days is
	// thousands of buckets per app.
	recent := LoadMetrics("PO", "app", time.Unix(now, 0))
	require.Len(t, recent, 1)
	assert.Equal(t, 7, recent[0].Req.Total)

	t.Run("an app with no traffic is not an error", func(t *testing.T) {
		assert.Empty(t, LoadMetrics("PO", "silent", time.Now().Add(-time.Hour)))
	})
}

// The proxy calls RecordRequest from every request goroutine at once.
func TestRecordRequestIsRaceFree(t *testing.T) {
	withTempAppData(t)
	resetMetrics(t)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			RecordRequest("PO", "app", 200, time.Millisecond, "u:"+string(rune('a'+i%5)))
			RecordResourceSample("PO", "app", 1<<20, 1, 1, 1, 1)
		})
	}
	wg.Wait()

	bucket, _ := liveBucketFor("PO", "app").snapshotAndReset(bucketStart(time.Now()))
	assert.Equal(t, 50, bucket.Req.Total)
	assert.Equal(t, 5, bucket.Users)
}

// A bucket exists as soon as a request arrives, so a host that cannot read an
// app's usage produced buckets full of zeroes — which every screen then showed
// as "this app uses no memory". An admin sizing a limit reads that as evidence.
func TestUnmeasuredResourcesAreNotReportedAsZero(t *testing.T) {
	// Requests happened; nothing sampled memory.
	unmeasured := SummarizeMetrics([]MetricsBucket{
		{Req: RequestCounts{Total: 10}, RT: ResponseTime{P95: 50}},
	})
	assert.False(t, unmeasured.ResourcesMeasured)
	assert.Zero(t, unmeasured.MemMaxMB)
	assert.Equal(t, 10, unmeasured.Requests, "request metrics come from the proxy and are unaffected")
	assert.Equal(t, 50, unmeasured.P95)

	// A genuine zero — sampled, and the app really used nothing measurable —
	// is still reported as measured, so the screen shows a number.
	measured := SummarizeMetrics([]MetricsBucket{
		{Req: RequestCounts{Total: 10}, ResourceSamples: 3},
	})
	assert.True(t, measured.ResourcesMeasured)
}
