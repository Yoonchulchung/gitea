// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	issues_model "gitea.dev/models/issues"
	"gitea.dev/modules/git"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	gitea_context "gitea.dev/services/context"
)

// An administrator deciding on a deploy request can run the requested
// version and look at it first. The preview is a second, separate copy of
// the app: its own release built the way a deploy builds one, its own
// process and socket, and a copy of today's data — so nothing it does
// reaches the live app or its database. It is reachable by administrators
// only, at /apps/_preview/{owner}/{repo}/, and stops by itself when nobody
// has opened it for a while or the request is decided.
//
// It borrows the app's identity with a suffix no repository can be named
// with ("~"), so every path, socket and supervisor the platform derives
// from a name is automatically its own. It never writes app state, so it
// is never listed, reconciled at boot, or routable at the app's own URL.

const (
	previewSuffix     = "~preview"
	previewIdle       = 30 * time.Minute
	previewSweep      = time.Minute
	previewMarkerFile = "preview" // in the preview's home, so leftovers are recognisable at boot
)

var previewPrefix = appProxyPrefix + "/_preview"

func previewRepo(repo string) string { return repo + previewSuffix }

// isPreviewRepo reports a name the preview borrowed.
func isPreviewRepo(repo string) bool { return strings.HasSuffix(repo, previewSuffix) }

// appPreview is one preview's record: what it is of, and where it stands.
type appPreview struct {
	Owner, Repo string
	PRID        int64
	SHA         string

	mu        sync.Mutex
	building  bool
	err       string // for an administrator, when the build or start failed
	dataNote  string // "copied" or "empty": what the preview's data is
	startedAt time.Time
	lastHit   atomic.Int64 // unix seconds of the last request, for the idle stop
}

// PreviewStatus is the record as the page and its poll see it.
type PreviewStatus struct {
	State     string // "building", "running", "failed" or "stopped"
	Error     string
	DataNote  string
	URL       string
	StartedAt time.Time
}

var previews sync.Map // appRegistryKey(owner, repo) -> *appPreview

func previewOf(owner, repo string) (*appPreview, bool) {
	v, ok := previews.Load(appRegistryKey(owner, repo))
	if !ok {
		return nil, false
	}
	pv, ok := v.(*appPreview)
	return pv, ok
}

// previewSupervisor is the process supervisor for the preview of one app —
// the app's own, under the borrowed name, told what it is.
func previewSupervisor(owner, repo string) *appSupervisor {
	s := supervisorFor(owner, previewRepo(repo))
	s.preview = true
	s.rootPath = previewURL(owner, repo)
	return s
}

func previewURL(owner, repo string) string {
	return previewPrefix + "/" + owner + "/" + repo
}

// PreviewStatusOf reports where an app's preview stands.
func PreviewStatusOf(owner, repo string) PreviewStatus {
	pv, ok := previewOf(owner, repo)
	if !ok {
		return PreviewStatus{State: "stopped"}
	}
	pv.mu.Lock()
	defer pv.mu.Unlock()
	st := PreviewStatus{DataNote: pv.dataNote, URL: setting.AppSubURL + previewURL(pv.Owner, pv.Repo) + "/", StartedAt: pv.startedAt}
	switch {
	case pv.building:
		st.State = "building"
	case pv.err != "":
		st.State = "failed"
		st.Error = pv.err
	case IsAppRunning(pv.Owner, previewRepo(pv.Repo)):
		st.State = "running"
	default:
		st.State = "stopped"
	}
	return st
}

// StartPreview builds and runs sha as the preview of owner/repo. The build
// takes as long as a deploy's; it runs in the background and the record
// says when it is done. A preview already being built is left alone.
func StartPreview(owner, repo string, prID int64, sha string) error {
	if _, err := pythonPath(); err != nil {
		return err
	}
	key := appRegistryKey(owner, repo)
	pv := &appPreview{Owner: owner, Repo: repo, PRID: prID, SHA: sha, building: true}
	pv.lastHit.Store(time.Now().Unix())
	if old, ok := previewOf(owner, repo); ok {
		old.mu.Lock()
		building := old.building
		old.mu.Unlock()
		if building {
			return userKeyError("company.err.preview_building")
		}
	}
	previews.Store(key, pv)
	go pv.build(graceful.GetManager().ShutdownContext())
	return nil
}

func (pv *appPreview) fail(err error) {
	pv.mu.Lock()
	pv.building = false
	pv.err = AdminError(err)
	pv.mu.Unlock()
	log.Warn("company: preview of %s/%s: %v", pv.Owner, pv.Repo, err)
}

// build is the deploy's own steps, on the preview's own paths, and never
// activateRelease: that is the one step that touches the live app.
func (pv *appPreview) build(ctx context.Context) {
	s := previewSupervisor(pv.Owner, pv.Repo)
	s.mu.Lock()
	stopErr := s.stopLocked()
	s.mu.Unlock()
	if stopErr != nil {
		pv.fail(stopErr)
		return
	}
	if err := os.RemoveAll(s.paths.home); err != nil {
		pv.fail(err)
		return
	}
	if err := os.MkdirAll(s.paths.home, 0o700); err != nil {
		pv.fail(err)
		return
	}
	if err := os.WriteFile(filepath.Join(s.paths.home, previewMarkerFile), []byte(pv.Owner+"/"+pv.Repo+"\n"), 0o600); err != nil {
		pv.fail(err)
		return
	}

	settings := SettingsFor(pv.Owner, pv.Repo)
	release := releaseDir(s.paths, pv.SHA+"@"+strconv.FormatInt(time.Now().UnixNano(), 10))
	job := deployJob{Owner: pv.Owner, Repo: pv.Repo, SHA: pv.SHA, PRID: pv.PRID}
	if _, err := buildRelease(ctx, job, s.paths, release, settings); err != nil {
		pv.fail(err)
		return
	}
	if err := swapSymlink(s.paths.current, release); err != nil {
		pv.fail(err)
		return
	}

	// A copy of the data as it is now, taken the way a snapshot is — safe
	// while the live app runs — so the preview shows real records and
	// whatever it writes stays with it. An app without data yet gets none.
	dataDir := filepath.Join(s.paths.home, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		pv.fail(err)
		return
	}
	pv.mu.Lock()
	pv.dataNote = "empty"
	pv.mu.Unlock()
	if AppDataEnabled() {
		if name, err := CreateSnapshot(ctx, pv.Owner, pv.Repo, "preview"); err == nil {
			liveData, _ := appDataForStart(pv.Owner, pv.Repo)
			if src, err := resolveSnapshot(liveData, name); err == nil {
				if err := copyFile(src, filepath.Join(dataDir, appDataDBName)); err != nil {
					pv.fail(err)
					return
				}
				pv.mu.Lock()
				pv.dataNote = "copied"
				pv.mu.Unlock()
			}
		}
	}

	appEnv, envVer, err := LoadAppEnv(pv.Owner, pv.Repo)
	if err != nil {
		pv.fail(err)
		return
	}
	s.mu.Lock()
	startErr := s.startLocked(settings, appEnv, envVer, dataDir)
	s.mu.Unlock()
	if startErr != nil {
		pv.fail(startErr)
		return
	}
	// Answering before it is offered: a link to a process still importing
	// its modules is a link to an error page.
	if err := waitHealthy(s.paths.socket, settings); err != nil {
		pv.fail(err)
		return
	}
	pv.mu.Lock()
	pv.building = false
	pv.startedAt = time.Now()
	pv.mu.Unlock()
	pv.lastHit.Store(time.Now().Unix())
	log.Info("company: preview of %s/%s is up at %s", pv.Owner, pv.Repo, previewURL(pv.Owner, pv.Repo))
}

// StopPreview ends an app's preview and removes what it built. Nothing to
// stop is not an error.
func StopPreview(owner, repo string) error {
	pv, ok := previewOf(owner, repo)
	if !ok {
		return nil
	}
	pv.mu.Lock()
	if pv.building {
		pv.mu.Unlock()
		return userKeyError("company.err.preview_building")
	}
	pv.mu.Unlock()
	s := previewSupervisor(owner, repo)
	s.mu.Lock()
	err := s.stopLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	stopBroker(owner, previewRepo(repo))
	previews.Delete(appRegistryKey(owner, repo))
	return os.RemoveAll(s.paths.home)
}

// stopPreviewForPR ends the preview of the app a decided request was for.
func stopPreviewForPR(pr *issues_model.PullRequest) {
	owner, repo, _, ok := parseDeployBranchName(pr.HeadBranch)
	if !ok {
		return
	}
	if pv, ok := previewOf(owner, repo); ok && pv.PRID == pr.ID {
		if err := StopPreview(owner, repo); err != nil {
			log.Error("company: stopping the preview of %s/%s: %v", owner, repo, err)
		}
	}
}

// StartPreviewSweeper stops previews nobody has opened for a while, and at
// boot removes what a previous Gitea's previews left behind: they are not
// in any state file, so nothing else would.
func StartPreviewSweeper() {
	cleanupPreviewHomes()
	ctx := graceful.GetManager().ShutdownContext()
	go func() {
		ticker := time.NewTicker(previewSweep)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				previews.Range(func(_, v any) bool {
					pv, ok := v.(*appPreview)
					if !ok {
						return true
					}
					if time.Since(time.Unix(pv.lastHit.Load(), 0)) > previewIdle {
						if err := StopPreview(pv.Owner, pv.Repo); err != nil {
							log.Error("company: idle preview of %s/%s: %v", pv.Owner, pv.Repo, err)
						}
					}
					return true
				})
			}
		}
	}()
}

func cleanupPreviewHomes() {
	if setting.AppDataPath == "" {
		return
	}
	homes, _ := filepath.Glob(filepath.Join(setting.AppDataPath, "company-apps", "*", previewMarkerFile))
	for _, marker := range homes {
		if err := os.RemoveAll(filepath.Dir(marker)); err != nil {
			log.Warn("company: removing a left-over preview at %s: %v", filepath.Dir(marker), err)
		}
	}
}

// AppPreviewProxy serves /apps/_preview/{owner}/{repo}/... to administrators.
// Mounted beside AppProxy (company/routes.go); the access mode, guard and
// metrics of the live app do not apply — this is one administrator looking
// at a copy — and a request here touches nothing the live app can see.
func AppPreviewProxy(ctx *gitea_context.Context) {
	if !ctx.IsSigned || ctx.Doer == nil || !ctx.Doer.IsAdmin {
		ctx.NotFound(nil)
		return
	}
	pv, ok := previewOf(ctx.PathParam("owner"), ctx.PathParam("repo"))
	if !ok {
		appUnavailable(ctx, http.StatusNotFound, AppRef{}, "company.preview.gate_none", "", nil)
		return
	}
	pv.mu.Lock()
	building, failed := pv.building, pv.err != ""
	pv.mu.Unlock()
	if building {
		appUnavailable(ctx, http.StatusServiceUnavailable, AppRef{}, "company.preview.gate_building", "", nil)
		return
	}
	if failed || !IsAppRunning(pv.Owner, previewRepo(pv.Repo)) {
		appUnavailable(ctx, http.StatusServiceUnavailable, AppRef{}, "company.preview.gate_failed", "", nil)
		return
	}
	if location, ok := rootRedirect(requestPath(ctx.Req), ctx.Req.URL.RawQuery, previewMountSegments); ok {
		ctx.Resp.Header().Set("Location", location)
		ctx.Resp.WriteHeader(http.StatusPermanentRedirect)
		return
	}
	if !guardBodyLimit(ctx) {
		return
	}
	pv.lastHit.Store(time.Now().Unix())
	ref := AppRef{Owner: pv.Owner, Repo: pv.Repo} // the live app's settings decide headers and downloads
	s := previewSupervisor(pv.Owner, pv.Repo)
	appProxyWith(appKey(pv.Owner, previewRepo(pv.Repo)), ref, s.paths.socket, previewURL(pv.Owner, pv.Repo), previewMountSegments).ServeHTTP(ctx.Resp, ctx.Req)
}

// previewMountSegments is "apps", "_preview", owner, repo.
const previewMountSegments = 4

// deployRequestTarget resolves an administrator's action on an open
// request to the app it is for, or answers the request itself.
func deployRequestTarget(ctx *gitea_context.Context) (owner, repo string, pr *issues_model.PullRequest, ok bool) {
	if ctx.Doer == nil || !ctx.Doer.IsAdmin {
		ctx.NotFound(nil)
		return "", "", nil, false
	}
	pr, err := issues_model.GetPullRequestByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		ctx.NotFound(err)
		return "", "", nil, false
	}
	owner, repo, ok = verifyDeployRequestPR(ctx, pr, false)
	if !ok {
		ctx.NotFound(nil)
		return "", "", nil, false
	}
	return owner, repo, pr, true
}

// DeployRequestPreview builds and starts the preview of a request's
// snapshot. Mounted at POST /company/deploy-request/{id}/preview.
func DeployRequestPreview(ctx *gitea_context.Context) {
	owner, repo, pr, ok := deployRequestTarget(ctx)
	if !ok {
		return
	}
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, pr.BaseRepo)
	if err != nil {
		ctx.ServerError("RepositoryFromRequestContextOrOpen", err)
		return
	}
	commit, err := gitRepo.GetBranchCommit(ctx, pr.HeadBranch)
	if err != nil {
		ctx.Flash.Error(ctx.Locale.TrString("company.err.version_gone"))
		ctx.Redirect(fmt.Sprintf("%s/pulls/%d", pr.BaseRepo.Link(), pr.Issue.Index))
		return
	}
	if err := StartPreview(owner, repo, pr.ID, commit.ID.String()); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	}
	ctx.Redirect(fmt.Sprintf("%s/pulls/%d", pr.BaseRepo.Link(), pr.Issue.Index))
}

// DeployRequestPreviewStop ends it. Mounted at POST .../preview/stop.
func DeployRequestPreviewStop(ctx *gitea_context.Context) {
	owner, repo, pr, ok := deployRequestTarget(ctx)
	if !ok {
		return
	}
	if err := StopPreview(owner, repo); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	}
	ctx.Redirect(fmt.Sprintf("%s/pulls/%d", pr.BaseRepo.Link(), pr.Issue.Index))
}

// DeployRequestPreviewStatus is what the page polls while a build runs.
// Mounted at GET .../preview/status.
func DeployRequestPreviewStatus(ctx *gitea_context.Context) {
	owner, repo, _, ok := deployRequestTarget(ctx)
	if !ok {
		return
	}
	st := PreviewStatusOf(owner, repo)
	ctx.JSON(http.StatusOK, map[string]any{"state": st.State, "error": st.Error, "url": st.URL})
}
