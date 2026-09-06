// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// Gitea owns department app processes directly: there is no root on the
// deploy server, so no systemd units, no per-app OS users, no sudo. An app
// is a child process of this one. See docs/company/app-platform.md.
//
// Everything that changes an app's lifecycle goes through appSupervisor's
// mutex — deploys, start/stop/restart, the watchdog, and startup
// reconciliation alike. Separate paths would race in ways that are very
// hard to reproduce: a restart landing between "current swapped" and
// "health checked", a watchdog kill arriving while an admin's stop is
// already tearing the process down.

// errNoRelease means start was pressed before any deploy ever succeeded —
// a different situation from an app that is broken, and the only one the
// department fixes by deploying rather than by editing code.
var errNoRelease = audienceError(
	"아직 실행할 수 있는 버전이 없습니다. 오른쪽 위 [Deploy Request] 에서 배포를 요청하면, "+
		"관리자 승인 뒤 앱이 자동으로 시작됩니다.",
	// The admin does not press Deploy Request — the department does, and the
	// admin approves it. Telling an operator to do the requester's job is
	// worse than telling them nothing.
	"빌드된 릴리스가 없어 시작할 수 없습니다. 이 부서의 배포 요청을 승인하면 자동으로 빌드·기동됩니다. "+
		"직전 배포가 실패했다면 아래 원문과 로그를 확인해 주세요.")

const (
	// stopGracePeriod is how long a process gets to finish in-flight
	// requests after SIGTERM before SIGKILL. uvicorn drains on TERM; the
	// point of the wait is that a deploy or restart doesn't cut off someone
	// mid-request.
	stopGracePeriod = 10 * time.Second

	// crashRestartLimit is how many times a crashing app is restarted before
	// giving up. Restarting forever would hide a broken deploy behind an app
	// that is technically "up" every few seconds, which is worse than it
	// being visibly down.
	crashRestartLimit = 3
	crashRestartDelay = 5 * time.Second

	// maxUnixSocketPath is the smaller of the two platform limits for
	// sun_path — 104 on macOS and the BSDs, 108 on Linux — so a path that
	// fits here fits everywhere this runs.
	maxUnixSocketPath = 104
)

// appPaths is the on-disk layout for one app. Everything lives under
// AppDataPath because that is the one directory this process is guaranteed
// to be able to write without root.
type appPaths struct {
	home     string // .../company-apps/<hash>
	releases string
	current  string // symlink — the atomic switch point
	previous string // symlink — the rollback target
	run      string // the only writable dir bind-mounted into the sandbox
	socket   string
	logs     string
}

func appPathsFor(owner, repo string) appPaths {
	home := filepath.Join(setting.AppDataPath, "company-apps", appKey(owner, repo))
	return appPaths{
		home:     home,
		releases: filepath.Join(home, "releases"),
		current:  filepath.Join(home, "current"),
		previous: filepath.Join(home, "previous"),
		run:      filepath.Join(home, "run"),
		socket:   filepath.Join(home, "run", "app.sock"),
		logs:     filepath.Join(home, "logs"),
	}
}

func releaseDir(p appPaths, sha string) string {
	// The SHA comes from git, but it reaches here through a state file and
	// an HTTP handler, so it is hashed rather than trusted as a path
	// component — a "../.." in this position would otherwise escape the app
	// home. Hashing removes the question entirely.
	sum := sha256.Sum256([]byte(sha))
	return filepath.Join(p.releases, hex.EncodeToString(sum[:16]))
}

// appSupervisor owns one app's process. Created lazily and kept for the
// lifetime of the instance — there is one per department app, so the map
// never grows meaningfully.
type appSupervisor struct {
	owner, repo string
	paths       appPaths

	// mu guards everything below and is held across whole lifecycle
	// transitions, not just field writes. That is the point: a stop must not
	// interleave with a deploy's symlink swap.
	// deployMu serializes whole deploys for this app. Separate from mu
	// because a deploy holds it for minutes (pip install) while mu must stay
	// available for the start/stop steps the deploy itself performs.
	deployMu sync.Mutex

	mu       sync.Mutex
	cmd      *exec.Cmd
	stopping bool // set while an intentional stop is in progress, so the
	// process-exit watcher doesn't treat it as a crash
	crashes int
	envVer  int64 // the env version this process was started with
}

var (
	supervisorsMu sync.Mutex
	supervisors   = map[string]*appSupervisor{}
)

// supervisorFor returns the supervisor for one app, creating it on first
// use. Only the map access is globally locked; per-app work happens under
// that app's own mutex, so one slow deploy can't block every other app.
func supervisorFor(owner, repo string) *appSupervisor {
	key := appKey(owner, repo)
	supervisorsMu.Lock()
	defer supervisorsMu.Unlock()
	s, ok := supervisors[key]
	if !ok {
		s = &appSupervisor{owner: owner, repo: repo, paths: appPathsFor(owner, repo)}
		supervisors[key] = s
	}
	return s
}

// lookupSupervisor returns an app's supervisor only if one exists. Unlike
// supervisorFor it never creates one, so a request for an invented app name
// cannot grow the map — that path is reachable from a URL.
func lookupSupervisor(owner, repo string) *appSupervisor {
	supervisorsMu.Lock()
	defer supervisorsMu.Unlock()
	return supervisors[appKey(owner, repo)]
}

// pid returns the running process id, or 0.
func (s *appSupervisor) pid() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// IsAppRunning reports whether the app currently has a live process. Used by
// the proxy to answer "app not running" without touching state files.
func IsAppRunning(owner, repo string) bool {
	s := lookupSupervisor(owner, repo)
	return s != nil && s.pid() != 0
}

// AppSocketPath is where the proxy connects. Exposed so the proxy doesn't
// have to know the directory layout.
func AppSocketPath(owner, repo string) string { return appPathsFor(owner, repo).socket }

// buildEnv constructs the child's environment explicitly.
//
// This does NOT inherit Gitea's environment, and that is the whole point:
// exec.Cmd with a nil Env passes the parent's environment through, so a
// Gitea started with database credentials or SECRET_KEY in its environment
// would hand them to every department app. The sandbox blocks the files;
// this blocks the other half. See docs/company/app-platform-impl.md §5.
func buildEnv(p appPaths, rootPath string, appEnv map[string]string) []string {
	home, tmp := appHomeAndTmp(p)
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		// Private, per-app, and inside the one directory the app may write.
		// Never /tmp: without a mount namespace that is shared with every
		// other app on the host.
		"HOME=" + home,
		"TMPDIR=" + tmp,
		"LANG=C.UTF-8",
		"PYTHONDONTWRITEBYTECODE=1", // the release tree is read-only in the sandbox
		"PYTHONUNBUFFERED=1",        // otherwise logs arrive in 4KB bursts, or not at all on a crash
		"SOCKET=" + appSocketForProcess(p),
		"ROOT_PATH=" + rootPath,
	}
	for k, v := range appEnv {
		// Names are validated on the way in (see envstore.go); this is a
		// second, cheap guard so a value that somehow got stored can't
		// redefine what the platform just set.
		if isReservedEnvName(k) {
			continue
		}
		env = append(env, k+"="+v)
	}
	return env
}

// startLocked launches the app. Caller holds s.mu.
func (s *appSupervisor) startLocked(settings AppSettings, appEnv map[string]string, envVer int64) error {
	if s.cmd != nil && s.cmd.Process != nil {
		return nil // already running
	}
	target, err := os.Readlink(s.paths.current)
	if err != nil {
		// Deliberately not wrapped: the underlying error carries an absolute
		// path inside Gitea's data directory, and this message reaches a
		// department's screen.
		return errNoRelease
	}
	if _, err := os.Stat(filepath.Join(target, "app", "main.py")); err != nil {
		// The app contract is a fixed convention, not a config file, so a
		// missing main.py is the one thing we can and must check up front —
		// otherwise the failure surfaces as an opaque uvicorn traceback.
		return userErrorf("저장소 최상위에 main.py 가 없습니다")
	}

	if err := os.MkdirAll(s.paths.run, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(s.paths.logs, 0o700); err != nil {
		return err
	}
	// HOME and TMPDIR point here; pip, matplotlib and plenty of libraries
	// write caches on import and fail confusingly if the directory is absent.
	for _, dir := range []string{filepath.Join(s.paths.run, "home"), filepath.Join(s.paths.run, "tmp")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	// Checked here rather than left to the app: uvicorn reports this as
	// "OSError: AF_UNIX path too long" from deep inside asyncio, which says
	// nothing about which path or what the limit is.
	if len(s.paths.socket) >= maxUnixSocketPath {
		return audienceError(
			"서버 설정 문제로 앱을 시작할 수 없습니다. 관리자에게 알려 주세요.",
			fmt.Sprintf("소켓 경로가 %d바이트로 커널 한도(%d)를 넘습니다: %s — APP_DATA_PATH 를 더 짧은 경로로 옮겨야 합니다",
				len(s.paths.socket), maxUnixSocketPath, s.paths.socket))
	}

	// A socket left behind by a process that died without cleaning up makes
	// bind() fail with "address already in use", so it has to go. But
	// "nothing of ours is running" is not the same as "nothing is running":
	// a hard kill of Gitea leaves the app orphaned and still serving, and
	// deleting the socket underneath it would take a working app off the air
	// without anything saying so, then start a second copy beside it.
	//
	// So the socket is asked first. Something answering means the old process
	// is alive, and refusing is the only safe move — an operator can see that
	// and kill it, which is recoverable, while two live copies writing to one
	// app's files is not.
	if socketInUse(s.paths.socket) {
		return audienceError(
			"이 앱이 이미 실행 중입니다. 잠시 뒤 다시 시도해 주세요.",
			"소켓이 이미 응답합니다 — 이전 프로세스가 살아 있습니다. Gitea 가 비정상 종료된 뒤라면 그 프로세스를 종료해야 합니다: "+s.paths.socket)
	}
	if err := os.Remove(s.paths.socket); err != nil && !os.IsNotExist(err) {
		log.Warn("company: %s/%s: could not remove stale socket: %v", s.owner, s.repo, err)
	}

	rootPath := "/apps/" + s.owner + "/" + s.repo
	cmd, err := buildAppCommand(target, s.paths, settings, rootPath)
	if err != nil {
		return err
	}
	// A virtualenv is not relocatable: its console scripts name the
	// interpreter by absolute path, so a moved one execs into nothing and
	// reports "no such file or directory" about a file that plainly exists.
	// Saying so here beats letting that reach anybody.
	if _, statErr := os.Stat(cmd.Path); statErr != nil {
		return audienceError(
			"앱 실행 환경이 손상되었습니다. 다시 배포하면 복구됩니다.",
			"실행 파일이 없습니다 ("+cmd.Path+") — venv 가 옮겨졌거나 지워졌습니다. [다시 배포]로 재생성하세요")
	}
	cmd.Env = buildEnv(s.paths, rootPath, appEnv)
	cmd.Dir = filepath.Join(target, "app")
	// Its own process group so a stop reaches everything the app spawned,
	// not just the process we launched. (Inside a sandbox with a PID
	// namespace this is belt-and-braces — killing the namespace's PID 1
	// takes the rest with it — but the dev path has no namespace.)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	logFile, err := openAppLog(s.paths.logs)
	if err != nil {
		return err
	}
	// Through a writer rather than straight to the file, so every line is
	// stamped with when it arrived (company/applogtime.go).
	stamped := newTimestampWriter(logFile)
	cmd.Stdout = stamped
	cmd.Stderr = stamped

	if err := cmd.Start(); err != nil {
		_ = stamped.Close()
		return err
	}
	s.cmd = cmd
	s.stopping = false
	s.envVer = envVer
	// Best-effort, and after start because both need a live pid: keep the app
	// behind Gitea for CPU, and ahead of it for the OOM killer.
	deprioritizeAndProtect(cmd.Process.Pid)

	go s.watchExit(cmd, stamped)
	return nil
}

// watchExit reaps the child and decides whether its death was expected.
//
// Deliberately not holding s.mu while waiting — Wait blocks for the life of
// the process, and holding the lock would deadlock every other operation on
// this app.
func (s *appSupervisor) watchExit(cmd *exec.Cmd, logFile io.Closer) {
	err := cmd.Wait()
	// Closing the stamping writer flushes a trailing partial line, which on a
	// crash is usually the last thing the app managed to say.
	_ = logFile.Close()

	s.mu.Lock()
	if s.cmd != cmd {
		s.mu.Unlock()
		return // superseded by a newer process; this exit is not about us
	}
	s.cmd = nil
	intentional := s.stopping
	s.crashes++
	crashes := s.crashes
	s.mu.Unlock()

	if intentional {
		s.mu.Lock()
		s.crashes = 0
		s.mu.Unlock()
		return
	}

	log.Warn("company: %s/%s exited unexpectedly (attempt %d): %v", s.owner, s.repo, crashes, err)
	if crashes >= crashRestartLimit {
		// Giving up is the honest outcome: an app that dies every few
		// seconds is not "up", and restarting forever would keep the badge
		// flickering green while nothing works.
		_ = MutateAppState(s.owner, s.repo, func(st *AppState) bool {
			st.Actual = AppStateFailed
			st.FailedAt = time.Now().Unix()
			st.Reason = ReasonCrashLoop
			st.Message = fmt.Sprintf("the app exited %d times in a row; automatic restart stopped", crashes)
			st.UserMessage = ""
			st.PID = 0
			st.AppendHistory(AppHistoryEntry{Status: AppStateFailed, Reason: ReasonCrashLoop})
			return true
		})
		return
	}

	time.Sleep(crashRestartDelay)
	if st := LoadAppState(s.owner, s.repo); st.Desired == AppStateRunning {
		if _, err := s.startProcess(false); err != nil {
			log.Error("company: %s/%s: restart after crash failed: %v", s.owner, s.repo, err)
		}
	}
}

// stopLocked terminates the process group: SIGTERM, then SIGKILL after the
// grace period. Caller holds s.mu.
func (s *appSupervisor) stopLocked() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	s.stopping = true
	pid := s.cmd.Process.Pid
	// Negative pid = the whole process group.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM) // group may not exist on some platforms
	}

	done := make(chan struct{})
	cmd := s.cmd
	go func() {
		for range int(stopGracePeriod / (200 * time.Millisecond)) {
			time.Sleep(200 * time.Millisecond)
			s.mu.Lock()
			gone := s.cmd != cmd || s.cmd == nil
			s.mu.Unlock()
			if gone {
				close(done)
				return
			}
		}
		close(done)
	}()
	// Release the lock while waiting so watchExit can take it to record the
	// exit; re-acquire before returning to the caller's critical section.
	s.mu.Unlock()
	<-done
	s.mu.Lock()

	if s.cmd == cmd && cmd.Process != nil {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			_ = cmd.Process.Kill()
		}
	}
}

// Start brings the app up and records it as running.
//
// A deploy uses startProcess instead: it must not let the state say "running"
// until the health check has passed, or a release that never answers would
// show the department a green badge for the whole check window and only then
// flip to failed.
func (s *appSupervisor) Start() error {
	pid, err := s.startProcess(true)
	if err != nil {
		return err
	}
	s.mu.Lock()
	envVer := s.envVer
	s.mu.Unlock()

	return MutateAppState(s.owner, s.repo, func(st *AppState) bool {
		st.Actual = AppStateRunning
		st.Desired = AppStateRunning
		// Also heals a state file written before this field existed.
		st.HasRelease = true
		st.PID = pid
		st.StartedAt = time.Now().Unix()
		st.EnvVersionRunning = envVer
		st.Reason, st.Message, st.UserMessage = "", "", ""
		st.FailedAt = 0
		return true
	})
}

// startProcess launches the app and returns its pid. It records failures —
// those are unambiguous — but leaves success for the caller to declare.
//
// freshAttempt distinguishes a start someone asked for from one the crash
// watcher is retrying. Resetting the counter on every start meant the
// watcher's own restart cleared the tally it had just incremented, so
// crashRestartLimit was never reached: an app that died immediately was
// respawned every five seconds forever while its state still read "running".
func (s *appSupervisor) startProcess(freshAttempt bool) (int, error) {
	settings := SettingsFor(s.owner, s.repo)
	if !settings.IsEnabled() {
		return 0, audienceError(
			"관리자가 이 앱을 비활성화했습니다",
			"apps.yml 에서 이 앱이 enabled: false 로 되어 있습니다")
	}
	appEnv, envVer, err := LoadAppEnv(s.owner, s.repo)
	if err != nil {
		// fail-closed: starting with missing secrets would produce a
		// confusing runtime error instead of a clear one here.
		_ = MutateAppState(s.owner, s.repo, func(st *AppState) bool {
			st.Actual = AppStateFailed
			st.FailedAt = time.Now().Unix()
			st.Reason = ReasonSecretError
			st.Message = "environment variables could not be decrypted: " + err.Error()
			st.UserMessage = "" // DepartmentCause has the sentence for this one
			return true
		})
		return 0, err
	}

	s.mu.Lock()
	if freshAttempt {
		s.crashes = 0
	}
	startErr := s.startLocked(settings, appEnv, envVer)
	pid := 0
	if s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}
	s.mu.Unlock()

	if startErr != nil {
		reason := ReasonContractViolation
		if errors.Is(startErr, errNoRelease) {
			reason = ReasonNoRelease
		}
		// Message keeps the real error for an admin; UserMessage takes it
		// only if it was written for a department in the first place.
		userMsg, _ := userMessage(startErr)
		_ = MutateAppState(s.owner, s.repo, func(st *AppState) bool {
			// A failed start must not erase why the last deploy failed.
			// "There is no release to run" is a consequence of that failure,
			// not a separate problem, and it is the one message that tells
			// nobody what to fix — so pressing start on an app whose build
			// broke would otherwise replace "openpyxl could not be
			// installed" with a sentence carrying no next step.
			if reason == ReasonNoRelease && st.Actual == AppStateFailed && st.Reason != "" {
				return false
			}
			st.Actual = AppStateFailed
			st.FailedAt = time.Now().Unix()
			st.Reason = reason
			// AdminError, not Error(): a userError's Error() is its *staff*
			// sentence, so this field was showing an administrator the
			// department's wording under a heading that said "raw".
			st.Message = AdminError(startErr)
			st.UserMessage = userMsg
			st.PID = 0
			return true
		})
		return 0, startErr
	}
	return pid, nil
}

// Stop takes the app down and records that this was deliberate, so startup
// reconciliation won't bring it back.
func (s *appSupervisor) Stop(actor, newActual, reason string) error {
	s.mu.Lock()
	s.stopLocked()
	s.mu.Unlock()

	return MutateAppState(s.owner, s.repo, func(st *AppState) bool {
		st.Desired = AppStateStopped
		st.Actual = newActual
		st.PID = 0
		st.Reason = reason
		st.Health = AppHealth{State: "unknown"}
		st.AppendHistory(AppHistoryEntry{Status: newActual, Actor: actor, Reason: reason})
		return true
	})
}

// Restart is stop-then-start under one lock acquisition each, so the app
// can't be observed half-transitioned by another control action.
func (s *appSupervisor) Restart() error {
	s.mu.Lock()
	s.stopLocked()
	s.mu.Unlock()
	return s.Start()
}

// StartApp / StopApp / RestartApp are the package-level entry points used by
// handlers. They deliberately go through CanTransition first: the
// stopped/suspended distinction is what makes an admin's protective stop
// mean anything (docs/company/app-platform.md).
func StartApp(owner, repo string) error {
	st := LoadAppState(owner, repo)
	if ok, why := st.CanTransition("start", false); !ok {
		return userErrorf("%s", why)
	}
	return supervisorFor(owner, repo).Start()
}

func StopApp(owner, repo, actor string, isAdmin bool) error {
	st := LoadAppState(owner, repo)
	if ok, why := st.CanTransition("stop", isAdmin); !ok {
		return userErrorf("%s", why)
	}
	return supervisorFor(owner, repo).Stop(actor, AppStateStopped, "")
}

func RestartApp(owner, repo string, isAdmin bool) error {
	st := LoadAppState(owner, repo)
	if ok, why := st.CanTransition("restart", isAdmin); !ok {
		return userErrorf("%s", why)
	}
	return supervisorFor(owner, repo).Restart()
}

// SuspendApp is the admin's protective stop. The department cannot undo it —
// that is the difference from Stop, and without it an admin who stops an app
// for eating the machine would simply be restarted by the department.
func SuspendApp(owner, repo, actor, reason string) error {
	st := LoadAppState(owner, repo)
	if ok, why := st.CanTransition("suspend", true); !ok {
		return userErrorf("%s", why)
	}
	if reason == "" {
		// The department only sees "an administrator stopped this app"; the
		// reason is the only thing that tells them what to do next.
		reason = "stopped by an administrator"
	}
	return supervisorFor(owner, repo).Stop(actor, AppStateSuspended, reason)
}

func ResumeApp(owner, repo, actor string) error {
	st := LoadAppState(owner, repo)
	if ok, why := st.CanTransition("resume", true); !ok {
		return userErrorf("%s", why)
	}
	if err := MutateAppState(owner, repo, func(s *AppState) bool {
		s.Actual = AppStateStopped
		s.Reason, s.Message = "", ""
		s.AppendHistory(AppHistoryEntry{Status: AppStateStopped, Actor: actor, Reason: "resumed"})
		return true
	}); err != nil {
		return err
	}
	return supervisorFor(owner, repo).Start()
}

// ReconcileApps brings running processes back in line with recorded intent
// after a Gitea restart. Called once at startup.
//
// It honours `desired`, not `actual`: an app a department deliberately
// switched off must stay off. Reviving everything that was running when
// Gitea last stopped would silently undo their decision.
func ReconcileApps() {
	for _, st := range ListAppStates() {
		owner, repo := st.Owner, st.Repo
		// The symlink decides what starts, so the record has to agree with it
		// before anything reads the record — a state left over from before a
		// rollback would otherwise name the failed commit forever.
		_ = MutateAppState(owner, repo, func(s *AppState) bool {
			before := s.SHA
			adoptCurrentReleaseSHA(s, owner, repo)
			return s.SHA != before
		})
		if st.Desired != AppStateRunning || st.Actual == AppStateSuspended {
			continue
		}
		go func() {
			if err := supervisorFor(owner, repo).Start(); err != nil {
				log.Error("company: reconcile %s/%s: %v", owner, repo, err)
			}
		}()
	}
}

// openAppLog opens the app's log file, rotating it first if it has grown
// past the size cap. Rotation is by rename so an open file handle in the
// previous process keeps writing to the rotated file rather than failing.
func openAppLog(dir string) (*os.File, error) {
	return openRotatingLog(dir, appLogName)
}

// appLogName and buildLogName are the two streams an app produces: what it
// printed while running, and what its deploys printed while building it.
const (
	appLogName   = "app.log"
	buildLogName = "build.log"
)

// openRotatingLog opens one of them, rotating first if it has grown past the
// size cap. Rotation is by rename so an open handle in another process keeps
// writing to the rotated file rather than failing.
func openRotatingLog(dir, name string) (*os.File, error) {
	const maxBytes = 10 << 20
	const keep = 5
	file := filepath.Join(dir, name)
	if fi, err := os.Stat(file); err == nil && fi.Size() > maxBytes {
		for i := keep - 1; i >= 1; i-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", file, i), fmt.Sprintf("%s.%d", file, i+1))
		}
		_ = os.Rename(file, file+".1")
	}
	return os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// isReservedEnvName rejects names that would either break the app or hand
// it a way out of the sandbox. LD_PRELOAD is the dangerous one: it injects
// an arbitrary shared library into the process.
func isReservedEnvName(name string) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "PYTHON") {
		return true
	}
	switch upper {
	case "PATH", "HOME", "LANG", "SOCKET", "ROOT_PATH", "IFS", "SHELL", "TMPDIR":
		return true
	}
	return false
}

// socketInUse reports whether something is listening on the app's socket.
//
// A connect, not a stat: the file existing proves nothing — it outlives the
// process that made it, which is the whole reason it has to be cleaned up.
// Only a connection that succeeds means a live listener.
func socketInUse(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, socketProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// socketProbeTimeout is short on purpose: this is on the start path, and a
// local unix socket either accepts immediately or is not there.
const socketProbeTimeout = 200 * time.Millisecond

// stableFor reports whether the process started for this deploy is still the
// one running after d.
//
// The health check proves an app answered three times a second apart. It does
// not prove the app that answered is still alive: one that crashes on its
// first real request, or a few seconds after boot, restarts fast enough to
// answer the probe every time. The badge then reads "running" over an app
// that is dying in a loop, and the release is activated.
//
// Compared by process identity rather than by a crash counter, because the
// counter is reset by an intentional stop and a supervisor that has since
// been handed a different process is exactly the case being caught.
func (s *appSupervisor) stableFor(pid int, d time.Duration) bool {
	time.Sleep(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil && s.cmd.Process != nil && s.cmd.Process.Pid == pid
}
