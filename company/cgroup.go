// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"os/exec"
	"strconv"
	"sync"
)

// Hard limits, when the host offers them. rlimits cap a heap and the
// watchdog stops what strays, but neither is a wall the kernel holds: an
// app can still take the memory before the watchdog's next sample, and a
// CPU share is only what the watchdog says it is. A cgroup is that wall,
// and an unprivileged service gets one through systemd: `systemd-run
// --user --scope` moves the process into its own scope with MemoryMax and
// CPUQuota the kernel enforces, no root needed once the user's session
// exists (`loginctl enable-linger`). Probed once; without it the platform
// runs as before, and the admin page says which it is.

// cgroupProbe finds systemd-run and confirms a user scope can be made.
var cgroupProbe = sync.OnceValues(func() (string, error) {
	if companySetting("CGROUP_LIMITS") == "false" {
		return "", errors.New("switched off by [company] CGROUP_LIMITS")
	}
	path, err := exec.LookPath("systemd-run")
	if err != nil {
		return "", errors.New("systemd-run was not found; per-app memory and CPU are limits the watchdog enforces, not the kernel")
	}
	if out, err := exec.Command(path, "--user", "--scope", "--quiet", "-p", "MemoryMax=64M", "--", "/bin/true").CombinedOutput(); err != nil { //nolint:gosec // a fixed probe of the host's own systemd
		return "", errors.New("systemd-run --user cannot make a scope (is lingering enabled for the platform user?): " + string(out))
	}
	return path, nil
})

// cgroupArgs wraps argv so the process runs inside its own scope with the
// app's limits as kernel-enforced ceilings. MemoryHigh below MemoryMax
// lets the kernel slow an app that nears its limit before it kills it.
// Nothing to wrap with is not an error: the wall is a bonus, and the rest
// of the enforcement stays.
func cgroupArgs(limits AppLimits, argv []string) []string {
	path, err := cgroupProbe()
	if err != nil {
		return argv
	}
	out := []string{path, "--user", "--scope", "--quiet", "--collect"}
	if limits.MemoryMB > 0 {
		out = append(out,
			"-p", "MemoryMax="+strconv.Itoa(limits.MemoryMB)+"M",
			"-p", "MemoryHigh="+strconv.Itoa(limits.MemoryMB*9/10)+"M",
			"-p", "MemorySwapMax=0")
	}
	if limits.CPUPercent > 0 {
		out = append(out, "-p", "CPUQuota="+strconv.Itoa(limits.CPUPercent)+"%")
	}
	if limits.Processes > 0 {
		out = append(out, "-p", "TasksMax="+strconv.Itoa(limits.Processes))
	}
	out = append(out, "--")
	return append(out, argv...)
}

// CgroupStatus reports whether the kernel holds the limits here.
func CgroupStatus() (available bool, detail string) {
	if _, err := cgroupProbe(); err != nil {
		return false, err.Error()
	}
	return true, "systemd user scope: MemoryMax, CPUQuota and TasksMax are enforced by the kernel"
}
