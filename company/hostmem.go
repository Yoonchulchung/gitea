// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"strconv"
	"strings"
)

// The limits are per app; the machine is one. Adding every running app's
// limit up against what the host has is the only way to see that thirty
// modest apps, or one generous one, have promised more memory than exists
// — before the kernel decides which of them finds out.

// hostMem is what the machine has.
type hostMem struct {
	TotalBytes     int64
	AvailableBytes int64 // what could be handed out now; HasAvailable says whether this host reports it
	HasAvailable   bool
}

// parseMeminfo reads Linux's /proc/meminfo. Values are in kB.
func parseMeminfo(text string) (hostMem, bool) {
	var m hostMem
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "MemTotal":
			m.TotalBytes = kb << 10
		case "MemAvailable":
			m.AvailableBytes = kb << 10
			m.HasAvailable = true
		}
	}
	return m, m.TotalBytes > 0
}

// memoryCommitmentMB adds up the memory limits of every app that is running,
// leaving out one app (the one whose limit is being changed, so the caller
// can add its new figure).
func memoryCommitmentMB(exceptOwner, exceptRepo string) int {
	total := 0
	for _, st := range ListAppStates() {
		if st.Actual != AppStateRunning || (st.Owner == exceptOwner && st.Repo == exceptRepo) {
			continue
		}
		total += SettingsFor(st.Owner, st.Repo).Limits.MemoryMB
	}
	return total
}

// memoryCommitWarnPct is where the fleet's committed memory turns amber:
// the host also runs Gitea, git and the database, and the limits are
// ceilings the apps are allowed to reach at once.
const memoryCommitWarnPct = 80
