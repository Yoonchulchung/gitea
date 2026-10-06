// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	gitea_context "gitea.dev/services/context"
)

// An approval the AI decided is not merged on the spot: the request page
// counts down first, and an administrator can stop it. Kept in memory only —
// a restart during the wait forgets it, and the request then waits for a
// person, which is the safe way to forget.

const (
	autoApprovePhaseReviewing = "reviewing"
	autoApprovePhaseWaiting   = "waiting"
)

const settingKeyAutoApproveDelay = "company.deploy.auto_approve_delay"

// defaultAutoApproveDelay is long enough to read the card and press stop.
const defaultAutoApproveDelay = 60 * time.Second

type autoApproveRun struct {
	phase   string
	mergeAt time.Time
	stop    chan struct{}
	stopper string // who stopped it, once stopped
}

var autoApproveRuns sync.Map // pull request id → *autoApproveRun

var autoApproveRunsMu sync.Mutex // phase changes and stopping

// startAutoApproveRun registers a review for prID; fresh is false when one is
// already registered, and the caller must not start a second.
func startAutoApproveRun(prID int64) (run *autoApproveRun, fresh bool) {
	v, loaded := autoApproveRuns.LoadOrStore(prID, &autoApproveRun{phase: autoApprovePhaseReviewing, stop: make(chan struct{})})
	run, _ = v.(*autoApproveRun)
	return run, !loaded
}

// recoverBackground keeps a bug in background AI work from taking Gitea down:
// a panic in a goroutine ends the whole process, every app with it.
func recoverBackground(what string, args ...any) {
	if r := recover(); r != nil {
		log.Error("company: %s panicked: %v\n%s", fmt.Sprintf(what, args...), r, debug.Stack())
	}
}

func finishAutoApproveRun(prID int64) { autoApproveRuns.Delete(prID) }

func loadAutoApproveRun(prID int64) (*autoApproveRun, bool) {
	v, ok := autoApproveRuns.Load(prID)
	if !ok {
		return nil, false
	}
	run, ok := v.(*autoApproveRun)
	return run, ok
}

// waitBeforeMerge counts down, and reports false when the merge must not go
// ahead: stopped by an administrator, or the platform is shutting down.
func (run *autoApproveRun) waitBeforeMerge(ctx context.Context, delay time.Duration) (stoppedBy string, ok bool) {
	autoApproveRunsMu.Lock()
	if run.stopper != "" {
		autoApproveRunsMu.Unlock()
		return run.stopper, false
	}
	run.phase, run.mergeAt = autoApprovePhaseWaiting, time.Now().Add(delay)
	autoApproveRunsMu.Unlock()

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return "", true
	case <-run.stop:
		return run.stopper, false
	case <-ctx.Done():
		return "", false
	}
}

// stopAutoApprove stops the run for prID; false when there is none to stop.
func stopAutoApprove(prID int64, who string) bool {
	run, ok := loadAutoApproveRun(prID)
	if !ok {
		return false
	}
	autoApproveRunsMu.Lock()
	defer autoApproveRunsMu.Unlock()
	if run.stopper != "" {
		return false
	}
	run.stopper = who
	close(run.stop)
	return true
}

// AutoApproveStatus is what the request page shows about a running auto-approval.
type AutoApproveStatus struct {
	Phase       string `json:"phase"` // "", reviewing, waiting, stopped
	MergeAt     int64  `json:"mergeAt,omitempty"`
	SecondsLeft int    `json:"secondsLeft,omitempty"`
}

func autoApproveStatusFor(prID int64) AutoApproveStatus {
	run, ok := loadAutoApproveRun(prID)
	if !ok {
		return AutoApproveStatus{}
	}
	autoApproveRunsMu.Lock()
	defer autoApproveRunsMu.Unlock()
	if run.stopper != "" {
		return AutoApproveStatus{Phase: "stopped"}
	}
	s := AutoApproveStatus{Phase: run.phase}
	if run.phase == autoApprovePhaseWaiting {
		s.MergeAt = run.mergeAt.Unix()
		s.SecondsLeft = max(0, int(time.Until(run.mergeAt).Seconds()+0.5))
	}
	return s
}

// AutoApproveDelay is how long an AI approval waits before it merges.
func AutoApproveDelay(ctx context.Context) time.Duration {
	loadPlatformSettings(ctx)
	platformSettingsMu.RLock()
	defer platformSettingsMu.RUnlock()
	if autoApproveDelaySeconds < 0 {
		return defaultAutoApproveDelay
	}
	return time.Duration(autoApproveDelaySeconds) * time.Second
}

func parseAutoApproveDelay(raw string) int {
	if raw == "" {
		return -1 // never set: the default
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return -1
	}
	return v
}

// DeployRequestAutoApproveStatus is GET /company/deploy-request/{id}/auto-approve.
func DeployRequestAutoApproveStatus(ctx *gitea_context.Context) {
	if !ctx.Doer.IsAdmin {
		ctx.NotFound(nil)
		return
	}
	ctx.JSON(http.StatusOK, autoApproveStatusFor(ctx.PathParamInt64("id")))
}

// DeployRequestAutoApproveStop is POST /company/deploy-request/{id}/auto-approve/stop.
func DeployRequestAutoApproveStop(ctx *gitea_context.Context) {
	if !ctx.Doer.IsAdmin {
		ctx.NotFound(nil)
		return
	}
	pr, err := issues_model.GetPullRequestByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		ctx.NotFound(err)
		return
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		ctx.ServerError("LoadBaseRepo", err)
		return
	}
	if stopAutoApprove(pr.ID, ctx.Doer.Name) {
		ctx.Flash.Success(ctx.Locale.TrString("company.review.auto_approve_stopped"))
	} else {
		ctx.Flash.Info(ctx.Locale.TrString("company.review.auto_approve_nothing_to_stop"))
	}
	ctx.Redirect(pr.BaseRepo.Link() + "/pulls/" + strconv.FormatInt(pr.Index, 10))
}

// DeployRequestAutoApproveStart is POST /company/deploy-request/{id}/auto-approve/start:
// the review a request should have had — missed, or lost to a restart during
// its countdown — run again.
func DeployRequestAutoApproveStart(ctx *gitea_context.Context) {
	if !ctx.Doer.IsAdmin {
		ctx.NotFound(nil)
		return
	}
	pr, err := issues_model.GetPullRequestByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		ctx.NotFound(err)
		return
	}
	deptOwner, deptName, ok := verifyDeployRequestPR(ctx, pr, false)
	if !ok {
		ctx.NotFound(nil)
		return
	}
	back := pr.BaseRepo.Link() + "/pulls/" + strconv.FormatInt(pr.Index, 10)
	if autoApproveStatusFor(pr.ID).Phase != "" {
		ctx.Flash.Info(ctx.Locale.TrString("company.review.auto_approve_running"))
		ctx.Redirect(back)
		return
	}
	if AutoApproveDelegate(ctx) == nil || PlatformAIUnavailableReason(ctx) != "" {
		ctx.Flash.Error(ctx.Locale.TrString("company.review.auto_approve_unavailable"))
		ctx.Redirect(back)
		return
	}
	centralOwner, err := user_model.GetUserByID(ctx, pr.BaseRepo.OwnerID)
	if err != nil {
		ctx.ServerError("GetUserByID", err)
		return
	}
	ScheduleDeployAIReview(pr.BaseRepo, centralOwner, pr.ID, deptOwner, deptName)
	ctx.Flash.Success(ctx.Locale.TrString("company.review.auto_approve_started"))
	ctx.Redirect(back)
}
