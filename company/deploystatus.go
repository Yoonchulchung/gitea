// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"

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
	ctx.JSON(http.StatusOK, map[string]any{"status": s.Status, "date": s.Date.AsTime().Unix()})
}
