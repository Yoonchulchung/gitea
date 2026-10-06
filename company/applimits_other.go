// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !linux

package company

// Both halves of deprioritizeAndProtect are Linux-specific: procfs for the
// OOM score, and a priority nudge that only matters next to the sandbox and
// watchdog this platform only has on Linux. Development hosts do without.
func deprioritizeAndProtect(int) {}

// nprocLimit has no per-user task accounting to work around off Linux.
func nprocLimit(allowance int) uint64 {
	if allowance <= 0 {
		return 0
	}
	return uint64(allowance)
}
