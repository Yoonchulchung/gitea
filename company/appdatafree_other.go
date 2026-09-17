// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !linux && !darwin

package company

// No portable way to ask, so the host floor is not enforced here. Fail-open
// is the right answer on exactly these hosts: without a sandbox this
// platform already refuses to run apps in production (appsandbox.go), so
// what is left is a developer's machine.
func freeBytesOn(string) (int64, bool) { return 0, false }
