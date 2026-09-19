// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import "golang.org/x/sys/unix"

// hostMemory reads the machine's size; what is free right now is not
// reported here — this is the development host, and the figure that
// matters on it is whether the limits add up to more than the machine.
func hostMemory() (hostMem, bool) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil || total == 0 {
		return hostMem{}, false
	}
	return hostMem{TotalBytes: int64(total)}, true //nolint:gosec // a machine's RAM fits in int64
}
