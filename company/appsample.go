// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
)

// Gitea is the parent of every app process, so resource usage costs nothing
// to collect: /proc has it, and no root is needed to read your own children.
// That same sampling feeds two different things — the admin dashboard, and
// the watchdog that stops an app eating the machine.
//
// The watchdog is not the only memory control, and must not be: sampling
// every few seconds cannot catch an allocation spike, so RLIMIT_DATA (see
// appproc.go) is the *prevention* the kernel enforces instantly, and this is
// the *operations* layer that shuts an app down cleanly and says why.

const (
	sampleInterval = 5 * time.Second

	// memoryBreachesBeforeStop requires the limit to be exceeded on
	// consecutive samples. A single spike — a large request being parsed, a
	// garbage collection that has not run yet — is not a reason to kill an
	// app someone is using.
	memoryBreachesBeforeStop = 3
)

// StartMetricsFlusher runs the periodic bucket flush and resource sampling.
func StartMetricsFlusher() {
	ctx := graceful.GetManager().ShutdownContext()

	go func() {
		ticker := time.NewTicker(metricsFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				FlushMetrics() // don't lose the current window on shutdown
				return
			case <-ticker.C:
				FlushMetrics()
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sampleAllApps()
			}
		}
	}()
}

// cpuTracker remembers the previous CPU reading so a rate can be derived.
type cpuTracker struct {
	ticks float64
	at    time.Time
}

var (
	sampleStateMu sync.Mutex
	cpuTrackers   = map[string]cpuTracker{}
	memBreaches   = map[string]int{}
)

func sampleAllApps() {
	appRegistry.Range(func(_, v any) bool {
		if ref, ok := v.(AppRef); ok {
			sampleApp(ref.Owner, ref.Repo)
		}
		return true
	})
}

func sampleApp(owner, repo string) {
	s := lookupSupervisor(owner, repo)
	if s == nil {
		return
	}
	pid := s.pid()
	if pid == 0 {
		return
	}

	usage, ok := readProcessTreeUsage(pid)
	if !ok {
		return
	}

	key := appKey(owner, repo)
	sampleStateMu.Lock()
	prev, hadPrev := cpuTrackers[key]
	cpuTrackers[key] = cpuTracker{ticks: usage.cpuTicks, at: time.Now()}
	cpuPercent := 0.0
	if hadPrev {
		if elapsed := time.Since(prev.at).Seconds(); elapsed > 0 {
			// utime+stime are in clock ticks; 100 Hz is the near-universal
			// value and the figure only has to be good enough to spot a
			// change, not to bill anyone.
			cpuPercent = (usage.cpuTicks - prev.ticks) / 100 / elapsed * 100
		}
	}
	sampleStateMu.Unlock()

	settings := SettingsFor(owner, repo)
	RecordResourceSample(owner, repo, float64(usage.rssBytes), cpuPercent,
		float64(usage.threads), usage.fds, diskUsageMB(appPathsFor(owner, repo)))

	checkMemoryLimit(owner, repo, usage.rssBytes, settings)
}

// checkMemoryLimit stops an app that stays over its limit.
func checkMemoryLimit(owner, repo string, rssBytes int64, settings AppSettings) {
	limit := int64(settings.Limits.MemoryMB) << 20
	if limit <= 0 {
		return
	}
	key := appKey(owner, repo)

	sampleStateMu.Lock()
	if rssBytes <= limit {
		delete(memBreaches, key)
		sampleStateMu.Unlock()
		return
	}
	memBreaches[key]++
	breaches := memBreaches[key]
	if breaches < memoryBreachesBeforeStop {
		sampleStateMu.Unlock()
		return
	}
	delete(memBreaches, key)
	sampleStateMu.Unlock()

	log.Warn("company: %s/%s exceeded its memory limit (%d MB) on %d consecutive samples; stopping it",
		owner, repo, settings.Limits.MemoryMB, breaches)

	// Stopped rather than killed: SIGTERM first, so uvicorn finishes the
	// requests it is holding. And recorded with a reason, so the department
	// sees "stopped for using too much memory" with a button to request a
	// higher limit, rather than an app that is mysteriously off.
	if err := supervisorFor(owner, repo).Stop("platform", AppStateFailed, ReasonOOM); err != nil {
		log.Error("company: stopping %s/%s after memory limit: %v", owner, repo, err)
	}
	_ = MutateAppState(owner, repo, func(st *AppState) bool {
		st.Message = "the app was stopped for using more than its " +
			strconv.Itoa(settings.Limits.MemoryMB) + " MB memory limit"
		// Desired stays "running": the department did not switch this off, the
		// platform did, and they should be able to start it again after
		// fixing the cause or getting the limit raised.
		st.Desired = AppStateRunning
		return true
	})
}

// processUsage is one app's whole process tree, summed.
type processUsage struct {
	rssBytes int64
	threads  int
	fds      int
	cpuTicks float64
}

// readProcessTreeUsage sums usage over a process and its descendants.
//
// The tree matters: an app that spawns workers would otherwise report only
// its supervisor's few megabytes while the real usage sits in children the
// limit was meant to cover.
//
// Caveat worth stating where the number is shown: summing RSS double-counts
// pages shared between processes (the Python runtime, mostly), so this
// over-estimates. It is the right basis for "is this app near its limit"
// and the wrong basis for "exactly how much memory is in use".
func readProcessTreeUsage(rootPID int) (processUsage, bool) {
	if runtime.GOOS != "linux" {
		// /proc in this shape is Linux-only. Development hosts simply have no
		// resource figures; nothing else depends on them.
		return processUsage{}, false
	}

	children := map[int][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return processUsage{}, false
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if ppid, ok := readPPID(pid); ok {
			children[ppid] = append(children[ppid], pid)
		}
	}

	var usage processUsage
	seen := map[int]bool{}
	stack := []int{rootPID}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		addProcessUsage(pid, &usage)
		stack = append(stack, children[pid]...)
	}
	return usage, len(seen) > 0
}

func readPPID(pid int) (int, bool) {
	body, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// The comm field is parenthesised and may contain spaces, so fields are
	// counted from after the closing paren rather than from the start.
	commEnd := strings.LastIndex(string(body), ")")
	if commEnd < 0 {
		return 0, false
	}
	fields := strings.Fields(string(body)[commEnd+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	return ppid, err == nil
}

func addProcessUsage(pid int, usage *processUsage) {
	base := "/proc/" + strconv.Itoa(pid)

	if body, err := os.ReadFile(base + "/status"); err == nil {
		for line := range strings.SplitSeq(string(body), "\n") {
			switch {
			case strings.HasPrefix(line, "VmRSS:"):
				if kb, err := strconv.ParseInt(strings.Fields(line)[1], 10, 64); err == nil {
					usage.rssBytes += kb << 10
				}
			case strings.HasPrefix(line, "Threads:"):
				if n, err := strconv.Atoi(strings.Fields(line)[1]); err == nil {
					usage.threads += n
				}
			}
		}
	}
	if body, err := os.ReadFile(base + "/stat"); err == nil {
		if commEnd := strings.LastIndex(string(body), ")"); commEnd >= 0 {
			fields := strings.Fields(string(body)[commEnd+1:])
			// utime and stime are fields 14 and 15 of the full line, which is
			// 12 and 13 counting from after the comm field.
			if len(fields) > 12 {
				utime, _ := strconv.ParseFloat(fields[11], 64)
				stime, _ := strconv.ParseFloat(fields[12], 64)
				usage.cpuTicks += utime + stime
			}
		}
	}
	if fds, err := os.ReadDir(base + "/fd"); err == nil {
		usage.fds += len(fds)
	}
}

// diskUsageMB reports how much disk one app occupies. There is no quota to
// enforce without root, so this exists to make growth visible before it
// fills the volume.
func diskUsageMB(p appPaths) int {
	var total int64
	err := filepath.WalkDir(p.home, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a file that vanished mid-walk is not a failure
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0
	}
	return int(total >> 20)
}
