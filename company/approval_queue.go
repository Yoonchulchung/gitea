// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	gocontext "context"
	"strings"

	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/timeutil"
	"gitea.dev/services/context"
)

// The central repository's pull request list is the approval queue, and a
// queue is a table: one request per row, the same three things on every row —
// what was asked for, who asked, and which department they are in.
//
// Gitea's own list is a flex layout of chips and sentences, which is right for
// a repository somebody develops in and wrong here: an administrator scanning
// twenty requests is comparing them, and things that are compared belong in
// columns. See docs/company/central-repo-ui.md.

// ApprovalQueueRow is the part of a request that is not on the issue itself.
//
// A Deploy Request is opened by the platform's own account (company/deploy.go),
// so the issue's poster names the platform on every row; who actually asked is
// encoded in the branch name, along with which department they asked for.
type ApprovalQueueRow struct {
	Requester *user_model.User
	Dept      string
	App       string
	// Approver is who merged it, which is who approved it — never the issue's
	// poster, which is the platform on every row. Nil until it is approved.
	Approver   *user_model.User
	ApprovedAt timeutil.TimeStamp
	// Canceller is who ended it without approving, and Cancelled says it was
	// withdrawn rather than turned down. Both nil/false while it is open or
	// approved.
	Canceller   *user_model.User
	CancelledAt timeutil.TimeStamp
	Cancelled   bool
}

// SetApprovalQueueData marks the central repository's pull list as the
// approval queue and gives the template a way to resolve one row.
//
// A function in ctx.Data rather than a prepared slice: the list this runs
// before has not been queried yet — it is the handler that follows — so there
// is nothing to prepare. The template calls this per row instead, and each
// call is one cached user lookup.
func SetApprovalQueueData(ctx *context.Context) {
	owner, name, err := centralDeployOwnerName()
	if err != nil || ctx.Repo.Repository == nil ||
		!strings.EqualFold(ctx.Repo.Repository.OwnerName, owner) || !strings.EqualFold(ctx.Repo.Repository.Name, name) {
		return
	}
	ctx.Data["CompanyIsApprovalQueue"] = true

	// Requesters repeat across a page of requests — the same department sends
	// several — so each account is read once however many rows it holds.
	seen := map[int64]*user_model.User{}
	person := func(id int64) *user_model.User {
		if u, cached := seen[id]; cached {
			return u
		}
		u, err := user_model.GetUserByID(ctx, id)
		if err != nil {
			// Cached as nil rather than failing the page: the account was
			// deleted since, and the request is still a request.
			u = nil
		}
		seen[id] = u
		return u
	}

	ctx.Data["CompanyRequestOf"] = func(pr *issues_model.PullRequest) *ApprovalQueueRow {
		if pr == nil {
			return nil
		}
		dept, app, requesterID, ok := parseDeployBranchName(pr.HeadBranch)
		if !ok {
			return nil // a pull request that is not a Deploy Request; the row falls back
		}
		row := &ApprovalQueueRow{Dept: dept, App: app, Requester: person(requesterID)}
		switch {
		case pr.HasMerged:
			row.Approver, row.ApprovedAt = person(pr.MergerID), pr.MergedUnix
		case pr.Issue != nil && pr.Issue.IsClosed:
			// Who ended it, from the record of the ending itself: the comment
			// CancelDeployRequest writes when the department withdraws
			// (company/deployrequests.go), or the close event when an
			// administrator turns it down. One query per closed row, and only
			// on a page an administrator opened.
			if c := lastDecisionComment(ctx, pr.Issue.ID); c != nil {
				row.Canceller, row.CancelledAt = person(c.PosterID), c.CreatedUnix
				row.Cancelled = strings.HasPrefix(c.Content, cancelCommentPrefix)
			}
		}
		return row
	}
}

// lastDecisionComment is the comment that ended an unapproved request: the
// withdrawal if there was one, otherwise whoever closed it.
//
// The withdrawal wins where both exist — CancelDeployRequest writes its
// comment and then closes, so the close event is the machinery and the
// comment is the decision.
func lastDecisionComment(ctx gocontext.Context, issueID int64) *issues_model.Comment {
	comments, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{
		IssueID: issueID,
		Type:    issues_model.CommentTypeUndefined,
	})
	if err != nil {
		log.Error("company: reading how deploy request issue %d ended: %v", issueID, err)
		return nil
	}
	var closed *issues_model.Comment
	for _, c := range comments {
		switch {
		case c.Type == issues_model.CommentTypeComment && strings.HasPrefix(c.Content, cancelCommentPrefix):
			return c
		case c.Type == issues_model.CommentTypeClose:
			closed = c
		}
	}
	return closed
}
