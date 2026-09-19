// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
	"gitea.dev/modules/process"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/translation"
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
	venvCompleteMarker = ".complete" // written last; an environment without it is rebuilt

	healthCheckTries   = 30
	healthCheckOK      = 3 // consecutive successes, so an app that answers once and dies fails
	healthCheckSpacing = time.Second
	healthCheckTimeout = 3 * time.Second

	// healthSettleDelay is how long the new process has to stay the same one
	// after passing its health check. Long enough to catch a crash loop
	// whose restarts are fast, short enough not to lengthen every deploy.
	healthSettleDelay = 10 * time.Second
)

type deployJob struct {
	Owner, Repo string
	SHA         string
	PRID        int64
	// Prior is what the app was doing when this deploy was queued. Read from
	// the state file inside the worker it was always "queued" — the queueing
	// had just written that — so every build failure marked a still-serving
	// app as failed and took the department's stop button away.
	Prior string
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
	prior := markQueued(owner, repo, sha, prID, false, AppHistoryEntry{})
	enqueueDeploy(owner, repo, sha, prID, prior)
}

// markQueued records that a deploy is on its way and returns what the app was
// doing before. wantRunning is whether this deploy is also a request to be
// running — an approved request is, a redeploy is — which a suspension
// outranks: an admin's stop is lifted by an admin, not by a merge.
func markQueued(owner, repo, sha string, prID int64, wantRunning bool, entry AppHistoryEntry) (prior string) {
	if err := MutateAppState(owner, repo, func(st *AppState) bool {
		prior = st.Actual
		st.Owner, st.Repo = owner, repo
		if wantRunning && st.Actual != AppStateSuspended {
			st.Desired = AppStateRunning
		}
		st.Actual = AppStateQueued
		st.SHA = sha
		if prID != 0 {
			st.PRID = prID
		}
		if prior != AppStateSuspended {
			st.Reason, st.Message, st.UserMessage = "", "", "" // a new attempt clears the previous failure
		}
		entry.Status, entry.SHA = AppStateQueued, sha
		st.AppendHistory(entry)
		return true
	}); err != nil {
		log.Error("company: %s/%s: recording queued state: %v", owner, repo, err)
	}
	return prior
}

// enqueueDeploy hands the job to a worker; markQueued has recorded it.
func enqueueDeploy(owner, repo, sha string, prID int64, prior string) {
	select {
	case deployQueue <- deployJob{Owner: owner, Repo: repo, SHA: sha, PRID: prID, Prior: prior}:
	default:
		// Never block the caller: this runs inside a merge notification, and
		// a full queue must not hold up the merge.
		failBuild(owner, repo, ReasonDeployQueueFull,
			"the deploy queue is full; the previously deployed version is still running", prior)
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
func failDeploy(owner, repo, reason, message string) {
	recordDeployFailure(owner, repo, reason, message, AppStateFailed)
}

// failBuild records a failure that happened before anything was swapped.
//
// The build phase never touches the running process — that is the whole
// reason the swap happens last — so the previously deployed version is still
// serving and `restore` is the state it was in before this attempt began.
// Marking it failed took its stop button away and offered a start button for
// something already running, which is the department losing control of the
// version their users are on because of a *later* request that did not work.
func failBuild(owner, repo, reason, message, restore string, userMessage ...string) {
	// Two states survive an attempt that never reached the swap. Running,
	// because those users are still being served by the previous version and
	// the department still owns it. Suspended, because an admin stopped this
	// app on purpose and a failed build is not a reason to undo that.
	// Everything else means nothing is serving, which is a failure.
	if restore != AppStateRunning && restore != AppStateSuspended {
		restore = AppStateFailed
	}
	recordDeployFailure(owner, repo, reason, message, restore, userMessage...)
}

func recordDeployFailure(owner, repo, reason, message, actual string, userMessage ...string) {
	log.Error("company: deploy %s/%s failed (%s): %s", owner, repo, reason, message)
	// Bounded: the state file is re-read on every dashboard load, and a
	// requirements.txt of a thousand lines had put all of them in it twice.
	message = util.TruncateRunes(message, 4000)
	safe := ""
	if len(userMessage) > 0 {
		safe = util.TruncateRunes(userMessage[0], 2000)
	}
	if err := MutateAppState(owner, repo, func(st *AppState) bool {
		st.Actual = actual
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
	// What the app was doing before this attempt. Every failure below happens
	// before the swap, so this is what it is still doing.
	priorActual := job.Prior
	if !settings.IsEnabled() {
		failBuild(owner, repo, ReasonContractViolation, "this app is disabled by an administrator", priorActual)
		return
	}

	// One deploy per app at a time. Two concurrent builds of the same app
	// would race on the release directory and the `current` symlink, and the
	// loser could leave `current` pointing at a half-built tree.
	s := supervisorFor(owner, repo)
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	// Before the build, not after: a deploy migrates the database and takes a
	// copy of it first, so no room now means failing deep inside the runner
	// later, with the previous release already stopped.
	if err := RefuseDeployIfDataFull(ctx, owner, repo); err != nil {
		failBuild(owner, repo, ReasonDataFull, AdminError(err), priorActual, DepartmentSafeError("deploy", err))
		return
	}

	_ = MutateAppState(owner, repo, func(st *AppState) bool {
		st.Actual = AppStateBuilding
		return true
	})

	// Before the build starts, because the build's first act is pip reaching
	// an index: refusing here is a message, refusing later is a connection.
	if !pipEgressAllowed() {
		failBuild(owner, repo, ReasonInstallFailed, AdminError(errPipNoIndex), priorActual, DepartmentSafeError("deploy", errPipNoIndex))
		return
	}

	p := appPathsFor(owner, repo)
	// A directory of its own for every attempt: keyed by the commit alone, a
	// redeploy of the running commit emptied the directory the app was
	// serving from before it knew whether the rebuild would work.
	release := releaseDir(p, job.SHA+"@"+strconv.FormatInt(time.Now().UnixNano(), 10))
	buildNote, err := buildRelease(ctx, job, p, release, settings)
	if err != nil {
		appendBuildLog(p, job.SHA, "FAILED", AdminError(err))
		if denied, ok := errors.AsType[*packagesDeniedError](err); ok {
			// Written by us and naming the packages, so it is the one thing
			// the department needs in order to fix this.
			failBuild(owner, repo, ReasonPackageDenied, denied.Error(), priorActual, denied.Error())
			// Recorded so the next Deploy Request can propose them. Without
			// this, an app whose own requirements are all approved has nothing
			// left to tick on the request form, and the dependency that
			// stopped the build can never be asked for at all.
			recordMissingPackages(owner, repo, denied.packages)
			return
		}
		if errors.Is(err, errNoPython) {
			failBuild(owner, repo, ReasonNoPython, AdminError(err), priorActual, DepartmentSafeError("build", err))
			return
		}
		failBuild(owner, repo, ReasonInstallFailed, AdminError(err), priorActual)
		return
	}

	// Between the build and the swap, and that position is the whole design:
	// the new venv exists (the runner needs it), the old release is still
	// serving, and the changes are additive so it cannot see them. A failure
	// here costs a deploy, never an outage.
	migrationNote, err := migrateForDeploy(ctx, owner, repo, release, job.SHA, settings)
	if err != nil {
		appendBuildLog(p, job.SHA, "FAILED", AdminError(err))
		failBuild(owner, repo, ReasonMigrationFailed, AdminError(err), priorActual, DepartmentSafeError("migrate", err))
		return
	}

	// One entry per attempt, written once its outcome is known. An entry at
	// the end of the build said OK for a release that then failed to come up
	// and was rolled back, and the history showed a success nobody got.
	if err := activateRelease(owner, repo, p, release, job.SHA, settings, job.Prior); err != nil {
		log.Error("company: %s/%s: activation failed: %v", owner, repo, err)
		appendBuildLog(p, job.SHA, "FAILED", AdminError(err)) // the state already says why
		return
	}
	appendBuildLog(p, job.SHA, "OK", strings.TrimSpace("deployed\n"+buildNote+"\n"+migrationNote))

	// Only after a successful activation, and still under deployMu: a failed
	// deploy leaves the previous release serving, and that is the worst
	// possible moment to be deleting anything.
	gcReleases(owner, repo)
}

// buildRelease materializes the code and its dependencies. It writes only
// inside the release directory and never touches `current`, so a failure at
// any point here leaves the running app completely untouched.
func buildRelease(ctx context.Context, job deployJob, p appPaths, release string, settings AppSettings) (note string, err error) {
	appDir := filepath.Join(release, "app")
	if err := os.RemoveAll(release); err != nil {
		return "", err
	}
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		return "", err
	}
	if err := writeReleaseSHA(release, job.SHA); err != nil {
		return "", err
	}

	note, err = extractAppSource(ctx, job, appDir)
	if err != nil {
		return "", err
	}

	return note, venvForAppDir(ctx, p, release, appDir, settings)
}

// venvForAppDir builds the environment the files in appDir ask for, under
// the policy in force. Shared with the startup check (company/deploystartup.go),
// which must install exactly what a deploy would.
func venvForAppDir(ctx context.Context, p appPaths, release, appDir string, settings AppSettings) error {
	body, err := os.ReadFile(filepath.Join(appDir, "requirements.txt"))
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
		return &packagesDeniedError{message: formatRequirementErrors(platformLocale(), reqErrs, settings.BasePackages)}
	}
	if denied := DeniedPackages(reqs, settings.AllowedPackages()); len(denied) > 0 {
		return &packagesDeniedError{message: formatDeniedPackages(denied)}
	}
	return buildVenv(ctx, p, release, reqs, settings)
}

// extractAppSource copies the department's files out of the central deploy
// repo at this commit into the release tree.
func extractAppSource(ctx context.Context, job deployJob, appDir string) (note string, err error) {
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return "", err
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		return "", err
	}
	gitRepo, err := git.OpenRepository(ctx, central)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()

	commit, err := gitRepo.GetCommit(ctx, job.SHA)
	if err != nil {
		return "", fmt.Errorf("the deployed commit %s could not be read: %w", job.SHA, err)
	}
	tree, err := commit.SubTree(ctx, gitRepo, deployPathPrefix(job.Owner, job.Repo))
	if err != nil {
		return "", fmt.Errorf("no files were found for this app in the deploy repository: %w", err)
	}
	links, err := writeTreeFiles(ctx, gitRepo, tree, appDir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(appDir, "main.py")); err != nil {
		return "", errors.New("main.py was not found — the app must have a main.py exposing `app` in its repository root")
	}
	if len(links) > 0 {
		note = "symbolic links are not deployed; left out: " + strings.Join(links, ", ")
	}
	return note, nil
}

// writeTreeFiles writes every file of a git tree under appDir, and names
// the links it left out.
func writeTreeFiles(ctx context.Context, gitRepo *git.Repository, tree *git.Tree, appDir string) (links []string, err error) {
	entries, err := tree.ListEntriesRecursiveFast(ctx, gitRepo)
	if err != nil {
		return nil, err
	}

	var total int64
	for _, entry := range entries {
		if entry.IsDir() || entry.IsSubModule() {
			continue
		}
		if entry.IsLink() {
			// Written out, a link would be a file holding its target's name,
			// and the app would open that instead of what it meant. Left out,
			// and said so in the build log, rather than silently changed.
			links = append(links, entry.Name())
			continue
		}
		dest, err := safeJoin(appDir, entry.Name())
		if err != nil {
			// A tree entry is not user input in the usual sense, but it does
			// reach here through data staff control, so the path is checked
			// rather than trusted.
			return nil, err
		}
		blob := entry.Blob(gitRepo)
		total += blob.Size(ctx)
		if total > maxSourceBytes {
			return nil, fmt.Errorf("the app's files exceed the %d MB limit", maxSourceBytes>>20)
		}
		// GetBlobBytes treats a non-positive limit as "read nothing".
		content, err := blob.GetBlobBytes(ctx, blob.Size(ctx))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return nil, err
		}
		// Never executable: the app is started by the platform, and a file
		// the department can mark +x is a way to get something other than
		// uvicorn running.
		if err := os.WriteFile(dest, content, 0o600); err != nil {
			return nil, err
		}
	}
	return links, nil
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

	// The marker, not bin/python: python -m venv writes the interpreter
	// before pip installs anything, and an install cut short by a kill left
	// an environment every later deploy of the same requirements reused.
	if _, err := os.Stat(filepath.Join(venv, venvCompleteMarker)); err != nil {
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
		if err := os.WriteFile(filepath.Join(venv, venvCompleteMarker), nil, 0o600); err != nil {
			return err
		}
	} else if extra, err := unapprovedInstalled(venv, settings, baseStackPackages(venv, settings.BasePackages)); err != nil {
		return err
	} else if len(extra) > 0 {
		// A cached environment is checked against today's allowlist, not the
		// one it was built under: a package an admin withdrew stayed
		// importable in every release built from the cache. Against the same
		// baseline as a fresh build, or the platform's own stack — starlette,
		// anyio and the rest — would be refused as the department's choice.
		return &packagesDeniedError{
			message:  platformLocale().TrString("company.err.deps_unapproved", strings.Join(extra, ", ")),
			packages: extra,
		}
	}

	// A symlink rather than a copy: a venv's scripts contain absolute paths,
	// so copying one produces an environment that silently uses the wrong
	// interpreter. bwrap resolves the symlink when it binds it.
	_ = os.Remove(link)
	if err := os.Symlink(venv, link); err != nil {
		return err
	}

	// What this release actually has, recorded beside it.
	//
	// Written here rather than during the install because the environment is
	// cached: a release that reused one installed nothing, and would
	// otherwise carry no record at all. The admin screen shows policy — what
	// an app *may* install — and policy changes without the app being
	// rebuilt, so the two drift and nothing on screen said which was which.
	//
	// Best-effort: a release that could not be described still deploys.
	recordInstalledPackages(venv, release)
	return nil
}

// recordInstalledPackages writes the venv's package list into the release.
func recordInstalledPackages(venv, release string) {
	installed, err := distInfoPackages(venv)
	if err != nil {
		log.Warn("company: listing packages for %s: %v", release, err)
		return
	}
	names := make([]string, 0, len(installed))
	for _, pkg := range installed {
		if slices.Contains(pipBaseline, strings.ToLower(pkg.Name)) {
			continue // every venv has these by construction
		}
		names = append(names, pkg.Name+"=="+pkg.Version)
	}
	slices.Sort(names)
	if err := writeReleaseManifest(release, names); err != nil {
		log.Warn("company: recording packages for %s: %v", release, err)
	}
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
			return audienceKeyError("company.err.base_install_failed", "company.err.base_install_failed.admin", err.Error())
		}
	}
	baseline, err := installedPackages(venv)
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
	extra, err := unapprovedInstalled(venv, settings, baseline)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		// The names travel as data as well as prose: they are what the next
		// Deploy Request has to offer for approval, and re-extracting them
		// from a sentence later would be guessing at our own message.
		return &packagesDeniedError{
			// platformLocale, not English: this is stored and shown to a
			// department later, and the build worker has no reader whose
			// language it could use instead (company/usererror.go).
			message:  platformLocale().TrString("company.err.deps_unapproved", strings.Join(extra, ", ")),
			packages: extra,
		}
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
func installedPackages(venv string) (map[string]bool, error) {
	installed, err := distInfoPackages(venv)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(installed))
	for _, pkg := range installed {
		names[normalizePackageName(pkg.Name)] = true
	}
	return names, nil
}

type installedPackage struct {
	Name, Version string
	Requires      []string // what its metadata says it depends on, by name
}

// platformStackPackages is the base stack with what it pulls in, as names
// and versions, read from the first complete environment on this host: the
// list an administrator sees under the base packages so that "anyio" on a
// request is recognisable. Empty until something has been built.
func platformStackPackages(basePackages []string) []installedPackage {
	venvs, _ := filepath.Glob(filepath.Join(setting.AppDataPath, "company-apps", "*", "venvs", "*", venvCompleteMarker))
	for _, marker := range venvs {
		venv := filepath.Dir(marker)
		stack := baseStackPackages(venv, basePackages)
		if len(stack) == 0 {
			continue
		}
		installed, err := distInfoPackages(venv)
		if err != nil {
			continue
		}
		var out []installedPackage
		for _, pkg := range installed {
			if stack[normalizePackageName(pkg.Name)] {
				out = append(out, installedPackage{Name: pkg.Name, Version: pkg.Version})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	return nil
}

// baseStackPackages is everything the platform's base packages bring in,
// read from the metadata of what is installed: the packages themselves and,
// following Requires-Dist, all they depend on. The same set a fresh build
// records after installing the base and before the department's additions.
func baseStackPackages(venv string, basePackages []string) map[string]bool {
	installed, err := distInfoPackages(venv)
	if err != nil {
		return map[string]bool{}
	}
	requires := make(map[string][]string, len(installed))
	for _, pkg := range installed {
		requires[normalizePackageName(pkg.Name)] = pkg.Requires
	}
	stack := map[string]bool{}
	queue := BasePackageNames(basePackages)
	for len(queue) > 0 {
		name := normalizePackageName(queue[0])
		queue = queue[1:]
		deps, installed := requires[name]
		if stack[name] || !installed {
			continue
		}
		stack[name] = true
		queue = append(queue, deps...)
	}
	return stack
}

// distInfoPackages reads what is installed from the metadata pip wrote,
// without running anything. `pip list` ran the interpreter — and with it any
// .pth file a freshly installed, not yet approved wheel had put in
// site-packages — outside the sandbox, as Gitea.
func distInfoPackages(venv string) ([]installedPackage, error) {
	dirs, err := filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages", "*.dist-info"))
	if err != nil {
		return nil, err
	}
	var out []installedPackage
	for _, dir := range dirs {
		body, err := os.ReadFile(filepath.Join(dir, "METADATA"))
		if err != nil {
			continue // a directory pip is still writing, or has half removed
		}
		var pkg installedPackage
		for line := range strings.SplitSeq(string(body), "\n") {
			if pkg.Name == "" && strings.HasPrefix(line, "Name: ") {
				pkg.Name = strings.TrimSpace(strings.TrimPrefix(line, "Name: "))
			} else if pkg.Version == "" && strings.HasPrefix(line, "Version: ") {
				pkg.Version = strings.TrimSpace(strings.TrimPrefix(line, "Version: "))
			} else if dep, ok := strings.CutPrefix(line, "Requires-Dist: "); ok {
				// "anyio<5,>=3.4.0; extra == 'standard'" — the name is what
				// precedes the first version, extra or condition marker.
				name := strings.TrimSpace(dep)
				if i := strings.IndexAny(name, " <>=!~;[("); i >= 0 {
					name = name[:i]
				}
				if name != "" {
					pkg.Requires = append(pkg.Requires, normalizePackageName(name))
				}
			} else if line == "" {
				break // the headers end at the first blank line
			}
		}
		if pkg.Name != "" {
			out = append(out, pkg)
		}
	}
	return out, nil
}

// pipBaseline are the packages every venv has by construction.
var pipBaseline = []string{"pip", "setuptools", "wheel", "pkg-resources"}

func unapprovedInstalled(venv string, settings AppSettings, baseline map[string]bool) ([]string, error) {
	installed, err := distInfoPackages(venv)
	if err != nil {
		return nil, fmt.Errorf("listing installed packages failed: %w", err)
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
func activateRelease(owner, repo string, p appPaths, release, sha string, settings AppSettings, prior string) error {
	s := supervisorFor(owner, repo)

	previous, _ := os.Readlink(p.current)

	_ = MutateAppState(owner, repo, func(st *AppState) bool {
		st.Actual = AppStateActivating
		return true
	})

	// A stop pressed while this was building is a decision, not a race to
	// lose: the new release is put in place but not started. Nor is an
	// admin's suspension lifted by a merge.
	suspended := prior == AppStateSuspended
	stayStopped := suspended || LoadAppState(owner, repo).Desired == AppStateStopped

	// The same sentence in the app's state and in the deploy history, which
	// otherwise got whatever error happened last — a socket refusing — for a
	// summary.
	fail := func(reason, msg string) error {
		failDeploy(owner, repo, reason, msg)
		return errors.New(msg)
	}

	s.mu.Lock()
	stopErr := s.stopLocked()
	s.mu.Unlock()
	if stopErr != nil {
		// Still running from the old release: swapping underneath it and
		// starting a second copy beside it would both be worse than stopping here.
		return fail(ReasonStopFailed, "the running version could not be stopped: "+AdminError(stopErr))
	}

	if err := swapSymlink(p.current, release); err != nil {
		return fail(ReasonContractViolation, "switching to the new version failed: "+err.Error())
	}
	if previous != "" {
		_ = swapSymlink(p.previous, previous)
	}

	deployedStopped := func() error {
		return MutateAppState(owner, repo, func(st *AppState) bool {
			st.Actual = AppStateStopped
			if suspended {
				st.Actual = AppStateSuspended // the reason and the admin's words are still there
			} else {
				st.Reason, st.Message, st.UserMessage = "", "", ""
			}
			st.HasRelease = true
			st.SHA = sha
			st.AppendHistory(AppHistoryEntry{Status: st.Actual, SHA: sha, Reason: "deployed while stopped"})
			return true
		})
	}
	if stayStopped {
		return deployedStopped()
	}

	// startProcess, not Start: the state must not say "running" until the
	// health check passes, or a release that never answers shows a green
	// badge for the whole check window before flipping to failed. Nor may it
	// claim the app running: a stop pressed meanwhile is the later decision.
	pid, startErr := s.startProcess(true)
	if errors.Is(startErr, errStartSuperseded) {
		return deployedStopped()
	}
	// Why the new release did not go live, for every record of this attempt.
	notLive := startErr
	if startErr == nil {
		// Healthy, and then still the same process a few seconds later. An app
		// that crashes shortly after boot restarts fast enough to answer every
		// probe, so the check alone would activate a release that is dying in
		// a loop and show it as running.
		notLive = waitHealthy(p.socket, settings)
		if notLive == nil && !s.stableFor(pid, healthSettleDelay) {
			notLive = errors.New("it answered its health check and then exited")
		}
		if notLive == nil {
			return MutateAppState(owner, repo, func(st *AppState) bool {
				st.HasRelease = true
				st.SHA = sha
				st.MissingPackages = nil // it installed; nothing is outstanding
				if st.Desired == AppStateStopped {
					// Stopped during the check: the release is in place, and
					// the stop's record of the app stands.
					st.AppendHistory(AppHistoryEntry{Status: AppStateStopped, SHA: sha, Reason: "deployed while stopped"})
					return true
				}
				st.Actual = AppStateRunning
				st.Desired = AppStateRunning
				st.Sandboxed, _ = SandboxStatus()
				st.PID = pid
				st.StartedAt = time.Now().Unix()
				st.Reason, st.Message, st.UserMessage = "", "", ""
				st.FailedAt = 0
				st.Health = AppHealth{State: "up", CheckedAt: time.Now().Unix()}
				st.AppendHistory(AppHistoryEntry{Status: AppStateRunning, SHA: sha})
				return true
			})
		}
	}

	// A stop pressed during the check is why it failed, not the release, and
	// the rollback below must not start anything against it.
	if LoadAppState(owner, repo).Desired == AppStateStopped {
		return deployedStopped()
	}

	failure := fmt.Sprintf("the new version did not come up (%s)", AdminError(notLive))

	// The new release is not serving. Put the old one back — nobody should be
	// left with a broken app because someone pushed a typo.
	if previous == "" {
		return fail(ReasonHealthTimeout, failure+", and there is no previous version to fall back to")
	}
	log.Warn("company: %s/%s: new release unhealthy, rolling back to %s", owner, repo, previous)

	s.mu.Lock()
	stopErr = s.stopLocked()
	s.mu.Unlock()
	if stopErr != nil {
		return fail(ReasonStopFailed, failure+", and it could not be stopped: "+AdminError(stopErr))
	}
	if err := swapSymlink(p.current, previous); err != nil {
		return fail(ReasonHealthTimeout, failure+", and rolling back failed: "+err.Error())
	}
	// `previous` was set to this same directory a moment ago, when the new
	// release went in. Leaving it there would make the next rollback restart
	// the version already running and look like it did nothing.
	_ = os.Remove(p.previous)
	restored := errors.New(failure + ", so the previous version was restored")
	// The attempt itself, under its own commit. Without it the history went
	// from "queued" straight to the previous version running, and read as
	// though nothing had gone wrong.
	failedAttempt := AppHistoryEntry{Status: AppStateFailed, SHA: sha, Reason: ReasonHealthTimeout}
	pid, err := s.startProcess(true)
	if errors.Is(err, errStartSuperseded) {
		// Stopped meanwhile; the previous version is in place for whoever starts it.
		_ = MutateAppState(owner, repo, func(st *AppState) bool {
			st.AppendHistory(failedAttempt)
			return true
		})
		return restored
	}
	if err != nil {
		return fail(ReasonRolledBack, failure+", and the previous version could not be restarted either: "+AdminError(err))
	}
	if err := waitHealthy(p.socket, settings); err != nil {
		// "rolled back" and "currently down" are different things and an admin
		// has to be able to tell them apart.
		return fail(ReasonHealthTimeout, failure+", and the previous version did not respond either, so the app is down: "+AdminError(err))
	}
	_ = MutateAppState(owner, repo, func(st *AppState) bool {
		st.AppendHistory(failedAttempt)
		if st.Desired == AppStateStopped {
			return true
		}
		st.Actual = AppStateRunning
		st.Desired = AppStateRunning
		st.Sandboxed, _ = SandboxStatus()
		st.PID = pid
		st.StartedAt = time.Now().Unix()
		st.HasRelease = true
		st.FailedAt = time.Now().Unix()
		st.Reason = ReasonRolledBack
		st.Message = restored.Error()
		st.Health = AppHealth{State: "up", CheckedAt: time.Now().Unix()}
		// The restored release, not the one that just failed: every later
		// restart and redeploy reads this field.
		adoptCurrentReleaseSHA(st, owner, repo)
		st.AppendHistory(AppHistoryEntry{Status: AppStateRunning, SHA: st.SHA, Reason: ReasonRolledBack})
		return true
	})
	// Serving, but not what was deployed: a failure, whatever the badge says.
	return restored
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
// FastAPI apps — so probeHealth decides what "answers" means. Consecutive
// successes are required because an app that starts, answers once, and dies
// would otherwise pass.
func waitHealthy(socket string, settings AppSettings) error {
	client := healthClient(socket)
	streak := 0
	var lastErr error
	for range healthCheckTries {
		time.Sleep(healthCheckSpacing)
		ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
		_, err := probeHealth(ctx, client, settings.HealthPath)
		cancel()
		if err != nil {
			lastErr = err
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
	cmd.Env = append([]string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + os.TempDir(),
		"LANG=C.UTF-8",
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
	}, pipIndexEnv()...) // the sanctioned index, if one is set (company/pip_policy.go)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// packagesDeniedError separates "you asked for something unapproved" from
// "the install broke", because the department sees a different message and a
// different button for each.
type packagesDeniedError struct {
	message string
	// packages is what still needs approving, where we know the names.
	packages []string
}

func (e *packagesDeniedError) Error() string { return e.message }

// formatRequirementErrors writes the rejected lines out for the department.
//
// Base packages are singled out because they are the likeliest thing in a
// broken requirements.txt and the advice for them is the opposite of the
// advice for everything else: the platform already installs these, so the fix
// is to delete the line, not to find a version for it. Telling someone to pin
// a package that is already installed sends them to look up a version number
// they then have to keep correct forever, for nothing.
func formatRequirementErrors(l translation.Locale, errs []RequirementError, basePackages []string) string {
	provided := make(map[string]bool, len(basePackages))
	for _, name := range BasePackageNames(basePackages) {
		provided[normalizePackageName(name)] = true
	}
	lines := make([]string, 0, len(errs))
	for _, e := range errs {
		reason := l.TrString(e.Reason, e.Args...)
		if provided[normalizePackageName(e.Text)] {
			reason = l.TrString("company.req.already_provided", e.Text)
		}
		lines = append(lines, l.TrString("company.req.at_line", e.Line, reason))
	}
	return strings.Join(lines, "\n")
}

func formatDeniedPackages(denied []Requirement) string {
	names := make([]string, 0, len(denied))
	for _, r := range denied {
		names = append(names, r.Name)
	}
	if len(names) > 20 {
		names = append(names[:20], fmt.Sprintf("and %d more", len(names)-20))
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

// recordMissingPackages stores what the last build could not install.
func recordMissingPackages(owner, repo string, packages []string) {
	if err := MutateAppState(owner, repo, func(st *AppState) bool {
		if slices.Equal(st.MissingPackages, packages) {
			return false
		}
		st.MissingPackages = packages
		return true
	}); err != nil {
		log.Error("company: %s/%s: recording missing packages: %v", owner, repo, err)
	}
}
