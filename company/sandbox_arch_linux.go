// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux

package company

import "runtime"

// AUDIT_ARCH_* values from linux/audit.h. The seccomp filter checks these so
// a 64-bit process cannot slip past every rule by issuing 32-bit syscalls,
// whose numbers mean entirely different things.
const (
	auditArchX86_64  = 0xc000003e
	auditArchAArch64 = 0xc00000b7
)

// seccompAuditArch reports the architecture this binary was built for, or 0
// where seccompSupported is false — a filter is never installed in that case.
func seccompAuditArch() uint32 {
	switch runtime.GOARCH {
	case "amd64":
		return auditArchX86_64
	case "arm64":
		return auditArchAArch64
	default:
		return 0
	}
}

// seccompSupported reports whether a filter can be built for this
// architecture. The platform targets x86-64 and arm64 servers; anywhere else
// the sandbox refuses to start rather than pretending, because an app that
// runs with a filter that was never installed is the one outcome this design
// must not produce.
func seccompSupported() bool { return seccompAuditArch() != 0 }
