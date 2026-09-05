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
	"gitea.dev/modules/container"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	webrepo "gitea.dev/routers/web/repo"
	"gitea.dev/services/context"
	issue_service "gitea.dev/services/issue"

	"xorm.io/xorm"
)

// deployRequestsPageSize is the page size for both the Requested and
// Deployed sections on DeployRequests — they paginate independently (see
// company/deploy_requests_pager.tmpl), so each gets its own page of this
// size rather than sharing one combined page.
const deployRequestsPageSize = 15

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

// cancelOpenDeployRequests auto-withdraws every still-open deploy-request
// PR for (deptOwner, deptName) — called by DeployPost (company/deploy.go)
// right before it opens a new one. A department repo's deploy request is
// always a full snapshot of its default branch (see
// snapshotFilesUnderPrefix), never a diff against the previous request, so
// an older pending one can never be "still relevant" once a newer one
// exists — it would only leave the admin reviewing stale content, or
// mistakenly merging it after the newer, more current request. doer is
// whoever just submitted the new request; the comment/close is attributed
// to them the same way a manual CancelDeployRequest would be, since this
// is functionally the same action, just triggered automatically instead
// of by an explicit "Cancel" click.
func cancelOpenDeployRequests(ctx *context.Context, central *repo_model.Repository, deptOwner, deptName string, doer *user_model.User) error {
	var prs []*issues_model.PullRequest
	if err := db.GetEngine(ctx).
		Join("INNER", "issue", "issue.id = pull_request.issue_id").
		Where("pull_request.base_repo_id = ?", central.ID).
		And("pull_request.head_repo_id = ?", central.ID).
		And("pull_request.head_branch LIKE ?", deployBranchPrefix(deptOwner, deptName)+"%").
		And("issue.is_closed = ?", false).
		Find(&prs); err != nil {
		return fmt.Errorf("list open deploy requests: %w", err)
	}
	for _, pr := range prs {
		if err := pr.LoadIssue(ctx); err != nil {
			return fmt.Errorf("LoadIssue: %w", err)
		}
		reason := cancelCommentPrefix + "superseded by a newer Deploy Request from the same repo"
		if _, err := issue_service.CreateIssueComment(ctx, doer, central, pr.Issue, reason, nil); err != nil {
			return fmt.Errorf("CreateIssueComment: %w", err)
		}
		if err := issue_service.CloseIssue(ctx, pr.Issue, doer, ""); err != nil {
			return fmt.Errorf("CloseIssue: %w", err)
		}
	}
	return nil
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

	requestedPage := max(ctx.FormInt("requested_page"), 1)
	deployedPage := max(ctx.FormInt("deployed_page"), 1)

	// Deploy-request PRs are same-repo PRs on the central deploy repo
	// (company/deploy.go's DeployPost) — which department repo each one
	// is "for" isn't HeadRepoID (that's central too), it's encoded in the
	// branch name (deployBranchName/parseDeployBranchName). Every branch
	// for a repo this org owns starts with "deploy/<org-name>/".
	branchPrefix := "deploy/" + org.Name + "/"

	requested, requestedTotal, err := loadDeployRequestPage(ctx, central, branchPrefix, false, requestedPage)
	if err != nil {
		ctx.ServerError("load requested deploy requests", err)
		return
	}
	deployed, deployedTotal, err := loadDeployRequestPage(ctx, central, branchPrefix, true, deployedPage)
	if err != nil {
		ctx.ServerError("load deployed deploy requests", err)
		return
	}

	// Requested and Deployed paginate independently (they're separate
	// lists on the same page, not tabs of one list). Build() carries every
	// query param of the current request into each pager's own links, which
	// is exactly what keeps the other section's page from resetting to 1 —
	// but it would also re-emit this pager's own page param alongside the
	// one deploy_requests_pager.tmpl writes explicitly, so each drops its
	// own here and keeps only the other's.
	requestedPager := context.NewPagerBuilder(ctx).TotalCount(requestedTotal).PerPageLimit(deployRequestsPageSize).CurPage(requestedPage).Build()
	requestedPager.RemoveParam(container.SetOf("requested_page"))
	deployedPager := context.NewPagerBuilder(ctx).TotalCount(deployedTotal).PerPageLimit(deployRequestsPageSize).CurPage(deployedPage).Build()
	deployedPager.RemoveParam(container.SetOf("deployed_page"))

	ctx.Data["Title"] = "Deploy requests"
	ctx.Data["Org"] = org
	ctx.Data["Requested"] = requested
	ctx.Data["RequestedPage"] = requestedPager
	ctx.Data["Deployed"] = deployed
	ctx.Data["DeployedPage"] = deployedPager
	ctx.HTML(http.StatusOK, tplDeployRequests)
}

// deployRequestSession builds the shared base query for one status (open or
// closed) of one org's deploy requests — factored out so
// loadDeployRequestPage can run it once for the count and once, fresh, for
// the page of rows (an xorm session is single-use).
func deployRequestSession(ctx *context.Context, central *repo_model.Repository, branchPrefix string, closed bool) *xorm.Session {
	return db.GetEngine(ctx).
		Join("INNER", "issue", "issue.id = pull_request.issue_id").
		Where("pull_request.base_repo_id = ?", central.ID).
		And("pull_request.head_repo_id = ?", central.ID).
		And("pull_request.head_branch LIKE ?", branchPrefix+"%").
		And("issue.is_closed = ?", closed)
}

// loadDeployRequestPage fetches one page of one status (open/closed) of
// org's deploy requests, translated into deployRequestView, plus the total
// count across all pages for Pagination. Split out of DeployRequests so
// Requested and Deployed can be queried, counted, and paginated
// independently despite sharing the same underlying PR data.
func loadDeployRequestPage(ctx *context.Context, central *repo_model.Repository, branchPrefix string, closed bool, page int) ([]*deployRequestView, int64, error) {
	total, err := deployRequestSession(ctx, central, branchPrefix, closed).Count(new(issues_model.PullRequest))
	if err != nil {
		return nil, 0, fmt.Errorf("count deploy requests: %w", err)
	}

	var prs []*issues_model.PullRequest
	if err := deployRequestSession(ctx, central, branchPrefix, closed).
		Desc("issue.created_unix").
		Limit(deployRequestsPageSize, (page-1)*deployRequestsPageSize).
		Find(&prs); err != nil {
		return nil, 0, fmt.Errorf("list deploy requests: %w", err)
	}

	views := make([]*deployRequestView, 0, len(prs))
	for _, pr := range prs {
		if err := pr.LoadIssue(ctx); err != nil {
			return nil, 0, fmt.Errorf("LoadIssue: %w", err)
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
		if !closed {
			view.CanCancel = ctx.Doer.IsAdmin || requesterID == ctx.Doer.ID
			views = append(views, view)
			continue
		}
		if !pr.HasMerged {
			cancelled, err := wasCancelledByRequester(ctx, pr.Issue.ID)
			if err != nil {
				return nil, 0, fmt.Errorf("wasCancelledByRequester: %w", err)
			}
			if cancelled {
				view.Cancelled = true
			} else {
				view.Rejected = true
			}
		}
		views = append(views, view)
	}
	return views, total, nil
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
