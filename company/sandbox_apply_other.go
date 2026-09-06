// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !linux

package company

import "errors"

// Landlock and seccomp are Linux kernel features. On a development machine
// there is nothing to fall back to and nothing that needs protecting, so the
// helper simply refuses; buildAppCommand never reaches for it there, because
// allowUnsandboxed already lets non-Linux hosts run apps directly.
type sandboxSpec struct {
	ReadOnly     []string
	ReadWrite    []string
	AllowNetwork bool
	GiteaPID     int
	Limits       AppLimits
}

func sandboxAndExec(sandboxSpec, []string, []string) error {
	return errors.New("the Landlock sandbox is only available on Linux")
}

func landlockAvailable() (int, error) {
	return 0, errors.New("landlock is only available on Linux")
}

func seccompSupported() bool { return false }
