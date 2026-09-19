// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"gitea.dev/modules/log"
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
//
// systemd-run --user finds the session through XDG_RUNTIME_DIR and the bus
// socket under it. The app's environment is built from nothing (buildEnv),
// so the wrapper is given those two names and `env -u` takes them away
// again before the app runs: the app must not be able to reach the
// platform user's session bus, which could make scopes of its own. And the
// bus is checked at every start, not only at the probe: a session that was
// there when the platform started (an SSH login) can be gone later, and a
// wrapper that cannot connect fails every start with "Failed to connect to
// bus" — the app then runs unwrapped, and the log says so.

var sessionBusEnvNames = []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}

// sessionBusSocket is where systemd-run --user will look, from the
// platform's own environment.
func sessionBusSocket() string {
	if addr, ok := strings.CutPrefix(os.Getenv("DBUS_SESSION_BUS_ADDRESS"), "unix:path="); ok {
		return addr
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "bus")
	}
	return ""
}

// sessionBusEnv is what the wrapper needs, taken from the platform's own
// environment.
func sessionBusEnv() []string {
	var out []string
	for _, name := range sessionBusEnvNames {
		if v := os.Getenv(name); v != "" {
			out = append(out, name+"="+v)
		}
	}
	return out
}

func sessionBusReachable() (string, error) {
	socket := sessionBusSocket()
	if socket == "" {
		return "", errors.New("XDG_RUNTIME_DIR is not set in the platform's environment, so systemd-run --user has no session bus to reach (start the platform from a login session or a user service, and enable lingering for its account)")
	}
	if _, err := os.Stat(socket); err != nil {
		return "", errors.New("the session bus " + socket + " does not exist (is lingering enabled for the platform user? `loginctl enable-linger`)")
	}
	return socket, nil
}

type cgroupTools struct {
	systemdRun string
	env        string // coreutils env, to take the bus variables away from the app; "" when not found
}

// cgroupProbe finds systemd-run and confirms a user scope can be made, with
// the same environment a start will use.
var cgroupProbe = sync.OnceValues(func() (cgroupTools, error) {
	if companySetting("CGROUP_LIMITS") == "false" {
		return cgroupTools{}, errors.New("switched off by [company] CGROUP_LIMITS")
	}
	path, err := exec.LookPath("systemd-run")
	if err != nil {
		return cgroupTools{}, errors.New("systemd-run was not found; per-app memory and CPU are limits the watchdog enforces, not the kernel")
	}
	if _, err := sessionBusReachable(); err != nil {
		return cgroupTools{}, err
	}
	probe := exec.Command(path, "--user", "--scope", "--quiet", "-p", "MemoryMax=64M", "--", "/bin/true") //nolint:gosec // a fixed probe of the host's own systemd
	probe.Env = append([]string{"PATH=/usr/local/bin:/usr/bin:/bin"}, sessionBusEnv()...)
	if out, err := probe.CombinedOutput(); err != nil {
		return cgroupTools{}, errors.New("systemd-run --user cannot make a scope: " + strings.TrimSpace(string(out)))
	}
	envPath, _ := exec.LookPath("env")
	return cgroupTools{systemdRun: path, env: envPath}, nil
})

var cgroupBusWarn sync.Once

// cgroupActive reports whether starts are wrapped right now: the probe
// passed, and the bus is still there.
func cgroupActive() bool {
	if _, err := cgroupProbe(); err != nil {
		return false
	}
	if _, err := sessionBusReachable(); err != nil {
		cgroupBusWarn.Do(func() {
			log.Warn("company: kernel limits are off from here on: %v; apps start without a cgroup", err)
		})
		return false
	}
	return true
}

// cgroupArgs wraps argv so the process runs inside its own scope with the
// app's limits as kernel-enforced ceilings. Nothing to wrap with is not an
// error: the wall is a bonus, and the rest of the enforcement stays.
func cgroupArgs(limits AppLimits, argv []string) []string {
	if !cgroupActive() {
		return argv
	}
	tools, _ := cgroupProbe()
	return cgroupWrap(tools, limits, argv)
}

// cgroupWrap is the wrapper's argv. MemoryHigh below MemoryMax lets the
// kernel slow an app that nears its limit before it kills it.
func cgroupWrap(tools cgroupTools, limits AppLimits, argv []string) []string {
	out := []string{tools.systemdRun, "--user", "--scope", "--quiet", "--collect"}
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
	if tools.env != "" {
		out = append(out, tools.env)
		for _, name := range sessionBusEnvNames {
			out = append(out, "-u", name)
		}
	}
	return append(out, argv...)
}

// CgroupStatus reports whether the kernel holds the limits here.
func CgroupStatus() (available bool, detail string) {
	if _, err := cgroupProbe(); err != nil {
		return false, err.Error()
	}
	if _, err := sessionBusReachable(); err != nil {
		return false, "the probe passed at start, but now: " + err.Error()
	}
	return true, "systemd user scope: MemoryMax, CPUQuota and TasksMax are enforced by the kernel"
}
