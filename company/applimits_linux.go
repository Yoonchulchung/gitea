// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux

package company

import (
	"os"
	"strconv"
	"strings"
	"syscall"

	"gitea.dev/modules/log"
)

// deprioritizeAndProtect lowers an app's scheduling priority and makes the
// kernel prefer it over Gitea when memory runs out.
//
// A hard CPU cap needs cgroups, which need root. Priority is the part
// available unprivileged, and it addresses the actual worry: not that an app
// uses CPU, but that an app using all of it makes Gitea unresponsive and
// takes the whole platform's only control surface with it.
//
// The OOM half matters for the same reason. If the machine runs out of
// memory and the kernel picks Gitea as its victim, every department's app
// goes down along with the only way to fix anything. Raising a process's own
// oom_score_adj is permitted unprivileged, so this is very cheap insurance.
func deprioritizeAndProtect(pid int) {
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10); err != nil {
		log.Warn("company: could not lower the priority of app process %d: %v", pid, err)
	}
	// 500 out of 0-1000: clearly more attractive to the OOM killer than Gitea
	// (which stays at 0), without jumping ahead of a genuinely runaway
	// process that deserves to be picked first.
	path := "/proc/" + strconv.Itoa(pid) + "/oom_score_adj"
	if err := os.WriteFile(path, []byte("500"), 0o600); err != nil {
		log.Warn("company: could not adjust the OOM score of app process %d: %v", pid, err)
	}
}

// nprocLimit is the RLIMIT_NPROC that gives an app `allowance` tasks of its own.
//
// The kernel counts that limit across every process and thread of the real
// user ID, not per process tree, and apps run as the same user as Gitea and
// whatever else that account runs. A bare 64 was already spent before the app
// started, and Python then failed its first thread ("can't start new thread").
// So the allowance goes on top of what the user is running at launch, with a
// margin for that to grow: a fork bomb is still stopped a few hundred tasks in,
// and the exact per-app cap is the cgroup's TasksMax where cgroups are
// available (company/cgroup.go).
func nprocLimit(allowance int) uint64 {
	if allowance <= 0 {
		return 0
	}
	// The count is a snapshot: the account's other programs keep starting
	// threads after the app does, and with only the allowance on top a busy
	// editor alone used it up ("can't start new thread" mid-request).
	return uint64(uidTaskCount(os.Getuid()) + allowance + nprocUserGrowthMargin)
}

// nprocUserGrowthMargin is room for the rest of the account to grow while the app runs.
const nprocUserGrowthMargin = 512

// uidTaskCount counts the tasks (threads) whose real user is uid, which is
// what RLIMIT_NPROC is checked against.
func uidTaskCount(uid int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	want := strconv.Itoa(uid)
	total := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		status, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue // exited while we looked
		}
		var realUID string
		threads := 0
		for line := range strings.SplitSeq(string(status), "\n") {
			if v, ok := strings.CutPrefix(line, "Uid:"); ok {
				if f := strings.Fields(v); len(f) > 0 {
					realUID = f[0]
				}
			} else if v, ok := strings.CutPrefix(line, "Threads:"); ok {
				threads, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		if realUID == want {
			total += threads
		}
	}
	return total
}
