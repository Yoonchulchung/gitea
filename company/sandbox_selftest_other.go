// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !linux

package company

import (
	"fmt"
	"io"
)

// The sandbox only exists on Linux, so there is nothing here to check.
func RunSandboxSelfTest(out io.Writer, _ int) int {
	fmt.Fprintln(out, "The app sandbox is Linux-only; there is nothing to self-test on this host.")
	return 0
}
