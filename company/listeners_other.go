// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !linux

package company

// No per-namespace socket table to read here, and no sandbox to make one
// meaningful. Development hosts do without the watchdog and the page says so.
func listenerWatchSupported() bool { return false }

func listeningPorts(int) []int { return nil }
