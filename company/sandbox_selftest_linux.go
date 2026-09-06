// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux

package company

import (
	"errors"
	"fmt"
	"io"

	"golang.org/x/sys/unix"
)

// `gitea deptapp-exec --self-test` checks the sandbox's own logic on the host
// that will run it.
//
// This lives in the shipped binary rather than only in `go test` because the
// deploy server has neither a Go toolchain nor a way to copy a test binary
// onto it — and because checking the binary that will actually run the apps
// is stronger evidence than checking a separately built one.
//
// It complements the end-to-end probe (docs/company/sandboxcheck/) rather
// than repeating it. The probe can only exercise an app with no network at
// all; these cases cover the branches it cannot reach — the filter an admin
// gets after granting outbound access, and the architecture check that stops
// a 64-bit process from issuing 32-bit syscalls to walk past every rule.

// seccompData mirrors the struct the kernel hands a filter.
type seccompData struct {
	nr   uint32
	arch uint32
	args [6]uint64
}

func (d seccompData) word(offset uint32) (uint32, error) {
	switch {
	case offset == seccompOffsetNR:
		return d.nr, nil
	case offset == seccompOffsetArch:
		return d.arch, nil
	case offset >= seccompOffsetArgs:
		n := (offset - seccompOffsetArgs) / 8
		if n >= 6 {
			return 0, fmt.Errorf("argument index %d is out of range", n)
		}
		if (offset-seccompOffsetArgs)%8 == 0 {
			return uint32(d.args[n]), nil
		}
		return uint32(d.args[n] >> 32), nil
	}
	return 0, fmt.Errorf("unexpected load offset %d", offset)
}

// evalSeccompFilter interprets a filter the way the kernel does.
//
// Simulating it is the only practical way to check the jump offsets: a wrong
// offset does not fail loudly, it silently allows what the rule was written
// to deny, and installing the filter for real would only ever reveal the
// cases that happen to be exercised.
func evalSeccompFilter(filter []unix.SockFilter, data seccompData) (uint32, error) {
	var acc uint32
	for pc := 0; pc < len(filter); pc++ {
		ins := filter[pc]
		switch ins.Code {
		case bpfLD | bpfW | bpfABS:
			word, err := data.word(ins.K)
			if err != nil {
				return 0, err
			}
			acc = word
		case bpfRET | bpfK:
			return ins.K, nil
		case bpfJMP | bpfJEQ | bpfK:
			pc += int(jumpOffset(acc == ins.K, ins))
		case bpfJMP | bpfJGT | bpfK:
			pc += int(jumpOffset(acc > ins.K, ins))
		default:
			return 0, fmt.Errorf("unhandled BPF instruction at %d: %+v", pc, ins)
		}
	}
	return 0, errors.New("the filter ran off the end without returning")
}

func jumpOffset(taken bool, ins unix.SockFilter) uint8 {
	if taken {
		return ins.Jt
	}
	return ins.Jf
}

// selfTestCase is one syscall the filter has to answer in a particular way.
type selfTestCase struct {
	name string
	spec sandboxSpec
	data seccompData
	want uint32
	why  string
}

func syscallData(nr uintptr, args ...uint64) seccompData {
	d := seccompData{nr: uint32(nr), arch: seccompAuditArch()}
	copy(d.args[:], args)
	return d
}

// selfTestCases is everything checked, in the order it is reported.
func selfTestCases(giteaPID int) []struct {
	section string
	cases   []selfTestCase
} {
	blocked := sandboxSpec{GiteaPID: giteaPID}
	granted := sandboxSpec{GiteaPID: giteaPID, AllowNetwork: true}

	return []struct {
		section string
		cases   []selfTestCase
	}{
		{"network: none — the ordinary app", []selfTestCase{
			{"socket(AF_INET) refused", blocked, syscallData(unix.SYS_SOCKET, unix.AF_INET), seccompDenyNetwork, "outbound TCP would be open"},
			{"socket(AF_INET6) refused", blocked, syscallData(unix.SYS_SOCKET, unix.AF_INET6), seccompDenyNetwork, "outbound TCP would be open"},
			{"socket(AF_PACKET) refused", blocked, syscallData(unix.SYS_SOCKET, unix.AF_PACKET), seccompDenyNetwork, "raw sockets would be open"},
			{"socket(AF_NETLINK) refused", blocked, syscallData(unix.SYS_SOCKET, unix.AF_NETLINK), seccompDenyNetwork, "host network config would be readable"},
			{"socket(AF_UNIX) allowed", blocked, syscallData(unix.SYS_SOCKET, unix.AF_UNIX), unix.SECCOMP_RET_ALLOW, "the app could not bind its own socket and would be unreachable"},
		}},

		// The branch the end-to-end probe cannot reach. If granting an app
		// outbound access also unblocked ptrace or kill, that grant would
		// double as permission to attack Gitea.
		{"network granted — an admin-approved exception", []selfTestCase{
			{"socket(AF_INET) allowed", granted, syscallData(unix.SYS_SOCKET, unix.AF_INET), unix.SECCOMP_RET_ALLOW, "the grant would have no effect"},
			{"ptrace still refused", granted, syscallData(unix.SYS_PTRACE), seccompDenyPerm, "granting network would also grant reading Gitea's memory"},
			{"process_vm_readv still refused", granted, syscallData(unix.SYS_PROCESS_VM_READV), seccompDenyPerm, "granting network would also grant reading Gitea's memory"},
			{"kill(gitea) still refused", granted, syscallData(unix.SYS_KILL, uint64(giteaPID)), seccompDenyPerm, "granting network would also grant killing Gitea"}, //nolint:gosec // a pid fits in 64 bits
			{"kill(-1) still refused", granted, syscallData(unix.SYS_KILL, 0xffffffff), seccompDenyPerm, "granting network would also grant killing everything"},
		}},

		{"signalling", []selfTestCase{
			// Both register forms: pid_t is 32 bits and x86-64 leaves the
			// upper half undefined, so glibc's zero-extended -1 is what
			// actually reaches the kernel. A filter checking only the
			// sign-extended form lets the real case straight through — which
			// is exactly what happened before this case existed.
			{"kill(-1) zero-extended refused", blocked, syscallData(unix.SYS_KILL, 0x00000000ffffffff), seccompDenyPerm, "one line of Python takes the platform down"},
			{"kill(-1) sign-extended refused", blocked, syscallData(unix.SYS_KILL, ^uint64(0)), seccompDenyPerm, "one line of Python takes the platform down"},
			{"kill(-pgid) refused", blocked, syscallData(unix.SYS_KILL, 0x00000000fffffc19), seccompDenyPerm, "a process group reaches beyond the app"},
			{"kill(gitea) refused", blocked, syscallData(unix.SYS_KILL, uint64(giteaPID)), seccompDenyPerm, "the app could stop Gitea"},                  //nolint:gosec // a pid fits in 64 bits
			{"tgkill(gitea) refused", blocked, syscallData(unix.SYS_TGKILL, uint64(giteaPID), 1), seccompDenyPerm, "the app could stop Gitea by thread"}, //nolint:gosec // a pid fits in 64 bits
			{"pidfd_send_signal refused", blocked, syscallData(unix.SYS_PIDFD_SEND_SIGNAL), seccompDenyPerm, "the pid checks above would be bypassable"},
			{"rt_sigqueueinfo refused", blocked, syscallData(unix.SYS_RT_SIGQUEUEINFO), seccompDenyPerm, "the pid checks above would be bypassable"},
			{"kill(own child) allowed", blocked, syscallData(unix.SYS_KILL, 9999), unix.SECCOMP_RET_ALLOW, "an app could not manage its own workers"},
		}},

		{"ordinary syscalls are untouched", []selfTestCase{
			{"read", blocked, syscallData(unix.SYS_READ), unix.SECCOMP_RET_ALLOW, "the app would not run at all"},
			{"write", blocked, syscallData(unix.SYS_WRITE), unix.SECCOMP_RET_ALLOW, "the app would not run at all"},
			{"openat", blocked, syscallData(unix.SYS_OPENAT), unix.SECCOMP_RET_ALLOW, "the app would not run at all"},
			{"clone", blocked, syscallData(unix.SYS_CLONE), unix.SECCOMP_RET_ALLOW, "threads would be impossible"},
			{"execve", blocked, syscallData(unix.SYS_EXECVE), unix.SECCOMP_RET_ALLOW, "subprocesses stay allowed — they inherit the same box"},
		}},

		{"architecture", []selfTestCase{
			// Without this a 64-bit process could issue 32-bit syscalls,
			// whose numbers mean entirely different things, and walk past
			// every rule above.
			{"foreign architecture killed", blocked, foreignArchData(), unix.SECCOMP_RET_KILL_PROCESS, "32-bit syscall numbers would bypass every rule"},
		}},
	}
}

func foreignArchData() seccompData {
	d := syscallData(unix.SYS_SOCKET, unix.AF_INET)
	d.arch = 0x40000003 // AUDIT_ARCH_I386
	return d
}

// RunSandboxSelfTest checks the filter and the Landlock ABI on this host,
// writing a report to out. Returns the number of failures.
func RunSandboxSelfTest(out io.Writer, giteaPID int) int {
	failures := 0

	fmt.Fprintf(out, "=== landlock ===\n")
	if abi, err := landlockABI(); err != nil {
		fmt.Fprintf(out, "  FAIL landlock is unavailable: %v\n", err)
		failures++
	} else {
		fs, net := handledRights(abi)
		fmt.Fprintf(out, "  ok   ABI %d (filesystem rights %#x, network rights %#x)\n", abi, fs, net)
		if net == 0 {
			fmt.Fprintf(out, "  note ABI below 4: outbound TCP is blocked by seccomp alone\n")
		}
	}
	if !seccompSupported() {
		fmt.Fprintf(out, "  FAIL no seccomp filter is available for this architecture\n")
		return failures + 1
	}

	for _, group := range selfTestCases(giteaPID) {
		fmt.Fprintf(out, "\n=== %s ===\n", group.section)
		for _, c := range group.cases {
			got, err := evalSeccompFilter(buildSeccompFilter(c.spec), c.data)
			switch {
			case err != nil:
				fmt.Fprintf(out, "  FAIL %-40s %v\n", c.name, err)
				failures++
			case got != c.want:
				fmt.Fprintf(out, "  FAIL %-40s got %#x, want %#x — %s\n", c.name, got, c.want, c.why)
				failures++
			default:
				fmt.Fprintf(out, "  ok   %-40s\n", c.name)
			}
		}
	}

	fmt.Fprintln(out)
	if failures > 0 {
		fmt.Fprintf(out, "FAILED %d check(s). The seccomp filter does not do what it claims;\n", failures)
		fmt.Fprintf(out, "do not run department apps on this host.\n")
	} else {
		fmt.Fprintf(out, "All checks passed.\n")
	}
	return failures
}
