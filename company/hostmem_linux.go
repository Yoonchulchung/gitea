// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import "os"

// hostMemory reads what the machine has and can hand out now.
func hostMemory() (hostMem, bool) {
	body, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return hostMem{}, false
	}
	return parseMeminfo(string(body))
}
