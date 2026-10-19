// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func readCPUTimes() (cpuTimes, bool) {
	body, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	return parseCPUTimes(string(body))
}

// parseCPUTimes reads the aggregate "cpu" line: idle and iowait are the idle
// columns, everything else up to steal is busy.
func parseCPUTimes(stat string) (cpuTimes, bool) {
	line, _, _ := strings.Cut(stat, "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, false
	}
	var t cpuTimes
	for i, f := range fields[1:min(len(fields), 9)] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return cpuTimes{}, false
		}
		t.total += v
		if i != 3 && i != 4 { // idle, iowait
			t.busy += v
		}
	}
	return t, true
}

func diskUsage(path string) (used, total int64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	total = int64(st.Blocks) * st.Bsize
	return total - int64(st.Bfree)*st.Bsize, total, total > 0
}

func loadAverage() (float64, bool) {
	body, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	first, _, _ := strings.Cut(string(body), " ")
	v, err := strconv.ParseFloat(first, 64)
	return v, err == nil
}
