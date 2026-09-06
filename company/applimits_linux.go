// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux

package company

import (
	"os"
	"strconv"
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
