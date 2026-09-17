// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux || darwin

package company

import "syscall"

// freeBytesOn reports the space left on the filesystem holding path.
//
// This is the only guard that protects the *host* rather than one app: app
// data has no disk quota behind it, because quotas need root and this
// platform has none (company/releasegc.go says the same about releases). A
// department filling its volume would take gitea.db down with it.
func freeBytesOn(path string) (int64, bool) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, false
	}
	// Bavail, not Bfree: the reserved blocks are not ours to spend.
	return int64(fs.Bavail * uint64(fs.Bsize)), true //nolint:gosec // block counts do not overflow int64 on real volumes
}
