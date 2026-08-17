// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
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

// DeployStatus reports this repo's most recent "Deploy Request" outcome —
// pending (open PR into the central deploy repo), approved (merged),
// cancelled (closed by the requester themselves, see CancelDeployRequest),
// or rejected (closed by an admin, not merged) — or null if it's never had
// one. Fetched client-side (custom/templates/repo/view_content.tmpl,
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

	status := "pending"
	if pr.Issue.IsClosed {
		switch {
		case pr.HasMerged:
			status = "approved"
		default:
			cancelled, err := wasCancelledByRequester(ctx, pr.Issue.ID)
			if err != nil {
				ctx.ServerError("wasCancelledByRequester", err)
				return
			}
			if cancelled {
				status = "cancelled"
			} else {
				status = "rejected"
			}
		}
	}
	ctx.JSON(http.StatusOK, map[string]any{"status": status})
}
