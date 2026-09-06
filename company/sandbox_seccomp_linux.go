// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux

package company

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seccomp covers the three things Landlock leaves open on this kernel.
//
//  1. Landlock's network support (ABI 4) is TCP-only. UDP and raw sockets
//     are untouched by it, so an app with "no network" could still resolve
//     names and exfiltrate over UDP. Blocking socket(AF_INET/AF_INET6/…)
//     outright closes every address family at once.
//  2. ptrace and process_vm_readv let a same-UID process read Gitea's
//     memory — including its session keys. There is no PID namespace here to
//     make Gitea invisible, so the syscalls have to go instead.
//  3. kill(-1, …) reaches every process this user owns, Gitea included. One
//     line of Python would take the whole platform down.
//
// seccomp can only test scalar arguments — it never dereferences a pointer —
// which is why it can filter a socket's *address family* but never a
// destination address. Address-level policy is the egress broker's job
// (docs/company/app-platform.md), not this filter's.

// BPF instruction encodings, from linux/filter.h. Spelled out rather than
// pulled from a library so this file has no dependency beyond x/sys.
const (
	bpfLD  = 0x00
	bpfJMP = 0x05
	bpfRET = 0x06

	bpfW   = 0x00
	bpfABS = 0x20

	bpfJEQ = 0x10
	bpfJGT = 0x20
	bpfK   = 0x00
)

// Offsets into struct seccomp_data.
const (
	seccompOffsetNR   = 0
	seccompOffsetArch = 4
	seccompOffsetArgs = 16
)

// argLow is the offset of an argument's low 32 bits. Arguments are 64-bit
// and BPF loads 32 bits at a time; on the little-endian machines this runs
// on, the low half comes first.
func argLow(n int) uint32  { return uint32(seccompOffsetArgs + n*8) }
func argHigh(n int) uint32 { return uint32(seccompOffsetArgs + n*8 + 4) }

func stmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

func jump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k, Jt: jt, Jf: jf}
}

// Return actions. ERRNO is used rather than KILL for the denials an app
// might hit legitimately: a Python program that gets EACCES from socket()
// raises a catchable exception naming the call, while a killed process
// leaves a department staring at an app that vanished with no message.
const (
	seccompDenyNetwork = unix.SECCOMP_RET_ERRNO | uint32(unix.EACCES)
	seccompDenyPerm    = unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
)

// buildSeccompFilter assembles the program.
//
// Structured as a chain of "does nr match? if so decide, else fall through",
// ending in ALLOW. A default-deny filter would be stronger, but it would
// mean enumerating every syscall CPython and every wheel might ever make,
// and getting that list wrong takes down a department's app for reasons
// nobody can diagnose. The threat here is a deliberately hostile app, and
// against that the filesystem and network are the controls that matter —
// this list closes the specific holes Landlock cannot.
func buildSeccompFilter(spec sandboxSpec) []unix.SockFilter {
	var filter []unix.SockFilter

	// Refuse to run under an unexpected architecture. Without this check a
	// 64-bit process could issue 32-bit syscalls, whose numbers mean
	// completely different things, and walk straight past every rule below.
	filter = append(filter,
		stmt(bpfLD|bpfW|bpfABS, seccompOffsetArch),
		jump(bpfJMP|bpfJEQ|bpfK, seccompAuditArch(), 1, 0),
		stmt(bpfRET|bpfK, unix.SECCOMP_RET_KILL_PROCESS),
	)

	// Load the syscall number once; every rule below re-loads it only when
	// it has inspected an argument in between.
	filter = append(filter, stmt(bpfLD|bpfW|bpfABS, seccompOffsetNR))

	// EPERM for all of these: every one is something no ordinary application
	// does, so there is no legitimate caller to give a gentler answer to.
	deny := func(nr uintptr) {
		filter = append(filter,
			jump(bpfJMP|bpfJEQ|bpfK, uint32(nr), 0, 1),
			stmt(bpfRET|bpfK, seccompDenyPerm),
		)
	}
	deny(unix.SYS_PTRACE)
	deny(unix.SYS_PROCESS_VM_READV)
	deny(unix.SYS_PROCESS_VM_WRITEV)

	// The other ways to signal a process. Refused outright rather than
	// filtered by pid, because no ordinary Python application uses them and
	// leaving them open would make the kill rules below decorative.
	deny(unix.SYS_PIDFD_OPEN)
	deny(unix.SYS_PIDFD_SEND_SIGNAL)
	deny(unix.SYS_RT_SIGQUEUEINFO)
	deny(unix.SYS_RT_TGSIGQUEUEINFO)

	if !spec.AllowNetwork {
		filter = append(filter, socketFamilyRules()...)
	}
	filter = append(filter, killRules(spec.GiteaPID)...)

	return append(filter, stmt(bpfRET|bpfK, unix.SECCOMP_RET_ALLOW))
}

// socketFamilyRules denies every network address family, leaving AF_UNIX
// alone — the app's own socket, the one Gitea proxies to, is a unix socket,
// so this is what makes "reachable but cannot reach out" true.
func socketFamilyRules() []unix.SockFilter {
	families := []uint32{unix.AF_INET, unix.AF_INET6, unix.AF_PACKET, unix.AF_NETLINK}

	// nr != socket → skip the whole block.
	rules := []unix.SockFilter{
		jump(bpfJMP|bpfJEQ|bpfK, uint32(unix.SYS_SOCKET), 0, uint8(2*len(families)+2)),
		stmt(bpfLD|bpfW|bpfABS, argLow(0)),
	}
	for _, family := range families {
		rules = append(rules,
			jump(bpfJMP|bpfJEQ|bpfK, family, 0, 1),
			stmt(bpfRET|bpfK, seccompDenyNetwork),
		)
	}
	// Restore the syscall number for the rules that follow.
	rules = append(rules, stmt(bpfLD|bpfW|bpfABS, seccompOffsetNR))
	return rules
}

// killRules stop an app from signalling Gitea.
//
// Only two cases are refused: the exact "everything I own" broadcast, and
// Gitea's own pid. Signalling other pids stays allowed because an app
// managing its own worker processes is normal, and blocking that would break
// working apps to close a hole the attacker can barely reach anyway — with
// /proc denied by Landlock, an app cannot discover another process's pid to
// aim at.
func killRules(giteaPID int) []unix.SockFilter {
	var rules []unix.SockFilter
	for _, nr := range []uintptr{unix.SYS_KILL, unix.SYS_TGKILL} {
		block := []unix.SockFilter{
			stmt(bpfLD|bpfW|bpfABS, argHigh(0)),
			// A negative pid arrives as a sign-extended 64-bit value, so a
			// non-zero high word means "process group" or "everything".
			jump(bpfJMP|bpfJGT|bpfK, 0, 0, 1),
			stmt(bpfRET|bpfK, seccompDenyPerm),
			stmt(bpfLD|bpfW|bpfABS, argLow(0)),
			jump(bpfJMP|bpfJEQ|bpfK, uint32(giteaPID), 0, 1), //nolint:gosec // a pid always fits in 32 bits
			stmt(bpfRET|bpfK, seccompDenyPerm),
			stmt(bpfLD|bpfW|bpfABS, seccompOffsetNR),
		}
		rules = append(rules,
			jump(bpfJMP|bpfJEQ|bpfK, uint32(nr), 0, uint8(len(block))),
		)
		rules = append(rules, block...)
	}
	return rules
}

// applySeccomp installs the filter on the calling thread.
func applySeccomp(spec sandboxSpec) error {
	filter := buildSeccompFilter(spec)
	prog := unix.SockFprog{
		Len:    uint16(len(filter)), //nolint:gosec // the filter is a fixed, small program
		Filter: &filter[0],
	}
	// no_new_privs is a precondition for an unprivileged seccomp filter.
	// applyLandlock already sets it; repeating it costs nothing and keeps
	// this function correct on its own.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP,
		unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return fmt.Errorf("installing the seccomp filter: %w", errno)
	}
	// The program is referenced by the kernel only for the duration of the
	// call, but keeping it alive until after the syscall returns stops the
	// collector from moving it out from under an in-flight pointer.
	runtime.KeepAlive(filter)
	return nil
}
