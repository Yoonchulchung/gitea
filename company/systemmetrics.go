// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gitea.dev/modules/graceful"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// The machine as a whole, sampled once a minute: per-app charts say which app
// is heavy, and none of them says the disk is about to fill or the host has
// been pinned for an hour. Kept in memory for the dashboard and appended to a
// file so a restart does not blank the charts.

const (
	systemSampleInterval = time.Minute
	systemRetention      = 7 * 24 * time.Hour
	systemCompactEvery   = time.Hour
)

// SystemSample is one reading of the host.
type SystemSample struct {
	T           int64   `json:"t"`   // unix seconds
	CPU         float64 `json:"cpu"` // percent of every core together
	Load1       float64 `json:"load1"`
	MemUsedMB   int64   `json:"memUsedMB"`
	MemTotalMB  int64   `json:"memTotalMB"`
	DiskUsedMB  int64   `json:"diskUsedMB"` // the filesystem holding the data directory
	DiskTotalMB int64   `json:"diskTotalMB"`
	AppsMemMB   int64   `json:"appsMemMB"` // what the running apps hold, of MemUsedMB
	Apps        int     `json:"apps"`
}

func (s SystemSample) MemPercent() float64  { return percentOf(s.MemUsedMB, s.MemTotalMB) }
func (s SystemSample) DiskPercent() float64 { return percentOf(s.DiskUsedMB, s.DiskTotalMB) }

func percentOf(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

var (
	systemMu      sync.RWMutex
	systemSamples []SystemSample
)

func systemMetricsFile() string {
	return filepath.Join(setting.AppDataPath, "company-metrics", "_system.jsonl")
}

// StartSystemMetrics loads what was recorded before and samples from now on.
func StartSystemMetrics() {
	loadSystemSamples()
	ctx := graceful.GetManager().ShutdownContext()
	go func() {
		prev, _ := readCPUTimes()
		ticker := time.NewTicker(systemSampleInterval)
		defer ticker.Stop()
		lastCompact := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				sample, cur, ok := takeSystemSample(now, prev)
				prev = cur
				if !ok {
					continue
				}
				recordSystemSample(sample)
				if now.Sub(lastCompact) >= systemCompactEvery {
					compactSystemSamples()
					lastCompact = now
				}
			}
		}
	}()
}

func takeSystemSample(now time.Time, prev cpuTimes) (SystemSample, cpuTimes, bool) {
	s := SystemSample{T: now.Unix()}
	cur, cpuOK := readCPUTimes()
	if cpuOK {
		s.CPU = cpuPercentBetween(prev, cur)
	}
	mem, memOK := hostMemory()
	if memOK {
		s.MemTotalMB = mem.TotalBytes >> 20
		if mem.HasAvailable {
			s.MemUsedMB = (mem.TotalBytes - mem.AvailableBytes) >> 20
		}
	}
	if used, total, ok := diskUsage(setting.AppDataPath); ok {
		s.DiskUsedMB, s.DiskTotalMB = used>>20, total>>20
	}
	s.Load1, _ = loadAverage()
	for _, st := range ListAppStates() {
		if st.Actual == AppStateRunning {
			s.Apps++
			s.AppsMemMB += int64(CurrentMemoryMB(st.Owner, st.Repo))
		}
	}
	return s, cur, cpuOK || memOK
}

// cpuTimes is the busy and total jiffies from /proc/stat's first line.
type cpuTimes struct{ busy, total uint64 }

func cpuPercentBetween(a, b cpuTimes) float64 {
	if b.total <= a.total || b.busy < a.busy {
		return 0
	}
	return float64(b.busy-a.busy) * 100 / float64(b.total-a.total)
}

func recordSystemSample(s SystemSample) {
	systemMu.Lock()
	systemSamples = append(systemSamples, s)
	systemMu.Unlock()

	line, err := json.Marshal(s)
	if err != nil {
		return
	}
	file := systemMetricsFile()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		log.Error("company: system metrics: %v", err)
		return
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Error("company: system metrics: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func loadSystemSamples() {
	body, err := os.ReadFile(systemMetricsFile())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-systemRetention).Unix()
	var loaded []SystemSample
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		var s SystemSample
		if json.Unmarshal(sc.Bytes(), &s) == nil && s.T >= cutoff {
			loaded = append(loaded, s)
		}
	}
	systemMu.Lock()
	systemSamples = loaded
	systemMu.Unlock()
}

// compactSystemSamples drops what is past retention, in memory and on disk.
func compactSystemSamples() {
	cutoff := time.Now().Add(-systemRetention).Unix()
	systemMu.Lock()
	i := 0
	for i < len(systemSamples) && systemSamples[i].T < cutoff {
		i++
	}
	systemSamples = append([]SystemSample(nil), systemSamples[i:]...)
	kept := systemSamples
	systemMu.Unlock()

	var buf bytes.Buffer
	for _, s := range kept {
		line, _ := json.Marshal(s)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := writeFileAtomic(systemMetricsFile(), buf.Bytes()); err != nil {
		log.Error("company: compacting system metrics: %v", err)
	}
}

// SystemSamplesSince is the window a chart shows, averaged into at most
// maxPoints so a week does not draw ten thousand points.
func SystemSamplesSince(since time.Time, maxPoints int) []SystemSample {
	systemMu.RLock()
	var window []SystemSample
	for _, s := range systemSamples {
		if s.T >= since.Unix() {
			window = append(window, s)
		}
	}
	systemMu.RUnlock()
	return downsampleSystem(window, maxPoints)
}

func downsampleSystem(in []SystemSample, maxPoints int) []SystemSample {
	if maxPoints <= 0 || len(in) <= maxPoints {
		return in
	}
	per := (len(in) + maxPoints - 1) / maxPoints
	out := make([]SystemSample, 0, maxPoints)
	for start := 0; start < len(in); start += per {
		group := in[start:min(start+per, len(in))]
		var avg SystemSample
		for _, s := range group {
			avg.CPU += s.CPU
			avg.Load1 += s.Load1
			avg.MemUsedMB += s.MemUsedMB
			avg.AppsMemMB += s.AppsMemMB
			avg.DiskUsedMB = max(avg.DiskUsedMB, s.DiskUsedMB)
			avg.Apps = max(avg.Apps, s.Apps)
		}
		n := len(group)
		last := group[n-1]
		avg.T = group[0].T
		avg.CPU /= float64(n)
		avg.Load1 /= float64(n)
		avg.MemUsedMB /= int64(n)
		avg.AppsMemMB /= int64(n)
		avg.MemTotalMB, avg.DiskTotalMB = last.MemTotalMB, last.DiskTotalMB
		out = append(out, avg)
	}
	return out
}

// LatestSystemSample is the most recent reading, if there is one.
func LatestSystemSample() (SystemSample, bool) {
	systemMu.RLock()
	defer systemMu.RUnlock()
	if len(systemSamples) == 0 {
		return SystemSample{}, false
	}
	return systemSamples[len(systemSamples)-1], true
}
