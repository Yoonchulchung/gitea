// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !linux && !darwin

package company

func hostMemory() (hostMem, bool) { return hostMem{}, false }
