// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"strings"
	"time"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
)

// Auto-approval that depends on the moment a request was submitted is not
// automatic: a submission whose browser went away, or a restart during a
// countdown, left requests nobody would ever decide unless someone noticed.
// The sweeper picks up every open request the AI has not decided yet.

const autoApproveSweepInterval = time.Minute

// autoApproveDecisionTitle starts every card that settles a request's
// auto-approval: approved, held, or stopped.
const autoApproveDecisionTitle = "#### AI 검토 · 자동 승인"

func runAutoApproveSweeper() {
	ctx := graceful.GetManager().ShutdownContext()
	ticker := time.NewTicker(autoApproveSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepAutoApprove(ctx)
		}
	}
}

func sweepAutoApprove(ctx context.Context) {
	defer recoverBackground("auto-approval sweep")
	if AutoApproveDelegate(ctx) == nil || PlatformAIUnavailableReason(ctx) != "" {
		return
	}
	owner, name, err := centralDeployOwnerName()
	if err != nil {
		return
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, name)
	if err != nil {
		return
	}
	centralOwner, err := user_model.GetUserByID(ctx, central.OwnerID)
	if err != nil {
		return
	}
	prs, err := issues_model.GetUnmergedPullRequestsByBaseInfo(ctx, central.ID, central.DefaultBranch)
	if err != nil {
		log.Error("company: auto-approval sweep: %v", err)
		return
	}
	for _, pr := range prs {
		deptOwner, deptName, ok := verifyDeployRequestPR(ctx, pr, false)
		if !ok || autoApproveStatusFor(pr.ID).Phase != "" || hasAutoApproveDecision(ctx, pr.Issue.ID) {
			continue
		}
		log.Info("company: auto-approval sweep: reviewing deploy request #%d for %s/%s", pr.Index, deptOwner, deptName)
		ScheduleDeployAIReview(central, centralOwner, pr.ID, deptOwner, deptName)
	}
}

// hasAutoApproveDecision reports whether the AI already approved, held or was
// stopped on this request — then it is a person's to decide, not the sweeper's.
func hasAutoApproveDecision(ctx context.Context, issueID int64) bool {
	comments, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{IssueID: issueID, Type: issues_model.CommentTypeComment})
	if err != nil {
		return true // unsure: leave it to a person rather than review it twice
	}
	for _, c := range comments {
		if strings.HasPrefix(c.Content, aiReviewCommentMarker) && strings.Contains(c.Content, autoApproveDecisionTitle) {
			return true
		}
	}
	return false
}
