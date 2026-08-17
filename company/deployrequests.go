// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"net/http"
	"strings"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	webrepo "gitea.dev/routers/web/repo"
	"gitea.dev/services/context"
	issue_service "gitea.dev/services/issue"
)

const tplDeployRequests templates.TplName = "company/deploy_requests"

// cancelCommentPrefix marks a comment CancelDeployRequest itself posted —
// the only signal that distinguishes "the requester withdrew this" from
// "an admin declined it" once both are just a closed, unmerged PR. No
// schema change for a dedicated flag; reusing the comment Gitea's own PR
// page already shows natively keeps this a plain read, see
// wasCancelledByRequester below.
const cancelCommentPrefix = "Cancelled: "

// wasCancelledByRequester reports whether issueID was closed via
// CancelDeployRequest (as opposed to an admin declining the PR natively) —
// used by DeployRequests below and DeployStatus (company/deploystatus.go)
// to show "Cancelled" instead of "Rejected".
func wasCancelledByRequester(ctx *context.Context, issueID int64) (bool, error) {
	comments, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{
		IssueID: issueID,
		Type:    issues_model.CommentTypeComment,
	})
	if err != nil {
		return false, err
	}
	for _, c := range comments {
		if strings.HasPrefix(c.Content, cancelCommentPrefix) {
			return true, nil
		}
	}
	return false, nil
}

// deployRequestView is one PR translated into plain language — no PR
// number, no branch names, no diff link. Just "who asked for what, and
// where it stands" — see docs/company/architecture.md on why the admin's
// PR review screen and this list are deliberately different surfaces.
// Poster/Repo are the real model objects (not pre-flattened strings) so the
// template can use Gitea's own helpers (ctx.AvatarUtils.Avatar, etc.) and
// stay visually consistent with the rest of the site.
type deployRequestView struct {
	ID          int64 // pull_request.id — DeployRequestFiles/CancelDeployRequest key off this, not an issue index (the issue lives on the central repo, not any repo the viewer necessarily has access to)
	Repo        *repo_model.Repository
	Title       string
	Poster      *user_model.User
	CreatedUnix any
	Rejected    bool // only meaningful when listed under "Deployed" — closed by an admin, not merged
	Cancelled   bool // only meaningful when listed under "Deployed" — withdrawn by the requester, not merged
	CanCancel   bool // only meaningful when listed under "Requested" — poster or admin
}

// DeployRequests lists every department's deploy requests, split the same
// way /pulls splits Open/Closed — "배포 요청됨" (still open, awaiting admin
// review) and "배포함" (closed: either merged = actually deployed, or
// rejected). Visible to any member of the org, not just admins, so a team
// can see what its colleagues have submitted without needing PR/branch
// vocabulary or admin access. Reuses the same underlying PR data the admin
// reviews natively (docs/company/architecture.md); this is a second,
// friendlier view onto it, not a second workflow.
//
// Mounted at /org/{org}/dashboard/deploy-requests, inside Gitea's own org
// group (routers/web/web.go) — so membership is already enforced by
// context.OrgAssignment(RequireMember: true) before this handler ever runs;
// org resolution/membership don't need to be repeated here.
func DeployRequests(ctx *context.Context) {
	org := ctx.Org.Organization

	central, err := centralDeployRepo(ctx)
	if err != nil {
		ctx.ServerError("centralDeployRepo", err)
		return
	}

	// Deploy-request PRs are same-repo PRs on the central deploy repo
	// (company/deploy.go's DeployPost) — which department repo each one
	// is "for" isn't HeadRepoID (that's central too), it's encoded in the
	// branch name (deployBranchName/parseDeployBranchName). Every branch
	// for a repo this org owns starts with "deploy/<org-name>/".
	var prs []*issues_model.PullRequest
	if err := db.GetEngine(ctx).
		Join("INNER", "issue", "issue.id = pull_request.issue_id").
		Where("pull_request.base_repo_id = ?", central.ID).
		And("pull_request.head_repo_id = ?", central.ID).
		And("pull_request.head_branch LIKE ?", "deploy/"+org.Name+"/%").
		Desc("issue.created_unix").
		Limit(200).
		Find(&prs); err != nil {
		ctx.ServerError("list deploy requests", err)
		return
	}

	requested := make([]*deployRequestView, 0, len(prs))
	deployed := make([]*deployRequestView, 0, len(prs))
	for _, pr := range prs {
		if err := pr.LoadIssue(ctx); err != nil {
			ctx.ServerError("LoadIssue", err)
			return
		}
		ownerName, repoName, requesterID, ok := parseDeployBranchName(pr.HeadBranch)
		if !ok {
			continue
		}
		repo, err := repo_model.GetRepositoryByOwnerAndName(ctx, ownerName, repoName)
		if err != nil {
			continue // repo renamed/deleted since — skip rather than fail the whole list
		}
		// pr.Issue.Poster is the central repo's own owner (see deploy.go's
		// DeployPost) — the real requester's identity only lives in the
		// branch name.
		requester, err := user_model.GetUserByID(ctx, requesterID)
		if err != nil {
			continue // requester account deleted since — skip rather than fail the whole list
		}
		view := &deployRequestView{
			ID:          pr.ID,
			Repo:        repo,
			Title:       pr.Issue.Title,
			Poster:      requester,
			CreatedUnix: pr.Issue.CreatedUnix,
		}
		if !pr.Issue.IsClosed {
			view.CanCancel = ctx.Doer.IsAdmin || requesterID == ctx.Doer.ID
			requested = append(requested, view)
			continue
		}
		if !pr.HasMerged {
			cancelled, err := wasCancelledByRequester(ctx, pr.Issue.ID)
			if err != nil {
				ctx.ServerError("wasCancelledByRequester", err)
				return
			}
			if cancelled {
				view.Cancelled = true
			} else {
				view.Rejected = true
			}
		}
		deployed = append(deployed, view)
	}

	ctx.Data["Title"] = "Deploy requests"
	ctx.Data["Org"] = org
	ctx.Data["Requested"] = requested
	ctx.Data["Deployed"] = deployed
	ctx.HTML(http.StatusOK, tplDeployRequests)
}

// CancelDeployRequest lets the original requester (or an admin) withdraw
// their own still-open deploy request, with a required reason recorded as
// a comment on the underlying PR — so the admin reviewing it natively
// still sees why it was pulled. Mounted at
// /company/deploy-request/{id}/cancel (company/routes.go).
func CancelDeployRequest(ctx *context.Context) {
	id := ctx.PathParamInt64("id")
	pr, err := issues_model.GetPullRequestByID(ctx, id)
	if err != nil {
		ctx.NotFound(err)
		return
	}
	if err := pr.LoadIssue(ctx); err != nil {
		ctx.ServerError("LoadIssue", err)
		return
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		ctx.ServerError("LoadBaseRepo", err)
		return
	}
	// pr.Issue.PosterID is the central repo's own owner (see deploy.go's
	// DeployPost) — the real requester's identity only lives in the branch
	// name (deployBranchName/parseDeployBranchName).
	_, _, requesterID, ok := parseDeployBranchName(pr.HeadBranch)
	if !ok {
		ctx.NotFound(nil)
		return
	}
	if !ctx.Doer.IsAdmin && requesterID != ctx.Doer.ID {
		ctx.NotFound(nil)
		return
	}
	if pr.Issue.IsClosed {
		ctx.HTTPError(http.StatusBadRequest, "already closed")
		return
	}

	reason := strings.TrimSpace(ctx.Req.FormValue("reason"))
	if reason == "" {
		ctx.HTTPError(http.StatusBadRequest, "reason required")
		return
	}

	if _, err := issue_service.CreateIssueComment(ctx, ctx.Doer, pr.BaseRepo, pr.Issue, cancelCommentPrefix+reason, nil); err != nil {
		ctx.ServerError("CreateIssueComment", err)
		return
	}
	if err := issue_service.CloseIssue(ctx, pr.Issue, ctx.Doer, ""); err != nil {
		ctx.ServerError("CloseIssue", err)
		return
	}

	org := ctx.Req.FormValue("org")
	ctx.Redirect(fmt.Sprintf("%s/org/%s/dashboard/deploy-requests", setting.AppSubURL, org))
}

// RepoCreateForOrg renders Gitea's own repo-creation page directly at
// /org/{org}/repo/create — deliberately not a redirect to
// /repo/create?org=N, so the address bar stays on the friendly,
// department-named URL. webrepo.Create only reads the owner from the
// ?org= query param (routers/web/repo/repo.go:144, ctx.FormInt64("org")),
// so that's injected onto the request before handing off. Mounted inside
// Gitea's own org group, so RequireMember is already enforced before this
// runs; the create page itself still separately checks CanCreateOrgRepo
// (team membership with create-repo rights, see docs/company/ui-gate.md)
// before actually letting the create form submit.
func RepoCreateForOrg(ctx *context.Context) {
	ctx.Req.URL.RawQuery = fmt.Sprintf("org=%d", ctx.Org.Organization.ID)
	webrepo.Create(ctx)
}

// RepoCreateForOrgPost handles the form submission from the page above.
// Unlike the GET above, no query-param injection is needed: the owner
// travels as the form's own hidden "uid" field (custom/templates/repo/create.tmpl),
// already set to the org, so webrepo.CreatePost reads it correctly as-is.
func RepoCreateForOrgPost(ctx *context.Context) {
	webrepo.CreatePost(ctx)
}
