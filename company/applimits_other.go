// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !linux

package company

// Both halves of deprioritizeAndProtect are Linux-specific: procfs for the
// OOM score, and a priority nudge that only matters next to the sandbox and
// watchdog this platform only has on Linux. Development hosts do without.
func deprioritizeAndProtect(int) {}
