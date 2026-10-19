// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !linux

package company

func readCPUTimes() (cpuTimes, bool)                { return cpuTimes{}, false }
func diskUsage(string) (used, total int64, ok bool) { return 0, 0, false }
func loadAverage() (float64, bool)                  { return 0, false }
