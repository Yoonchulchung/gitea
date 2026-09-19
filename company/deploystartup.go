// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/modules/git"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	gitea_context "gitea.dev/services/context"
)

// Parsing says a file is Python; it does not say the app starts. A main.py
// holding one undefined name parses and dies on import, and until now the
// department learned that after an administrator had approved it. The
// startup check is the deploy, done early and thrown away: the department's
// current files built the way a release is built — same packages, same
// environment, same sandbox — then asked to do what the platform asks of a
// deployed app: import main, provide `app`, finish startup, answer the
// health path. Nothing of it touches the live app; the release it builds
// is deleted when it is done and only the package environment, keyed by the
// requirements, stays for the real deploy to reuse.
//
// A request cannot be submitted while its version fails this, or before the
// check has finished. It can when the check cannot run at all — packages
// still waiting for approval, a host that refuses to sandbox — because the
// request is then the thing that fixes it.

const (
	startupCheckTimeout = 90 * time.Second
	startupOutputTail   = 8 << 10
	startupCheckDir     = "checks" // under the app's home, beside releases; never the live release
)

// StartupCheck is one run's record.
type StartupCheck struct {
	Owner, Repo, SHA string
	State            string // "building", "running", "passed", "failed", "blocked", "unavailable"
	Stage            string // where a failed run stopped: "build", "import", "app", "startup", "request", "timeout"
	Error            string // what the run said, for the department
	Output           string // the app's own output, its tail
	HealthPath       string
	StartedAt        time.Time
	FinishedAt       time.Time
}

// Pending is a check still working.
func (c StartupCheck) Pending() bool { return c.State == "building" || c.State == "running" }

// Blocks says whether a request on this version has to wait.
func (c StartupCheck) Blocks() bool { return c.Pending() || c.State == "failed" }

// Seconds is how long the run took.
func (c StartupCheck) Seconds() int {
	if c.FinishedAt.IsZero() {
		return 0
	}
	return int(c.FinishedAt.Sub(c.StartedAt).Seconds())
}

type startupRecord struct {
	mu    sync.Mutex
	check StartupCheck
}

var startupChecks sync.Map // appKey → *startupRecord

func startupRecordFor(owner, repo string) *startupRecord {
	rec, _ := startupChecks.LoadOrStore(appKey(owner, repo), &startupRecord{})
	record, _ := rec.(*startupRecord)
	return record
}

func (r *startupRecord) set(mutate func(*StartupCheck)) {
	r.mu.Lock()
	mutate(&r.check)
	r.mu.Unlock()
}

// EnsureStartupCheck is the check of the repository's default branch as it
// is now, started if there is none for that version. A run in progress is
// left to finish, whatever version it is of. force runs it again — after a
// package was approved, say.
func EnsureStartupCheck(ctx *gitea_context.Context, repo *repo_model.Repository, force bool) (StartupCheck, bool) {
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, repo)
	if err != nil {
		return StartupCheck{}, false
	}
	commit, err := gitRepo.GetBranchCommit(ctx, repo.DefaultBranch)
	if err != nil {
		return StartupCheck{}, false // an empty repository: nothing to run
	}
	sha := commit.ID.String()
	rec := startupRecordFor(repo.OwnerName, repo.Name)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.check.Pending() || (rec.check.SHA == sha && !force) {
		return rec.check, true
	}
	rec.check = StartupCheck{
		Owner: repo.OwnerName, Repo: repo.Name, SHA: sha,
		State: "building", StartedAt: time.Now(),
		HealthPath: SettingsFor(repo.OwnerName, repo.Name).HealthPath,
	}
	go runStartupCheck(graceful.GetManager().ShutdownContext(), rec, repo.ID, sha)
	return rec.check, true
}

// runStartupCheck is the deploy's own steps on a throwaway release, then
// the run.
func runStartupCheck(ctx context.Context, rec *startupRecord, repoID int64, sha string) {
	owner, name := rec.check.Owner, rec.check.Repo
	settings := SettingsFor(owner, name)
	p := appPathsFor(owner, name)
	sum := sha256.Sum256([]byte(sha))
	release := filepath.Join(p.home, startupCheckDir, hex.EncodeToString(sum[:8]))
	appDir := filepath.Join(release, "app")
	// A traceback names files by where the check put them; the department
	// knows them by their names in the repository.
	forDepartment := func(text string) string {
		text = strings.ReplaceAll(text, appDir+"/", "")
		text = strings.ReplaceAll(text, `"/app/`, `"`)
		return RedactServerPaths(text)
	}
	finish := func(state, stage string, err error, output string) {
		rec.set(func(c *StartupCheck) {
			c.State, c.Stage, c.FinishedAt = state, stage, time.Now()
			if err != nil {
				if _, spoken := userMessage(err); spoken {
					c.Error = DepartmentSafeErrorL(platformLocale(), "startup check", err)
				} else {
					c.Error = forDepartment(err.Error()) // pip's line, a traceback: what the department has to fix
				}
			}
			c.Output = forDepartment(output)
		})
		log.Info("company: %s/%s startup check of %s: %s %s", owner, name, sha[:8], state, stage)
	}

	defer func() { _ = os.RemoveAll(release) }()
	if err := os.RemoveAll(release); err != nil {
		finish("unavailable", "", err, "")
		return
	}
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		finish("unavailable", "", err, "")
		return
	}
	if err := extractRepoTree(ctx, repoID, sha, appDir); err != nil {
		finish("unavailable", "", err, "")
		return
	}
	if _, err := os.Stat(filepath.Join(appDir, "main.py")); err != nil {
		finish("failed", "app", userKeyError("company.err.no_main_py"), "")
		return
	}

	// Under the app's deploy lock: the package environment is shared with
	// real deploys, and two installs into one directory corrupt it.
	s := supervisorFor(owner, name)
	s.deployMu.Lock()
	err := venvForAppDir(ctx, p, release, appDir, settings)
	s.deployMu.Unlock()
	if err != nil {
		if _, denied := errors.AsType[*packagesDeniedError](err); denied {
			finish("blocked", "", err, "")
			return
		}
		finish("failed", "build", err, "")
		return
	}
	if err := installPlatformShim(filepath.Join(release, ".venv")); err != nil {
		finish("unavailable", "", err, "")
		return
	}

	// Its own run and data directories, so the check shares nothing with the
	// live app: not the socket, not the database, not the logs.
	cp := p
	cp.run = filepath.Join(release, "run")
	cp.ctl = filepath.Join(release, "ctl")
	cp.socket = filepath.Join(cp.run, "c.sock")
	cp.broker = filepath.Join(cp.run, "b.sock")
	cp.logs = filepath.Join(release, "logs")
	dataDir := filepath.Join(release, "data")
	for _, dir := range []string{filepath.Join(cp.run, "home"), filepath.Join(cp.run, "tmp"), dataDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			finish("unavailable", "", err, "")
			return
		}
	}
	cmd, err := buildSandboxCommand(release, cp, settings, dataDir, "", []string{"python", "-c", startupCheckScript, settings.HealthPath})
	if err != nil {
		finish("unavailable", "", err, "")
		return
	}
	appEnv, _, _ := LoadAppEnv(owner, name)
	cmd.Env = buildEnv(cp, "/apps/"+owner+"/"+name, dataDir, appEnv, false)
	cmd.Dir = appDir
	cmd.Env = append(cmd.Env, platformShimEnv+"="+appCodeDirForProcess(appDir))
	out := &tailBuffer{limit: startupOutputTail}
	setAppProcessAttrs(cmd, out)
	rec.set(func(c *StartupCheck) { c.State = "running" })
	if err := cmd.Start(); err != nil {
		finish("unavailable", "", err, "")
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timedOut := false
	select {
	case <-done:
	case <-time.After(startupCheckTimeout):
		timedOut = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // whatever the app started and left

	result, found := parseStartupResult(out.String())
	output := out.StringWithoutResult()
	switch {
	case timedOut:
		finish("failed", "timeout", nil, output)
	case !found:
		finish("failed", "import", errors.New("the check ended without a result — the app exited on its own while starting"), output)
	case result.OK:
		finish("passed", "", nil, output)
	default:
		finish("failed", result.Stage, errors.New(strings.TrimSpace(result.Error)), output)
	}
}

// extractRepoTree writes the department repository's tree at sha into
// appDir, the way a deploy writes the snapshot's.
func extractRepoTree(ctx context.Context, repoID int64, sha, appDir string) error {
	repo, err := repo_model.GetRepositoryByID(ctx, repoID)
	if err != nil {
		return err
	}
	gitRepo, err := git.OpenRepository(ctx, repo)
	if err != nil {
		return err
	}
	defer gitRepo.Close()
	commit, err := gitRepo.GetCommit(ctx, sha)
	if err != nil {
		return err
	}
	tree, err := commit.SubTree(ctx, gitRepo, "/")
	if err != nil {
		return err
	}
	_, err = writeTreeFiles(ctx, gitRepo, tree, appDir)
	return err
}

const startupResultMarker = "@@RESULT@@ "

type startupResult struct {
	OK     bool   `json:"ok"`
	Stage  string `json:"stage"`
	Error  string `json:"error"`
	Status int    `json:"status"`
}

// parseStartupResult finds the script's verdict in the process output.
func parseStartupResult(output string) (startupResult, bool) {
	i := strings.LastIndex(output, startupResultMarker)
	if i < 0 {
		return startupResult{}, false
	}
	line := output[i+len(startupResultMarker):]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	var r startupResult
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return startupResult{}, false
	}
	return r, true
}

// tailBuffer keeps the end of what a process wrote.
type tailBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if over := t.buf.Len() - t.limit; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

// StringWithoutResult is the output minus the verdict line, which is ours.
func (t *tailBuffer) StringWithoutResult() string {
	s := t.String()
	if i := strings.LastIndex(s, startupResultMarker); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// startupCheckScript runs inside the app's environment and sandbox, with
// the health path as its argument. It reports on one line and leaves by
// os._exit, so a thread or loop the app started cannot keep the check
// waiting.
const startupCheckScript = `import asyncio, json, os, sys, traceback

HEALTH = sys.argv[1] if len(sys.argv) > 1 and sys.argv[1] else "/health"
ROOT = os.environ.get("ROOT_PATH", "")

def report(**r):
    sys.stdout.flush(); sys.stderr.flush()
    print("\n@@RESULT@@ " + json.dumps(r), flush=True)
    os._exit(0)

def fail(stage, error=None):
    report(ok=False, stage=stage, error=error or traceback.format_exc(limit=8))

try:
    import main
except BaseException:
    fail("import")
app = getattr(main, "app", None)
if app is None:
    fail("app", "main.py defines no app")
try:
    from uvicorn.config import Config
    cfg = Config(app, root_path=ROOT, lifespan="on")
    cfg.load()
    asgi = cfg.loaded_app
except BaseException:
    fail("app")

async def run():
    state, seen = {}, {}
    inbox = asyncio.Queue()
    started, stopped = asyncio.Event(), asyncio.Event()
    async def receive():
        return await inbox.get()
    async def send(msg):
        seen[msg["type"]] = msg
        if msg["type"].startswith("lifespan.startup"): started.set()
        if msg["type"].startswith("lifespan.shutdown"): stopped.set()
    await inbox.put({"type": "lifespan.startup"})
    life = asyncio.ensure_future(asgi({"type": "lifespan", "asgi": {"version": "3.0", "spec_version": "2.0"}, "state": state}, receive, send))
    waiter = asyncio.ensure_future(started.wait())
    await asyncio.wait({life, waiter}, timeout=60, return_when=asyncio.FIRST_COMPLETED)
    waiter.cancel()
    if "lifespan.startup.failed" in seen:
        fail("startup", seen["lifespan.startup.failed"].get("message") or "startup failed")
    if not started.is_set() and not life.done():
        fail("startup", "startup did not complete within 60 seconds")
    # an app without lifespan support raises here; uvicorn carries on, and so does this
    path = HEALTH.split("?", 1)[0]
    scope = {"type": "http", "asgi": {"version": "3.0", "spec_version": "2.3"}, "http_version": "1.1",
             "method": "GET", "scheme": "http", "path": path, "raw_path": path.encode(),
             "query_string": b"platform-health-check=1", "root_path": ROOT,
             "headers": [(b"host", b"app")], "client": ("127.0.0.1", 0), "server": ("app", 80), "state": state}
    status = {}
    async def rreceive():
        return {"type": "http.request", "body": b"", "more_body": False}
    async def rsend(msg):
        if msg["type"] == "http.response.start": status["code"] = msg["status"]
    try:
        await asyncio.wait_for(asgi(scope, rreceive, rsend), 30)
    except asyncio.TimeoutError:
        fail("request", "GET %s did not answer within 30 seconds" % path)
    except BaseException:
        fail("request")
    code = status.get("code")
    if code is None or code >= 500:
        fail("request", "GET %s answered HTTP %s" % (path, code))
    if started.is_set():
        await inbox.put({"type": "lifespan.shutdown"})
        w = asyncio.ensure_future(stopped.wait())
        await asyncio.wait({life, w}, timeout=10, return_when=asyncio.FIRST_COMPLETED)
        w.cancel()
    report(ok=True, status=code)

asyncio.run(run())
`

// StartupCheckStatus is GET /{owner}/{repo}/deploy/startup-check: the box
// on the request form, for the form to refresh while the check runs.
func StartupCheckStatus(ctx *gitea_context.Context) {
	if !ctx.Repo.Permission.CanRead(unit.TypeCode) || isCentralDeployRepo(ctx.Repo.Repository) {
		ctx.NotFound(nil)
		return
	}
	check, ok := EnsureStartupCheck(ctx, ctx.Repo.Repository, false)
	ctx.Data["StartupCheck"] = check
	ctx.Data["HasStartupCheck"] = ok
	ctx.HTML(http.StatusOK, "company/deploy_startup")
}

// StartupCheckRerun is POST /{owner}/{repo}/deploy/startup-check.
func StartupCheckRerun(ctx *gitea_context.Context) {
	if !ctx.Repo.Permission.CanRead(unit.TypeCode) || isCentralDeployRepo(ctx.Repo.Repository) {
		ctx.NotFound(nil)
		return
	}
	EnsureStartupCheck(ctx, ctx.Repo.Repository, true)
	ctx.Redirect(ctx.Repo.Repository.Link()+"/deploy", http.StatusSeeOther)
}
