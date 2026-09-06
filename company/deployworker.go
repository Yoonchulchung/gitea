// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/process"
	"gitea.dev/modules/util"
)

// Building a release means running `pip install`, which takes minutes for
// anything with numpy in it. That must never happen on the goroutine
// handling an HTTP request, so the merge hook only enqueues and returns; the
// work happens here. See docs/company/app-platform-impl.md §3.

const (
	// deployQueueCapacity bounds the backlog. Rejecting a deploy with a clear
	// reason is better than accumulating work nobody is waiting for any more
	// and eventually running out of memory.
	deployQueueCapacity = 64
	deployWorkers       = 2

	// maxSourceBytes caps the department's own files. The venv is not capped —
	// a disk quota needs root, and refusing to install pandas because its
	// wheels are large would be wrong. Disk usage is reported to admins
	// instead (docs/company/app-platform.md).
	maxSourceBytes = 64 << 20

	installTimeout     = 15 * time.Minute
	healthCheckTries   = 30
	healthCheckOK      = 3 // consecutive successes, so an app that answers once and dies fails
	healthCheckSpacing = time.Second
)

type deployJob struct {
	Owner, Repo string
	SHA         string
	PRID        int64
}

var deployQueue = make(chan deployJob, deployQueueCapacity)

// StartDeployWorkers launches the background workers. Called once at startup.
func StartDeployWorkers() {
	ctx := graceful.GetManager().ShutdownContext()
	for range deployWorkers {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-deployQueue:
					runDeploy(ctx, job)
				}
			}
		}()
	}
}

// QueueDeploy records the intent and hands the work to a worker.
//
// The state is written to `queued` here rather than in the worker so the
// badge flips to "deploying" the moment an admin merges, and so a job that
// is still `queued` minutes later is visibly stuck rather than invisible.
func QueueDeploy(owner, repo, sha string, prID int64) {
	if err := MutateAppState(owner, repo, func(st *AppState) bool {
		st.Owner, st.Repo = owner, repo
		st.Actual = AppStateQueued
		st.SHA, st.PRID = sha, prID
		st.Reason, st.Message = "", ""
		st.AppendHistory(AppHistoryEntry{Status: AppStateQueued, SHA: sha})
		return true
	}); err != nil {
		log.Error("company: %s/%s: recording queued state: %v", owner, repo, err)
	}

	enqueueDeploy(owner, repo, sha, prID)
}

// enqueueDeploy hands the job to a worker without touching state, for callers
// that have already recorded `queued` themselves with more context than this
// function has — company/deploy_notifier.go records who approved it.
func enqueueDeploy(owner, repo, sha string, prID int64) {
	select {
	case deployQueue <- deployJob{Owner: owner, Repo: repo, SHA: sha, PRID: prID}:
	default:
		// Never block the caller: this runs inside a merge notification, and
		// a full queue must not hold up the merge.
		failDeploy(owner, repo, ReasonDeployQueueFull,
			"the deploy queue is full; the previously deployed version is still running")
	}
}

// failDeploy records a deployment failure. It never touches the running
// process: a build that fails has not replaced anything, so the previous
// version keeps serving. That is the whole reason the swap happens last.
//
// message is admin detail and may contain anything — a build log, a
// filesystem path. userMessage is the half a department may see, and is
// empty unless there is something specific worth telling them beyond the
// sentence DepartmentCause already has for the reason code.
func failDeploy(owner, repo, reason, message string, userMessage ...string) {
	log.Error("company: deploy %s/%s failed (%s): %s", owner, repo, reason, message)
	safe := ""
	if len(userMessage) > 0 {
		safe = userMessage[0]
	}
	if err := MutateAppState(owner, repo, func(st *AppState) bool {
		st.Actual = AppStateFailed
		st.FailedAt = time.Now().Unix()
		st.Reason = reason
		st.Message = message
		st.UserMessage = safe
		st.AppendHistory(AppHistoryEntry{Status: AppStateFailed, Reason: reason})
		return true
	}); err != nil {
		log.Error("company: %s/%s: recording failure: %v", owner, repo, err)
	}
}

// appendBuildLog records one deploy's output so it survives the next one.
//
// Before this the only copy lived in AppState.Message, which the following
// deploy overwrote — so the log of the failure someone is trying to
// understand was gone by the time they went looking, and comparing "it broke
// the same way last time" against anything was impossible.
//
// Best-effort: a deploy must not fail because its own log could not be
// written.
func appendBuildLog(p appPaths, sha, outcome, output string) {
	if err := os.MkdirAll(p.logs, 0o700); err != nil {
		log.Error("company: build log directory: %v", err)
		return
	}
	f, err := openRotatingLog(p.logs, buildLogName)
	if err != nil {
		log.Error("company: opening the build log: %v", err)
		return
	}
	defer func() { _ = f.Close() }()

	header := fmt.Sprintf("\n===== %s  %s  %s =====\n",
		time.Now().Format(time.RFC3339), outcome, util.TruncateRunes(sha, 12))
	if _, err := f.WriteString(header + strings.TrimRight(output, "\n") + "\n"); err != nil {
		log.Error("company: writing the build log: %v", err)
	}
}

func runDeploy(ctx context.Context, job deployJob) {
	owner, repo := job.Owner, job.Repo
	settings := SettingsFor(owner, repo)
	if !settings.IsEnabled() {
		failDeploy(owner, repo, ReasonContractViolation, "this app is disabled by an administrator")
		return
	}

	// One deploy per app at a time. Two concurrent builds of the same app
	// would race on the release directory and the `current` symlink, and the
	// loser could leave `current` pointing at a half-built tree.
	s := supervisorFor(owner, repo)
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	_ = MutateAppState(owner, repo, func(st *AppState) bool {
		st.Actual = AppStateBuilding
		return true
	})

	p := appPathsFor(owner, repo)
	release := releaseDir(p, job.SHA)
	if err := buildRelease(ctx, job, p, release, settings); err != nil {
		appendBuildLog(p, job.SHA, "FAILED", err.Error())
		if denied, ok := errors.AsType[*packagesDeniedError](err); ok {
			// Written by us and naming the packages, so it is the one thing
			// the department needs in order to fix this.
			failDeploy(owner, repo, ReasonPackageDenied, denied.Error(), denied.Error())
			return
		}
		if errors.Is(err, errNoPython) {
			failDeploy(owner, repo, ReasonNoPython, AdminError(err), DepartmentSafeError("build", err))
			return
		}
		failDeploy(owner, repo, ReasonInstallFailed, err.Error())
		return
	}
	appendBuildLog(p, job.SHA, "OK", "build succeeded")

	if err := activateRelease(owner, repo, p, release, job.SHA, settings); err != nil {
		log.Error("company: %s/%s: activation failed: %v", owner, repo, err)
		return // activateRelease has already recorded the outcome
	}

	// Only after a successful activation, and still under deployMu: a failed
	// deploy leaves the previous release serving, and that is the worst
	// possible moment to be deleting anything.
	gcReleases(owner, repo)
}

// buildRelease materializes the code and its dependencies. It writes only
// inside the release directory and never touches `current`, so a failure at
// any point here leaves the running app completely untouched.
func buildRelease(ctx context.Context, job deployJob, p appPaths, release string, settings AppSettings) error {
	appDir := filepath.Join(release, "app")
	if err := os.RemoveAll(release); err != nil {
		return err
	}
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		return err
	}

	if err := extractAppSource(ctx, job, appDir); err != nil {
		return err
	}

	reqPath := filepath.Join(appDir, "requirements.txt")
	body, err := os.ReadFile(reqPath)
	if errors.Is(err, os.ErrNotExist) {
		// No requirements.txt is the normal case for an app that only uses
		// what the platform provides.
		return buildVenv(ctx, p, release, nil, settings)
	}
	if err != nil {
		return err
	}
	reqs, reqErrs := ParseRequirements(string(body))
	if len(reqErrs) > 0 {
		return &packagesDeniedError{message: formatRequirementErrors(reqErrs)}
	}
	if denied := DeniedPackages(reqs, settings.AllowedPackages()); len(denied) > 0 {
		return &packagesDeniedError{message: formatDeniedPackages(denied)}
	}
	return buildVenv(ctx, p, release, reqs, settings)
}

// extractAppSource copies the department's files out of the central deploy
// repo at this commit into the release tree.
func extractAppSource(ctx context.Context, job deployJob, appDir string) error {
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return err
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		return err
	}
	gitRepo, err := git.OpenRepository(ctx, central)
	if err != nil {
		return err
	}
	defer gitRepo.Close()

	commit, err := gitRepo.GetCommit(ctx, job.SHA)
	if err != nil {
		return fmt.Errorf("the deployed commit %s could not be read: %w", job.SHA, err)
	}
	tree, err := commit.SubTree(ctx, gitRepo, deployPathPrefix(job.Owner, job.Repo))
	if err != nil {
		return fmt.Errorf("no files were found for this app in the deploy repository: %w", err)
	}
	entries, err := tree.ListEntriesRecursiveFast(ctx, gitRepo)
	if err != nil {
		return err
	}

	var total int64
	for _, entry := range entries {
		if entry.IsDir() || entry.IsSubModule() {
			continue
		}
		dest, err := safeJoin(appDir, entry.Name())
		if err != nil {
			// A tree entry is not user input in the usual sense, but it does
			// reach here through data staff control, so the path is checked
			// rather than trusted.
			return err
		}
		blob := entry.Blob(gitRepo)
		total += blob.Size(ctx)
		if total > maxSourceBytes {
			return fmt.Errorf("the app's files exceed the %d MB limit", maxSourceBytes>>20)
		}
		// GetBlobBytes treats a non-positive limit as "read nothing".
		content, err := blob.GetBlobBytes(ctx, blob.Size(ctx))
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		// Never executable: the app is started by the platform, and a file
		// the department can mark +x is a way to get something other than
		// uvicorn running.
		if err := os.WriteFile(dest, content, 0o600); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(appDir, "main.py")); err != nil {
		return errors.New("main.py was not found — the app must have a main.py exposing `app` in its repository root")
	}
	return nil
}

// safeJoin joins base and rel, refusing anything that escapes base.
func safeJoin(base, rel string) (string, error) {
	joined := filepath.Join(base, filepath.Clean("/"+rel))
	if joined != base && !strings.HasPrefix(joined, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("the path %q is not allowed", rel)
	}
	return joined, nil
}

// buildVenv creates (or reuses) the virtualenv and links it into the release.
//
// The venv is keyed by the exact requirement set rather than by commit, so
// the common case — a code change with unchanged dependencies — skips pip
// entirely and deploys in seconds instead of minutes.
func buildVenv(ctx context.Context, p appPaths, release string, reqs []Requirement, settings AppSettings) error {
	python, err := pythonPath()
	if err != nil {
		// Checked before anything else: without an interpreter nothing here
		// can work, and the failure must not arrive dressed as a dependency
		// problem a department would try to fix in requirements.txt.
		return err
	}

	key := requirementsKey(reqs, settings.BasePackages)
	venv := filepath.Join(p.home, "venvs", key)
	link := filepath.Join(release, ".venv")

	if _, err := os.Stat(filepath.Join(venv, "bin", "python")); err != nil {
		if err := os.RemoveAll(venv); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(venv), 0o700); err != nil {
			return err
		}
		buildCtx, cancel := context.WithTimeout(ctx, installTimeout)
		defer cancel()

		if out, err := runBuildCmd(buildCtx, python, "-m", "venv", venv); err != nil {
			_ = os.RemoveAll(venv)
			return fmt.Errorf("creating the Python environment failed: %w\n%s", err, out)
		}
		if err := installIntoVenv(buildCtx, venv, reqs, settings); err != nil {
			// A half-installed venv would be reused by the next deploy and
			// fail in a way that looks unrelated to this one.
			_ = os.RemoveAll(venv)
			return err
		}
	}

	// A symlink rather than a copy: a venv's scripts contain absolute paths,
	// so copying one produces an environment that silently uses the wrong
	// interpreter. bwrap resolves the symlink when it binds it.
	_ = os.Remove(link)
	return os.Symlink(venv, link)
}

// installIntoVenv installs the platform's own stack, then the department's
// additions, and checks only the difference against the allowlist.
//
// Two phases rather than one because of what the check means. The allowlist
// answers "may this department use this package", and fastapi's own
// dependency tree — starlette, anyio, typing-extensions and a dozen more —
// was never a department's choice. Installing the base first and treating
// whatever it drags in as the baseline keeps the check about the thing it is
// actually policing.
func installIntoVenv(ctx context.Context, venv string, reqs []Requirement, settings AppSettings) error {
	if len(settings.BasePackages) > 0 {
		if err := pipInstall(ctx, venv, settings.BasePackages); err != nil {
			// The department did not ask for these and cannot fix them.
			return audienceError(
				"서버가 기본 제공하는 패키지를 설치하지 못했습니다. 관리자에게 알려 주세요.",
				"기본 패키지 설치 실패 — 관리자 화면에서 목록을 확인하세요: "+err.Error())
		}
	}
	baseline, err := installedPackages(ctx, venv)
	if err != nil {
		return err
	}
	if len(reqs) == 0 {
		return nil
	}

	raw := make([]string, 0, len(reqs))
	for _, r := range reqs {
		raw = append(raw, r.Raw)
	}
	if err := pipInstall(ctx, venv, raw); err != nil {
		return err
	}

	// pip resolves transitive dependencies, which the requirements file never
	// listed and nobody approved. Rather than force every app to pin its
	// whole tree (which non-developers cannot do), the resolution is allowed
	// to run — nothing executed, wheels only — and then the *result* is
	// checked. Anything unapproved fails the deploy before it is ever
	// imported.
	extra, err := unapprovedInstalled(ctx, venv, settings, baseline)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		return &packagesDeniedError{message: "이 패키지들이 의존성으로 필요한데 아직 승인되지 않았습니다: " +
			strings.Join(extra, ", ")}
	}
	return nil
}

// pipInstall runs one install step.
//
// --only-binary=:all: means wheels only, so no setup.py runs during install.
// Package code executes later, when the app imports it — by then it is inside
// the sandbox. See docs/company/app-platform.md.
func pipInstall(ctx context.Context, venv string, packages []string) error {
	args := append([]string{"install", "--only-binary=:all:", "--no-input", "--disable-pip-version-check"}, packages...)
	if out, err := runBuildCmd(ctx, filepath.Join(venv, "bin", "pip"), args...); err != nil {
		return fmt.Errorf("installing dependencies failed: %w\n%s", err, lastLines(out, 30))
	}
	return nil
}

// installedPackages lists what is in the environment now, normalised.
func installedPackages(ctx context.Context, venv string) (map[string]bool, error) {
	out, err := runBuildCmd(ctx, filepath.Join(venv, "bin", "pip"), "list", "--format=json")
	if err != nil {
		return nil, fmt.Errorf("listing installed packages failed: %w", err)
	}
	var installed []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &installed); err != nil {
		return nil, fmt.Errorf("listing installed packages produced unreadable output: %w", err)
	}
	names := make(map[string]bool, len(installed))
	for _, pkg := range installed {
		names[normalizePackageName(pkg.Name)] = true
	}
	return names, nil
}

// pipBaseline are the packages every venv has by construction.
var pipBaseline = []string{"pip", "setuptools", "wheel", "pkg-resources"}

func unapprovedInstalled(ctx context.Context, venv string, settings AppSettings, baseline map[string]bool) ([]string, error) {
	out, err := runBuildCmd(ctx, filepath.Join(venv, "bin", "pip"), "list", "--format=json")
	if err != nil {
		return nil, fmt.Errorf("listing installed packages failed: %w", err)
	}
	var installed []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &installed); err != nil {
		return nil, fmt.Errorf("listing installed packages produced unreadable output: %w", err)
	}

	allowed := map[string]bool{}
	for _, name := range append(settings.AllowedPackages(), pipBaseline...) {
		allowed[normalizePackageName(name)] = true
	}
	var extra []string
	for _, pkg := range installed {
		name := normalizePackageName(pkg.Name)
		// Anything the platform's own stack brought in is not the
		// department's choice and is not theirs to have approved.
		if baseline[name] || allowed[name] {
			continue
		}
		extra = append(extra, pkg.Name)
	}
	slices.Sort(extra)
	return extra, nil
}

// activateRelease swaps the app onto the new release and verifies it, rolling
// back to the previous one if it does not come up.
func activateRelease(owner, repo string, p appPaths, release, sha string, settings AppSettings) error {
	s := supervisorFor(owner, repo)

	previous, _ := os.Readlink(p.current)

	_ = MutateAppState(owner, repo, func(st *AppState) bool {
		st.Actual = AppStateActivating
		return true
	})

	s.mu.Lock()
	s.stopLocked()
	s.mu.Unlock()

	if err := swapSymlink(p.current, release); err != nil {
		failDeploy(owner, repo, ReasonContractViolation, "switching to the new version failed: "+err.Error())
		return err
	}
	if previous != "" {
		_ = swapSymlink(p.previous, previous)
	}

	// startProcess, not Start: the state must not say "running" until the
	// health check passes, or a release that never answers shows a green
	// badge for the whole check window before flipping to failed.
	pid, startErr := s.startProcess()
	if startErr == nil {
		if err := waitHealthy(p.socket, settings); err == nil {
			return MutateAppState(owner, repo, func(st *AppState) bool {
				st.Actual = AppStateRunning
				st.Desired = AppStateRunning
				st.HasRelease = true
				st.PID = pid
				st.StartedAt = time.Now().Unix()
				st.SHA = sha
				st.Reason, st.Message, st.UserMessage = "", "", ""
				st.FailedAt = 0
				st.Health = AppHealth{State: "up", CheckedAt: time.Now().Unix()}
				st.AppendHistory(AppHistoryEntry{Status: AppStateRunning, SHA: sha})
				return true
			})
		}
	}

	// The new release is not serving. Put the old one back — nobody should be
	// left with a broken app because someone pushed a typo.
	if previous == "" {
		failDeploy(owner, repo, ReasonHealthTimeout,
			"the app did not respond after starting, and there is no previous version to fall back to")
		return errors.New("health check failed with no rollback target")
	}
	log.Warn("company: %s/%s: new release unhealthy, rolling back to %s", owner, repo, previous)

	s.mu.Lock()
	s.stopLocked()
	s.mu.Unlock()
	if err := swapSymlink(p.current, previous); err != nil {
		failDeploy(owner, repo, ReasonHealthTimeout, "rolling back failed: "+err.Error())
		return err
	}
	if err := s.Start(); err != nil {
		failDeploy(owner, repo, ReasonRolledBack, "the previous version could not be restarted either")
		return err
	}
	if err := waitHealthy(p.socket, settings); err != nil {
		// "rolled back" and "currently down" are different things and an admin
		// has to be able to tell them apart.
		failDeploy(owner, repo, ReasonHealthTimeout, "the app is down: the previous version did not respond either")
		return err
	}
	return MutateAppState(owner, repo, func(st *AppState) bool {
		st.Actual = AppStateRunning
		st.Desired = AppStateRunning
		st.HasRelease = true
		st.Reason = ReasonRolledBack
		st.Message = "the new version did not respond after starting, so the previous version was restored"
		st.Health = AppHealth{State: "up", CheckedAt: time.Now().Unix()}
		st.AppendHistory(AppHistoryEntry{Status: AppStateRunning, Reason: ReasonRolledBack})
		return true
	})
}

// swapSymlink points link at target atomically. A plain remove-then-create
// leaves a window where `current` does not exist at all.
func swapSymlink(link, target string) error {
	tmp := link + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// waitHealthy polls the app's socket until it answers consistently.
//
// The contract cannot require a /health route — departments write plain
// FastAPI apps — so a non-5xx response to the configured path (or "/") counts
// as healthy. Consecutive successes are required because an app that starts,
// answers once, and dies would otherwise pass.
func waitHealthy(socket string, settings AppSettings) error {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}
	path := settings.HealthPath
	if path == "" {
		path = "/"
	}

	streak := 0
	var lastErr error
	for range healthCheckTries {
		time.Sleep(healthCheckSpacing)
		resp, err := client.Get("http://app" + path) //nolint:noctx // the client carries a timeout
		if err != nil {
			lastErr = err
			streak = 0
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("the app answered with HTTP %d", resp.StatusCode)
			streak = 0
			continue
		}
		if streak++; streak >= healthCheckOK {
			return nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("the app did not respond")
	}
	return lastErr
}

// runBuildCmd runs a build step with an explicit, minimal environment. The
// build runs outside the sandbox — pip needs the network — so it must not
// inherit Gitea's environment either.
func runBuildCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := process.CommandContext(ctx, name, args...) //nolint:gosec // name is a fixed interpreter/pip path, args are validated requirements
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + os.TempDir(),
		"LANG=C.UTF-8",
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// packagesDeniedError separates "you asked for something unapproved" from
// "the install broke", because the department sees a different message and a
// different button for each.
type packagesDeniedError struct{ message string }

func (e *packagesDeniedError) Error() string { return e.message }

func formatRequirementErrors(errs []RequirementError) string {
	lines := make([]string, 0, len(errs))
	for _, e := range errs {
		lines = append(lines, fmt.Sprintf("requirements.txt line %d: %s", e.Line, e.Reason))
	}
	return strings.Join(lines, "\n")
}

func formatDeniedPackages(denied []Requirement) string {
	names := make([]string, 0, len(denied))
	for _, r := range denied {
		names = append(names, r.Name)
	}
	return "these packages are not approved yet: " + strings.Join(names, ", ")
}

// requirementsKey identifies an exact dependency set, so an unchanged one
// reuses its venv.
func requirementsKey(reqs []Requirement, base []string) string {
	raw := make([]string, 0, len(reqs)+len(base))
	for _, r := range reqs {
		raw = append(raw, r.Name+"=="+r.Version)
	}
	// The base list is part of the environment, so changing it has to build a
	// new one rather than reuse a venv assembled from the old stack.
	raw = append(raw, base...)
	slices.Sort(raw) // order in the file must not produce a different venv
	sum := sha256.Sum256([]byte(strings.Join(raw, "\n")))
	return hex.EncodeToString(sum[:16])
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
