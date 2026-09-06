// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"strings"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/modules/timeutil"
	"gitea.dev/services/context"
)

// latestDeployRequest finds the most recent deploy-request PR for the
// given department repo (owner/name) — a same-repo PR on the central
// deploy repo, whose branch DeployPost named deploy/<owner>/<name>/<ts>
// (see deployBranchName/parseDeployBranchName, company/deploy.go). Used
// by DeployStatus and Submitted below; returns (nil, nil) if there's
// never been one.
func latestDeployRequest(ctx *context.Context, owner, name string) (*issues_model.PullRequest, error) {
	central, err := centralDeployRepo(ctx)
	if err != nil {
		return nil, err
	}

	var pr issues_model.PullRequest
	has, err := db.GetEngine(ctx).
		Join("INNER", "issue", "issue.id = pull_request.issue_id").
		Where("pull_request.base_repo_id = ?", central.ID).
		And("pull_request.head_repo_id = ?", central.ID).
		And("pull_request.head_branch LIKE ?", deployBranchPrefix(owner, name)+"%").
		Desc("issue.created_unix").
		Get(&pr)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, nil
	}
	if err := pr.LoadIssue(ctx); err != nil {
		return nil, err
	}
	// LoadIssue doesn't load the issue's own repo, but templates.
	// {{.Issue.HTMLURL}} (base/head_opengraph, company/submitted.tmpl) needs
	// it to build the link.
	if err := pr.Issue.LoadRepo(ctx); err != nil {
		return nil, err
	}
	return &pr, nil
}

// deployRequestStatus is the outcome + the one date that matters for it —
// when it was opened (pending), merged (approved), or closed (rejected/
// cancelled) — shared by DeployStatus (JSON, for the repo code page's
// badge) and DeployForm (rendered directly, for the /deploy page itself).
type deployRequestStatus struct {
	Status string
	Date   timeutil.TimeStamp
}

// deployStatusFor computes pr's outcome — pending (open PR into the
// central deploy repo), approved (merged), cancelled (closed by the
// requester themselves, see CancelDeployRequest), or rejected (closed by
// an admin, not merged) — and the date that outcome happened.
func deployStatusFor(ctx *context.Context, pr *issues_model.PullRequest) (*deployRequestStatus, error) {
	if pr.Issue.IsClosed {
		if pr.HasMerged {
			if runtime := runtimeStatusFor(pr); runtime != nil {
				return runtime, nil
			}
			return &deployRequestStatus{Status: "approved", Date: pr.MergedUnix}, nil
		}
		cancelled, err := wasCancelledByRequester(ctx, pr.Issue.ID)
		if err != nil {
			return nil, err
		}
		status := "rejected"
		if cancelled {
			status = "cancelled"
		}
		return &deployRequestStatus{Status: status, Date: pr.Issue.ClosedUnix}, nil
	}
	return &deployRequestStatus{Status: "pending", Date: pr.Issue.CreatedUnix}, nil
}

// runtimeStatusFor replaces the flat "approved" (= merged) with what the
// app is *actually* doing now, or nil when there's nothing better to say.
//
// The mapping is done here in Go rather than in the template on purpose:
// staff must never see "rolled_back" as its own word. To a non-developer it
// reads like a distinct fourth outcome they're expected to act on, when the
// only thing they need to know is that the deploy did not take. Everything
// that isn't running collapses into one failure word, and the *reason* is
// what carries the detail — in plain language, elsewhere on the page.
//
// Returns nil (falling back to "approved") when this PR isn't the one that
// produced the live state, so an old request in a list doesn't claim
// credit for a later deploy's outcome.
func runtimeStatusFor(pr *issues_model.PullRequest) *deployRequestStatus {
	owner, repo, _, ok := parseDeployBranchName(pr.HeadBranch)
	if !ok {
		return nil
	}
	st := LoadAppState(owner, repo)
	if st.PRID != pr.ID || st.UpdatedAt == 0 {
		return nil // predates this feature, or belongs to a different request
	}

	var status string
	switch st.Actual {
	case AppStateQueued, AppStateBuilding, AppStateActivating:
		status = "deploying"
	case AppStateRunning:
		status = "deployed"
	case AppStateFailed:
		status = "deploy_failed"
	default:
		// stopped/suspended are deliberate, not deploy outcomes — the badge
		// answers "did my deploy work?", and the app's on/off state is shown
		// by its own control panel instead.
		return nil
	}
	return &deployRequestStatus{Status: status, Date: timeutil.TimeStamp(st.UpdatedAt)}
}

// rejectionReasons returns every comment on pr's issue except the automatic
// AI review (posted on every submission regardless of outcome —
// postAIReviewComment) — that one isn't a human's rejection reason and
// would be misleading under a "why was this rejected" heading specifically.
// Never guesses which single comment "is" the reason (an admin can type as
// many as they like natively); shared by DeployForm (deploy.go, full list on
// the /deploy page) and DeployStatus below (summarized into the repo home
// page badge's tooltip). DeployRequestFiles (company/deployrequestfiles.go)
// has its own, unfiltered comment list — it isn't making the same claim, so
// it still shows the AI review too.
func rejectionReasons(ctx *context.Context, pr *issues_model.PullRequest) ([]*issues_model.Comment, error) {
	comments, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{
		IssueID: pr.Issue.ID,
		Type:    issues_model.CommentTypeComment,
	})
	if err != nil {
		return nil, err
	}
	if err := comments.LoadPosters(ctx); err != nil {
		return nil, err
	}
	reasons := make([]*issues_model.Comment, 0, len(comments))
	for _, c := range comments {
		if !strings.HasPrefix(c.Content, aiReviewCommentMarker) {
			reasons = append(reasons, c)
		}
	}
	return reasons, nil
}

// DeployStatus reports this repo's most recent "Deploy Request" outcome —
// see deployStatusFor — or null if it's never had one. Fetched
// client-side (custom/templates/repo/view_content.tmpl,
// web_src/js/features/company-deploy-status.ts) rather than computed in
// the native repo.Home handler, to avoid a core-file touch there.
// Mounted at /{owner}/{repo}/deploy-status alongside Gitea's own repo
// routes (routers/web/web.go), so ctx.Repo.Repository is already resolved.
func DeployStatus(ctx *context.Context) {
	pr, err := latestDeployRequest(ctx, ctx.Repo.Repository.OwnerName, ctx.Repo.Repository.Name)
	if err != nil {
		ctx.ServerError("latestDeployRequest", err)
		return
	}
	if pr == nil {
		ctx.JSON(http.StatusOK, map[string]any{"status": nil})
		return
	}
	s, err := deployStatusFor(ctx, pr)
	if err != nil {
		ctx.ServerError("deployStatusFor", err)
		return
	}
	resp := map[string]any{"status": s.Status, "date": s.Date.AsTime().Unix()}
	// Reason is only meaningful (and only fetched) for "rejected" — the
	// repo home page badge shows it as a tooltip so staff don't have to
	// open /deploy just to find out why, see company-deploy-status.ts.
	if s.Status == "rejected" {
		reasons, err := rejectionReasons(ctx, pr)
		if err != nil {
			ctx.ServerError("rejectionReasons", err)
			return
		}
		if len(reasons) > 0 {
			texts := make([]string, len(reasons))
			for i, c := range reasons {
				texts[i] = c.Poster.Name + ": " + c.Content
			}
			resp["reason"] = strings.Join(texts, "\n\n")
		}
	}
	ctx.JSON(http.StatusOK, resp)
}
