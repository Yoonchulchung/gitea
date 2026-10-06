// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"sync"
	"time"

	gitea_context "gitea.dev/services/context"
)

// A code review takes tens of seconds; the request page says it is under way
// rather than looking idle, and refreshes when the comments land. Memory only:
// a restart mid-review loses the progress, not the comments already posted.

// codeReviewFailureShown is how long a failed review stays on the page.
const codeReviewFailureShown = 5 * time.Minute

type codeReviewRun struct {
	mu          sync.Mutex
	startedAt   time.Time
	by          string // who pressed the button; "" for the review on submission
	done, total int
	failed      string
	finishedAt  time.Time
}

var codeReviewRuns sync.Map // pull request id → *codeReviewRun

// startCodeReview registers a review of prID; fresh is false while one is
// already running, and the caller must not start a second.
func startCodeReview(prID int64, by string) (run *codeReviewRun, fresh bool) {
	if run, ok := loadCodeReview(prID); ok {
		run.mu.Lock()
		running := run.finishedAt.IsZero()
		run.mu.Unlock()
		if running {
			return run, false
		}
	}
	run = &codeReviewRun{startedAt: time.Now(), by: by}
	codeReviewRuns.Store(prID, run)
	return run, true
}

func loadCodeReview(prID int64) (*codeReviewRun, bool) {
	v, ok := codeReviewRuns.Load(prID)
	if !ok {
		return nil, false
	}
	run, ok := v.(*codeReviewRun)
	return run, ok
}

func (r *codeReviewRun) progress(done, total int) {
	r.mu.Lock()
	r.done, r.total = done, total
	r.mu.Unlock()
}

// finish ends the run: a success simply goes (the comments are the result), a
// failure stays visible for a while so whoever is looking learns why.
func (r *codeReviewRun) finish(prID int64, failure string) {
	r.mu.Lock()
	r.finishedAt, r.failed = time.Now(), failure
	r.mu.Unlock()
	if failure == "" {
		codeReviewRuns.CompareAndDelete(prID, r)
	}
}

// CodeReviewStatus is what the request page shows about a review in progress.
type CodeReviewStatus struct {
	Phase     string `json:"phase"` // "", running, failed
	StartedAt int64  `json:"startedAt,omitempty"`
	By        string `json:"by,omitempty"`
	Done      int    `json:"done,omitempty"`
	Total     int    `json:"total,omitempty"`
	Failed    string `json:"failed,omitempty"`
}

func codeReviewStatusFor(prID int64) CodeReviewStatus {
	run, ok := loadCodeReview(prID)
	if !ok {
		return CodeReviewStatus{}
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if !run.finishedAt.IsZero() {
		if run.failed == "" || time.Since(run.finishedAt) > codeReviewFailureShown {
			return CodeReviewStatus{}
		}
		return CodeReviewStatus{Phase: "failed", Failed: run.failed}
	}
	return CodeReviewStatus{Phase: "running", StartedAt: run.startedAt.Unix(), By: run.by, Done: run.done, Total: run.total}
}

// DeployRequestAIReviewStatus is GET /company/deploy-request/{id}/ai-review.
func DeployRequestAIReviewStatus(ctx *gitea_context.Context) {
	if !ctx.Doer.IsAdmin {
		ctx.NotFound(nil)
		return
	}
	ctx.JSON(http.StatusOK, codeReviewStatusFor(ctx.PathParamInt64("id")))
}
