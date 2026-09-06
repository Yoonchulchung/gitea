// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// runFilter interprets the BPF program the way the kernel does, so the jump
// offsets can be checked. An off-by-one in a seccomp filter does not fail
// loudly — it silently allows what it was written to deny — which is exactly
// why this is worth simulating rather than eyeballing.
func runFilter(t *testing.T, filter []unix.SockFilter, data seccompData) uint32 {
	t.Helper()
	var acc uint32
	for pc := 0; pc < len(filter); pc++ {
		ins := filter[pc]
		switch {
		case ins.Code == bpfLD|bpfW|bpfABS:
			acc = data.word(t, ins.K)
		case ins.Code == bpfRET|bpfK:
			return ins.K
		case ins.Code == bpfJMP|bpfJEQ|bpfK:
			if acc == ins.K {
				pc += int(ins.Jt)
			} else {
				pc += int(ins.Jf)
			}
		case ins.Code == bpfJMP|bpfJGT|bpfK:
			if acc > ins.K {
				pc += int(ins.Jt)
			} else {
				pc += int(ins.Jf)
			}
		default:
			t.Fatalf("unhandled BPF instruction at %d: %+v", pc, ins)
		}
	}
	t.Fatal("filter ran off the end without returning")
	return 0
}

// seccompData mirrors the struct the kernel hands the filter.
type seccompData struct {
	nr   uint32
	arch uint32
	args [6]uint64
}

func (d seccompData) word(t *testing.T, offset uint32) uint32 {
	t.Helper()
	switch {
	case offset == seccompOffsetNR:
		return d.nr
	case offset == seccompOffsetArch:
		return d.arch
	case offset >= seccompOffsetArgs:
		n := (offset - seccompOffsetArgs) / 8
		require.Less(t, n, uint32(6))
		if (offset-seccompOffsetArgs)%8 == 0 {
			return uint32(d.args[n])
		}
		return uint32(d.args[n] >> 32)
	}
	t.Fatalf("unexpected load offset %d", offset)
	return 0
}

func call(nr uintptr, args ...uint64) seccompData {
	d := seccompData{nr: uint32(nr), arch: seccompAuditArch()}
	copy(d.args[:], args)
	return d
}

func TestSeccompFilterDenies(t *testing.T) {
	const giteaPID = 4242
	filter := buildSeccompFilter(sandboxSpec{GiteaPID: giteaPID})

	t.Run("network address families", func(t *testing.T) {
		for _, family := range []uint64{unix.AF_INET, unix.AF_INET6, unix.AF_PACKET, unix.AF_NETLINK} {
			assert.Equal(t, seccompDenyNetwork,
				runFilter(t, filter, call(unix.SYS_SOCKET, family, unix.SOCK_STREAM, 0)),
				"family %d", family)
		}
		// AF_UNIX is what the app binds its own socket on — the one Gitea
		// proxies to. Blocking it would make the app unreachable.
		assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW),
			runFilter(t, filter, call(unix.SYS_SOCKET, unix.AF_UNIX, unix.SOCK_STREAM, 0)))
	})

	t.Run("reading another process", func(t *testing.T) {
		for _, nr := range []uintptr{unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV} {
			assert.Equal(t, seccompDenyPerm, runFilter(t, filter, call(nr)))
		}
	})

	t.Run("signalling Gitea", func(t *testing.T) {
		// kill(-1) reaches every process this user owns. One line of Python
		// would take the whole platform down with it.
		//
		// Both register forms, because they are not interchangeable and the
		// first version of this filter only handled the second one: pid_t is
		// 32 bits and x86-64 leaves the upper half undefined, so glibc's
		// zero-extended form is what actually reaches the kernel. Testing
		// only the sign-extended value passed while the real case went
		// straight through.
		assert.Equal(t, seccompDenyPerm,
			runFilter(t, filter, call(unix.SYS_KILL, 0x00000000ffffffff, uint64(unix.SIGKILL))),
			"zero-extended -1, which is what glibc actually produces")
		assert.Equal(t, seccompDenyPerm,
			runFilter(t, filter, call(unix.SYS_KILL, ^uint64(0), uint64(unix.SIGKILL))),
			"sign-extended -1")
		// A process group, e.g. killpg.
		assert.Equal(t, seccompDenyPerm,
			runFilter(t, filter, call(unix.SYS_KILL, 0x00000000fffffc19, uint64(unix.SIGTERM))),
			"negative pgid")
		assert.Equal(t, seccompDenyPerm,
			runFilter(t, filter, call(unix.SYS_KILL, giteaPID, uint64(unix.SIGTERM))))
		assert.Equal(t, seccompDenyPerm,
			runFilter(t, filter, call(unix.SYS_TGKILL, giteaPID, 1, uint64(unix.SIGTERM))))

		// The alternative routes to the same thing.
		for _, nr := range []uintptr{
			unix.SYS_PIDFD_OPEN, unix.SYS_PIDFD_SEND_SIGNAL,
			unix.SYS_RT_SIGQUEUEINFO, unix.SYS_RT_TGSIGQUEUEINFO,
		} {
			assert.Equal(t, seccompDenyPerm, runFilter(t, filter, call(nr)))
		}
	})

	t.Run("an app signalling its own workers still works", func(t *testing.T) {
		assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW),
			runFilter(t, filter, call(unix.SYS_KILL, 9999, uint64(unix.SIGTERM))))
		// raise() is tgkill against the caller's own thread group.
		assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW),
			runFilter(t, filter, call(unix.SYS_TGKILL, 9999, 9999, uint64(unix.SIGUSR1))))
	})

	t.Run("ordinary syscalls are untouched", func(t *testing.T) {
		// Default-allow is deliberate: enumerating every syscall CPython and
		// every wheel might make would break apps for undiagnosable reasons.
		for _, nr := range []uintptr{unix.SYS_READ, unix.SYS_WRITE, unix.SYS_OPENAT, unix.SYS_EXECVE, unix.SYS_CLONE} {
			assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW), runFilter(t, filter, call(nr)))
		}
	})

	t.Run("a foreign architecture is killed", func(t *testing.T) {
		// Without this a 64-bit process could issue 32-bit syscalls, whose
		// numbers mean different things, and walk past every rule above.
		foreign := call(unix.SYS_SOCKET, unix.AF_INET)
		foreign.arch = 0x40000003 // AUDIT_ARCH_I386
		assert.Equal(t, uint32(unix.SECCOMP_RET_KILL_PROCESS), runFilter(t, filter, foreign))
	})
}

// An app an admin granted outbound access must still be protected from
// reading Gitea's memory and killing it.
func TestSeccompFilterWithNetworkAllowed(t *testing.T) {
	filter := buildSeccompFilter(sandboxSpec{GiteaPID: 4242, AllowNetwork: true})

	assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW),
		runFilter(t, filter, call(unix.SYS_SOCKET, unix.AF_INET, unix.SOCK_STREAM, 0)))
	assert.Equal(t, seccompDenyPerm, runFilter(t, filter, call(unix.SYS_PTRACE)))
	assert.Equal(t, seccompDenyPerm, runFilter(t, filter, call(unix.SYS_KILL, 4242, uint64(unix.SIGKILL))))
}

func TestHandledRightsTrimsToABI(t *testing.T) {
	// Asking to handle a right the running kernel does not know about makes
	// landlock_create_ruleset fail outright, so the mask has to be trimmed.
	fs1, net1 := handledRights(1)
	assert.Zero(t, fs1&unix.LANDLOCK_ACCESS_FS_TRUNCATE, "TRUNCATE arrived in ABI 3")
	assert.Zero(t, fs1&unix.LANDLOCK_ACCESS_FS_REFER, "REFER arrived in ABI 2")
	assert.Zero(t, net1, "network restrictions arrived in ABI 4")

	fs4, net4 := handledRights(4)
	assert.NotZero(t, fs4&unix.LANDLOCK_ACCESS_FS_TRUNCATE)
	assert.NotZero(t, fs4&unix.LANDLOCK_ACCESS_FS_REFER)
	assert.Equal(t, uint64(unix.LANDLOCK_ACCESS_NET_BIND_TCP|unix.LANDLOCK_ACCESS_NET_CONNECT_TCP), net4)
	assert.Zero(t, fs4&unix.LANDLOCK_ACCESS_FS_IOCTL_DEV, "IOCTL_DEV arrived in ABI 5")

	fs5, _ := handledRights(5)
	assert.NotZero(t, fs5&unix.LANDLOCK_ACCESS_FS_IOCTL_DEV)
}
