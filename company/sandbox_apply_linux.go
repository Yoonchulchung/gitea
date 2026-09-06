// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux

package company

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The fallback sandbox, for hosts where bubblewrap cannot run.
//
// bubblewrap needs unprivileged user namespaces, and Ubuntu 24.04 restricts
// those by default (AppArmor's kernel.apparmor_restrict_unprivileged_userns).
// Asking an administrator to turn that off is asking them to weaken a
// distribution default, so this platform brings its own isolation instead:
// Landlock for the filesystem and the network, seccomp for the few things
// Landlock does not cover.
//
// Landlock and seccomp both need to be applied *in the child, after fork and
// before exec*, and Go's os/exec offers no hook there. That is the entire
// reason for the `gitea deptapp-exec` subcommand: Gitea runs it, it locks
// itself down, and then it execve()s the app. Both restrictions survive
// execve, so the app inherits a process it can never escape.
//
// Honest comparison with bubblewrap, because this is weaker in two ways:
//
//   - No mount namespace. Files are *denied* rather than absent, which is
//     the same outcome for an attacker but a different error message.
//   - No PID namespace, so killing one process does not reap the tree, and
//     other processes on the host remain visible in principle. /proc is
//     closed off below, which removes the visibility half; the reaping half
//     is handled by the process group the supervisor already sets up.
//
// See docs/company/app-platform.md.

// Landlock ABI levels, each adding rights that must be *handled* by the
// ruleset before they can be restricted. Asking to handle a right the
// running kernel does not know about makes the whole call fail, so the mask
// is trimmed to what this kernel reports.
const (
	landlockABIRefer    = 2 // LANDLOCK_ACCESS_FS_REFER
	landlockABITruncate = 3 // LANDLOCK_ACCESS_FS_TRUNCATE
	landlockABINetwork  = 4 // TCP bind/connect — kernel 6.7+
	landlockABIIoctlDev = 5 // LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// landlockReadRights is what a read-only path is granted. EXECUTE is
// included because the interpreter and every shared library it loads live
// on read-only paths.
const landlockReadRights = unix.LANDLOCK_ACCESS_FS_EXECUTE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_DIR

// landlockFileRights are the only rights Landlock accepts on a path that is
// not a directory. Handing a regular file a directory-only right — READ_DIR,
// MAKE_REG, REMOVE_FILE and the rest — makes landlock_add_rule return EINVAL
// and takes the whole sandbox down with it. /etc/resolv.conf and /dev/null
// are both on the allow list, so this is not a corner case.
const landlockFileRights = unix.LANDLOCK_ACCESS_FS_EXECUTE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE |
	unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_TRUNCATE |
	unix.LANDLOCK_ACCESS_FS_IOCTL_DEV

// landlockWriteRights is added for the one directory an app may write.
const landlockWriteRights = unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_TRUNCATE |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG |
	unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
	unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
	unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
	unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
	unix.LANDLOCK_ACCESS_FS_REMOVE_DIR

// landlockABI returns the ABI version this kernel supports, or an error if
// Landlock is unavailable at all.
func landlockABI() (int, error) {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, fmt.Errorf("landlock is not available on this kernel: %w", errno)
	}
	return int(abi), nil
}

// handledRights returns the access mask to restrict, trimmed to this ABI.
func handledRights(abi int) (fsRights, netRights uint64) {
	fsRights = landlockReadRights | landlockWriteRights
	if abi >= landlockABIRefer {
		fsRights |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi < landlockABITruncate {
		fsRights &^= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= landlockABIIoctlDev {
		// Without this an app can reconfigure a device it can open. It has
		// /dev/urandom and /dev/null and nothing else, so this is thin
		// defence in depth rather than a load-bearing control.
		fsRights |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	if abi >= landlockABINetwork {
		netRights = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	}
	return fsRights, netRights
}

// sandboxSpec is everything the helper needs to lock itself down.
type sandboxSpec struct {
	ReadOnly  []string
	ReadWrite []string
	// AllowNetwork leaves TCP alone. Only ever true for an app an admin has
	// explicitly granted outbound access.
	AllowNetwork bool
	GiteaPID     int
	Limits       AppLimits
}

// applyLandlock builds and enters the ruleset. After it returns, this thread
// and anything it execs can reach nothing outside the listed paths.
func applyLandlock(spec sandboxSpec) error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}
	fsRights, netRights := handledRights(abi)
	if spec.AllowNetwork {
		netRights = 0 // nothing to restrict, so nothing to handle
	}

	attr := unix.LandlockRulesetAttr{Access_fs: fsRights, Access_net: netRights}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	rulesetFD := int(fd)
	defer func() { _ = unix.Close(rulesetFD) }()

	add := func(path string, rights uint64) error {
		pathFD, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			// A path that is not there is not a failure: /lib64 exists on
			// some distributions and not others, and refusing to start over
			// a missing optional directory would be its own outage.
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("opening %s for the sandbox: %w", path, err)
		}
		defer func() { _ = unix.Close(pathFD) }()

		// Directory-only rights are rejected outright on a regular file, so
		// they have to be dropped rather than merely unused.
		var stat unix.Stat_t
		if err := unix.Fstat(pathFD, &stat); err != nil {
			return fmt.Errorf("inspecting %s for the sandbox: %w", path, err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			rights &= landlockFileRights
		}

		rule := unix.LandlockPathBeneathAttr{
			Allowed_access: rights & fsRights, // never ask for a right the ruleset does not handle
			Parent_fd:      int32(pathFD),
		}
		if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE,
			fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
			return fmt.Errorf("landlock_add_rule for %s: %w", path, errno)
		}
		return nil
	}

	for _, path := range spec.ReadOnly {
		if err := add(path, landlockReadRights); err != nil {
			return err
		}
	}
	for _, path := range spec.ReadWrite {
		if err := add(path, landlockReadRights|landlockWriteRights); err != nil {
			return err
		}
	}

	// no_new_privs is required by landlock_restrict_self, and is what stops
	// the app gaining privileges through a setuid binary it can execute.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

// applyRlimits caps the app's resources.
//
// Called here rather than through prlimit(1) because this process *is* the
// child: setrlimit applies to the caller, which is exactly what is wanted,
// and it removes a dependency on a utility that may not be installed.
func applyRlimits(limits AppLimits) error {
	set := func(resource int, value uint64, name string) error {
		if value == 0 {
			return nil
		}
		if err := unix.Setrlimit(resource, &unix.Rlimit{Cur: value, Max: value}); err != nil {
			return fmt.Errorf("setting the %s limit: %w", name, err)
		}
		return nil
	}
	if err := set(unix.RLIMIT_NPROC, uint64(limits.Processes), "process"); err != nil {
		return err
	}
	if err := set(unix.RLIMIT_NOFILE, uint64(limits.OpenFiles), "open file"); err != nil {
		return err
	}
	// RLIMIT_DATA, not RLIMIT_AS: numpy and friends reserve far more virtual
	// address space than they ever touch, and a tight RLIMIT_AS kills healthy
	// apps in a way that reads as a random crash.
	return set(unix.RLIMIT_DATA, uint64(limits.MemoryMB)<<20, "memory")
}

// EnterSandbox applies every restriction to the calling thread, in the order
// they must happen. The caller must hold the OS thread and execve from it.
func EnterSandbox(spec sandboxSpec) error {
	// rlimits first: they are process-wide and unaffected by what follows,
	// and doing them last would mean setrlimit itself had to be permitted by
	// the seccomp filter.
	if err := applyRlimits(spec.Limits); err != nil {
		return err
	}
	if err := applyLandlock(spec); err != nil {
		return err
	}
	// seccomp last, because it is the one that would block the others.
	return applySeccomp(spec)
}

// landlockAvailable reports the kernel's Landlock ABI, for the startup probe.
func landlockAvailable() (int, error) { return landlockABI() }

// sandboxAndExec is the tail of the helper: lock the thread, lock down, then
// become the app.
func sandboxAndExec(spec sandboxSpec, argv, env []string) error {
	// seccomp filters and Landlock domains are per-thread. execve keeps the
	// calling thread's and discards every other thread, so the same thread
	// has to do both — hence the lock, which is never released because this
	// process stops existing at Exec.
	runtime.LockOSThread()

	if err := EnterSandbox(spec); err != nil {
		return err
	}
	return unix.Exec(argv[0], argv, env)
}
