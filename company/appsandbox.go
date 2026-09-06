// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// Department apps run as the same OS user as Gitea — there is no root on
// the deploy server, so per-app system accounts are not an option. Without
// a sandbox an app could read data/sessions/ and log in as an admin, read
// gitea.db, and read every other department's repository. bubblewrap closes
// that, and needs no privileges of its own.
//
// See docs/company/app-platform.md for the threat analysis, and for why
// bubblewrap rather than Landlock (Landlock cannot restrict the network
// below kernel 6.7, and leaves same-UID /proc visible).

// Inside the sandbox the app's writable directory is always mounted at
// /run, so its socket is always at this fixed path regardless of where the
// release actually lives on the host.
const sandboxSocketPath = "/run/app.sock"

// sandboxProbe caches whether bwrap actually works here. Probed once: the
// answer cannot change while the process runs, and the check spawns a
// process, far too expensive to repeat on every app start.
var sandboxProbe = sync.OnceValues(func() (string, error) {
	path, err := lookupTool("BWRAP_PATH", "bwrap")
	if err != nil {
		return "", errors.New("bubblewrap (bwrap) was not found — install it, or set [company] BWRAP_PATH to where it lives")
	}
	// Presence isn't enough: several distributions ship bwrap but disable
	// unprivileged user namespaces, and then every sandboxed start fails at
	// runtime instead of here, where it can be reported clearly.
	out, err := exec.Command(path, "--unshare-all", "--ro-bind", "/usr", "/usr", "--", "/bin/true").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("bubblewrap cannot create a sandbox (unprivileged user namespaces may be disabled): %v: %s",
			err, strings.TrimSpace(string(out)))
	}
	return path, nil
})

// appSocketForProcess is the path the app itself must bind, which is not
// where Gitea connects: inside the sandbox the app's writable directory is
// mounted at /run, so the same socket has two names. Both buildAppCommand
// and buildEnv read it from here so the ${SOCKET} placeholder and the
// SOCKET variable can never disagree — if they did, the app would bind
// somewhere the proxy never looks and every request would 502.
func appSocketForProcess(p appPaths) string {
	if mode, _ := sandboxMode(); mode == SandboxBubblewrap {
		return sandboxSocketPath
	}
	// Landlock has no mount namespace, so paths inside the sandbox are the
	// real ones — the app binds exactly where Gitea connects.
	return p.socket
}

// appHomeAndTmp are the app's private writable directories.
//
// Both live under the app's run directory rather than pointing at /tmp,
// because in Landlock mode there is no mount namespace: /tmp is the host's,
// shared with every other app, and one department reading another's
// temporary files is precisely the leak this platform exists to prevent.
// Under bubblewrap the run directory is mounted at /run, so the same two
// directories have in-sandbox names.
func appHomeAndTmp(p appPaths) (home, tmp string) {
	if mode, _ := sandboxMode(); mode == SandboxBubblewrap {
		return "/run/home", "/run/tmp"
	}
	return filepath.Join(p.run, "home"), filepath.Join(p.run, "tmp")
}

// lookupTool resolves a helper binary, preferring an explicit path from
// app.ini over PATH.
//
// The explicit setting exists because this platform's whole premise is a
// deploy server where the operator cannot write outside their home
// directory. Where `sudo apt install` is unavailable, bwrap ends up
// somewhere like ~/opt/bin/bwrap, and relying on PATH would then mean the
// sandbox silently depends on how Gitea happened to be started — a service
// that comes up unsandboxed after a reboot because a shell profile did not
// run is exactly the kind of failure this design must not have.
func lookupTool(settingKey, name string) (string, error) {
	if configured := companySetting(settingKey); configured != "" {
		// Checked rather than trusted: a path that is set but wrong must fail
		// here, where the reason is reported, not at the first app start.
		if info, err := os.Stat(configured); err != nil {
			return "", fmt.Errorf("[company] %s points at %q, which cannot be used: %w", settingKey, configured, err)
		} else if info.IsDir() {
			return "", fmt.Errorf("[company] %s points at a directory, not the %s executable", settingKey, name)
		}
		return configured, nil
	}
	return exec.LookPath(name)
}

// prlimitProbe finds prlimit(1), which is how resource limits reach the
// child. Go's os/exec offers no hook between fork and exec, so setrlimit
// cannot be called in the child — and calling it in the parent would cap
// Gitea itself at a department app's limit. util-linux's prlimit does
// exactly this job, and is already present on any server that has bwrap.
var prlimitProbe = sync.OnceValues(func() (string, error) {
	return lookupTool("PRLIMIT_PATH", "prlimit")
})

// LimitsStatus reports whether resource limits can be enforced, for the
// admin page.
func LimitsStatus() (available bool, detail string) {
	path, err := prlimitProbe()
	if err != nil {
		return false, "prlimit (util-linux) was not found, so per-app resource limits are not enforced — install it, or set [company] PRLIMIT_PATH"
	}
	return true, path
}

// prlimitArgs turns an app's limits into a prlimit invocation.
//
// Unlike the sandbox this is fail-open: missing limits are a fairness
// problem, not an isolation one, and the memory case is still covered by the
// watchdog (appsample.go). Refusing to run every app because one utility is
// absent would be the worse failure. It is reported on the admin page so it
// is a known state rather than a silent one.
func prlimitArgs(limits AppLimits) []string {
	path, err := prlimitProbe()
	if err != nil {
		return nil
	}
	args := []string{path}
	if limits.Processes > 0 {
		args = append(args, "--nproc="+strconv.Itoa(limits.Processes)) // fork bombs
	}
	if limits.OpenFiles > 0 {
		args = append(args, "--nofile="+strconv.Itoa(limits.OpenFiles))
	}
	if limits.MemoryMB > 0 {
		// --data (RLIMIT_DATA), not --as (RLIMIT_AS): numpy and friends
		// reserve far more virtual address space than they ever touch, and a
		// tight RLIMIT_AS kills healthy apps in a way that reads as a random
		// crash. RLIMIT_DATA covers the heap, which is what actually grows.
		args = append(args, "--data="+strconv.Itoa(limits.MemoryMB<<20))
	}
	if len(args) == 1 {
		return nil // nothing to limit; don't add a process for nothing
	}
	return append(args, "--")
}

// landlockProbe reports whether the fallback sandbox can be used here.
// Probed once, like the bubblewrap one: the kernel's answer cannot change
// while this process runs.
var landlockProbe = sync.OnceValues(func() (int, error) {
	// Landlock first, so a non-Linux host reports "Linux only" rather than
	// blaming its architecture for a check that never applied to it.
	abi, err := landlockAvailable()
	if err != nil {
		return 0, err
	}
	if !seccompSupported() {
		return 0, fmt.Errorf("no seccomp filter is available for %s, so the app's network could not be closed off", runtime.GOARCH)
	}
	if abi < landlockABINetworkRequired {
		// Below ABI 4 Landlock cannot restrict TCP at all. seccomp still
		// blocks socket() by address family, which covers the same ground —
		// so this is a warning about defence in depth, not a refusal.
		log.Warn("company: landlock ABI %d has no network support; outbound TCP is blocked by seccomp alone", abi)
	}
	return abi, nil
})

// landlockABINetworkRequired is the ABI at which Landlock gained TCP
// restrictions (kernel 6.7).
const landlockABINetworkRequired = 4

// SandboxMode names how apps are isolated on this host.
type SandboxMode string

const (
	SandboxBubblewrap SandboxMode = "bubblewrap"
	SandboxLandlock   SandboxMode = "landlock"
	SandboxNone       SandboxMode = "none"
)

// sandboxMode picks the strongest isolation this host can actually provide.
//
// bubblewrap first because it is strictly stronger — a mount namespace makes
// Gitea's data *absent* rather than merely unreadable, and a PID namespace
// means killing one process reaps everything the app spawned. Landlock is
// the fallback for hosts that restrict unprivileged user namespaces, which
// Ubuntu 24.04 does by default.
func sandboxMode() (SandboxMode, string) {
	if path, err := sandboxProbe(); err == nil {
		return SandboxBubblewrap, path
	}
	if abi, err := landlockProbe(); err == nil {
		return SandboxLandlock, fmt.Sprintf("landlock ABI %d + seccomp", abi)
	}
	_, bwrapErr := sandboxProbe()
	_, landlockErr := landlockProbe()
	return SandboxNone, fmt.Sprintf("bubblewrap: %v; landlock: %v", bwrapErr, landlockErr)
}

// SandboxStatus reports whether apps can be isolated on this host, for the
// admin page. An instance running apps unsandboxed is something an admin
// needs to be told, not to discover later.
func SandboxStatus() (available bool, detail string) {
	mode, detail := sandboxMode()
	return mode != SandboxNone, string(mode) + ": " + detail
}

// allowUnsandboxed reports whether starting without isolation is permitted.
//
// Fail-closed by default: an app that cannot be isolated must not start,
// because "runs, but can read every secret on the box" is worse than "does
// not run". Two deliberate escapes:
//
//   - Non-Linux, i.e. development. bubblewrap is Linux-only, so this is the
//     difference between "you can work on this locally" and "you cannot".
//     A laptop does not hold the company's data.
//   - An explicit admin opt-in in app.ini, for a Linux host where
//     unprivileged user namespaces are unavailable and the operator has
//     knowingly accepted the risk.
func allowUnsandboxed() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	return companySetting("ALLOW_UNSANDBOXED_APPS") == "true"
}

// companySetting reads one [company] key, tolerating an unloaded config.
// Unit tests exercise this package without app.ini, and a nil provider must
// read as "unset" rather than panic.
func companySetting(key string) string {
	if setting.CfgProvider == nil {
		return ""
	}
	return strings.TrimSpace(setting.CfgProvider.Section("company").Key(key).String())
}

// sandboxReadOnlyBinds are host paths the interpreter needs to run at all.
// Bound read-only, and only if present: /lib and /bin are symlinks into
// /usr on merged-usr systems and real directories elsewhere, and bwrap
// refuses to start if asked to bind something that isn't there.
var sandboxReadOnlyBinds = []string{
	"/usr",
	"/lib", "/lib64", "/bin", "/sbin",
	"/etc/ssl", "/etc/ca-certificates", "/etc/resolv.conf",
	// CPython touches these during startup and in common library calls —
	// getpass.getuser() and pwd.getpwuid() read passwd/group through NSS,
	// datetime reads localtime. Leaving them out does not make the app
	// safer, it makes it fail to start with an error nobody can trace back
	// to a sandbox. None of them contain a secret: password hashes live in
	// /etc/shadow, which is deliberately absent.
	"/etc/passwd", "/etc/group", "/etc/nsswitch.conf", "/etc/localtime",
}

// buildAppCommand assembles the command that runs one release.
//
// On Linux this is bwrap wrapping the release's own interpreter; elsewhere
// (development) it is that interpreter directly. The caller sets Env — and
// deliberately NOT via bwrap's --setenv, which would put secrets into argv
// where `ps` shows them to anyone on the host. bwrap passes its own
// environment through to the child.
func buildAppCommand(release string, p appPaths, settings AppSettings, rootPath string) (*exec.Cmd, error) {
	mode, detail := sandboxMode()
	if mode == SandboxNone && !allowUnsandboxed() {
		// Surfaces to the admin as-is; the department sees the
		// "sandbox_unavailable" code translated into plain language.
		return nil, errors.New("this app cannot be isolated on this host, so it was not started — " + detail)
	}

	args := buildStartArgs(settings.Start, appSocketForProcess(p), rootPath)
	if len(args) == 0 {
		return nil, errors.New("the start command in apps.yml is empty")
	}
	interpreter := filepath.Join(release, ".venv", "bin", args[0])

	switch mode {
	case SandboxBubblewrap:
		bwrapPath, _ := sandboxProbe()
		return bwrapCommand(bwrapPath, release, p, settings, args), nil
	case SandboxLandlock:
		return landlockCommand(release, p, settings, interpreter, args)
	default:
		log.Warn("company: starting an app WITHOUT a sandbox: %s", detail)
		return exec.Command(interpreter, args[1:]...), nil //nolint:gosec // args come from admin-owned apps.yml
	}
}

// bwrapCommand builds the bubblewrap invocation.
func bwrapCommand(bwrapPath, release string, p appPaths, settings AppSettings, args []string) *exec.Cmd {
	bwrapArgs := []string{
		// A fresh namespace of every kind. --unshare-all includes the
		// network, which is what makes "requests.get() reaches nothing"
		// true — the app stays reachable anyway because a unix socket is a
		// filesystem object, not a network one, and p.run is bound below.
		"--unshare-all",
		"--die-with-parent", // no orphaned apps if Gitea goes away
		"--new-session",     // detach from any terminal (blocks TIOCSTI injection)
		"--proc", "/proc",   // private /proc: other processes are invisible
		"--dev", "/dev",
		// An unbounded tmpfs would be charged to host RAM, so one app could
		// push the machine into swap by writing to /tmp.
		"--size", strconv.Itoa(settings.Limits.TmpMB << 20), "--tmpfs", "/tmp",
	}
	for _, dir := range sandboxReadOnlyBinds {
		if _, err := os.Lstat(dir); err == nil {
			bwrapArgs = append(bwrapArgs, "--ro-bind", dir, dir)
		}
	}
	bwrapArgs = append(bwrapArgs,
		// The code and its dependencies are read-only: an app that cannot
		// rewrite its own release cannot persist a backdoor into it.
		"--ro-bind", filepath.Join(release, "app"), "/app",
		"--ro-bind", filepath.Join(release, ".venv"), "/venv",
		"--bind", p.run, "/run", // the socket, and the only writable path
		"--chdir", "/app",
		"--",
		filepath.Join("/venv", "bin", args[0]),
	)
	bwrapArgs = append(bwrapArgs, args[1:]...)

	// prlimit wraps bwrap rather than the other way round: rlimits are
	// inherited across exec, so setting them outside means everything inside
	// the sandbox — including anything the app spawns — is covered.
	argv := append(prlimitArgs(settings.Limits), append([]string{bwrapPath}, bwrapArgs...)...)
	return exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv[0] is resolved from PATH, the rest is admin-owned apps.yml
}

// landlockCommand builds the `gitea deptapp-exec` invocation for hosts where
// bubblewrap cannot run.
//
// The read-only list is the interesting part: it is everything the app is
// allowed to see, and Gitea's own data directory is simply not on it. Note
// what is *absent* — /proc, /tmp, and the home directory. Denying /proc is
// what replaces the PID namespace bubblewrap would have given: without it an
// app could read /proc/<pid>/environ and lift another process's secrets.
// Denying /tmp matters because, with no mount namespace, /tmp is shared with
// every other app on the host; each app gets a private directory under its
// own run directory instead, pointed at by HOME and TMPDIR.
func landlockCommand(release string, p appPaths, settings AppSettings, interpreter string, args []string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating the Gitea binary for the sandbox helper: %w", err)
	}

	argv := []string{self, "deptapp-exec"}
	for _, dir := range sandboxReadOnlyBinds {
		argv = append(argv, "--ro", dir)
	}
	argv = append(argv,
		"--ro", filepath.Join(release, "app"),
		"--ro", filepath.Join(release, ".venv"),
		"--ro", "/dev/urandom", "--ro", "/dev/null",
		// /proc as a whole stays denied — that is what replaces the PID
		// namespace bubblewrap would have given, and without it an app could
		// read another process's /proc/<pid>/environ. Its *own* entry is
		// allowed because parts of the standard library read it, and it
		// exposes nothing the app does not already have. This resolves to
		// /proc/<pid> of the helper, which is still the app's own pid after
		// execve.
		"--ro", "/proc/self",
		"--rw", p.run,
		"--gitea-pid", strconv.Itoa(os.Getpid()),
		"--memory-mb", strconv.Itoa(settings.Limits.MemoryMB),
		"--processes", strconv.Itoa(settings.Limits.Processes),
		"--open-files", strconv.Itoa(settings.Limits.OpenFiles),
	)
	if settings.Network.Mode != NetworkNone {
		argv = append(argv, "--allow-network")
	}
	argv = append(argv, "--", interpreter)
	argv = append(argv, args[1:]...)

	return exec.Command(argv[0], argv[1:]...), nil //nolint:gosec // argv[0] is this binary; the rest is admin-owned apps.yml
}

// buildStartArgs expands the ${SOCKET} and ${ROOT_PATH} placeholders in the
// configured start command and splits it into argv.
//
// Split on whitespace rather than run through a shell: there is no shell in
// the sandbox, and going through one would turn the start string into an
// injection surface for anything interpolated into it.
func buildStartArgs(start, socket, rootPath string) []string {
	return strings.Fields(strings.NewReplacer(
		"${SOCKET}", socket,
		"$SOCKET", socket,
		"${ROOT_PATH}", rootPath,
		"$ROOT_PATH", rootPath,
	).Replace(start))
}
