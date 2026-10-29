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
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/container"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
	issue_service "gitea.dev/services/issue"

	"xorm.io/builder"
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
	// Items is what this request asks to be allowed, with its ticks — the
	// requester can take one back while nobody has decided yet
	// (company/permissions_decide.go). Only filled for open requests.
	Items []permissionRequestRow
}

// DeployRequests lists the deploy requests of every project the viewer can
// see, split the same way /pulls splits Open/Closed — "배포 요청됨" (still
// open, awaiting admin review) and "배포함" (closed: either merged =
// actually deployed, or rejected). A project's members see each other's
// requests without needing PR/branch vocabulary or admin access. Reuses the
// same underlying PR data the admin reviews natively
// (docs/company/architecture.md); this is a second, friendlier view onto it,
// not a second workflow.
func DeployRequests(ctx *context.Context) {
	central, err := centralDeployRepo(ctx)
	if err != nil {
		ctx.ServerError("centralDeployRepo", err)
		return
	}
	branches, err := visibleDeployBranches(ctx, ctx.Doer)
	if err != nil {
		ctx.ServerError("visibleDeployBranches", err)
		return
	}

	requestedPage := max(ctx.FormInt("requested_page"), 1)
	deployedPage := max(ctx.FormInt("deployed_page"), 1)

	requested, requestedTotal, err := loadDeployRequestPage(ctx, central, branches, false, requestedPage)
	if err != nil {
		ctx.ServerError("load requested deploy requests", err)
		return
	}
	deployed, deployedTotal, err := loadDeployRequestPage(ctx, central, branches, true, deployedPage)
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

	ctx.Data["Title"] = ctx.Locale.TrString("company.deploy_requests.title")
	ctx.Data["Requested"] = requested
	ctx.Data["RequestedPage"] = requestedPager
	ctx.Data["Deployed"] = deployed
	ctx.Data["DeployedPage"] = deployedPager
	ctx.HTML(http.StatusOK, tplDeployRequests)
}

// visibleDeployBranches matches the deploy branches of every repository doer
// can read, plus the ones doer opened. Deploy-request PRs are same-repo PRs on
// the central deploy repo, so which repository each one is "for" is encoded
// in the branch name (deployBranchName), not in HeadRepoID.
func visibleDeployBranches(ctx *context.Context, doer *user_model.User) (builder.Cond, error) {
	if doer.IsAdmin {
		return builder.Like{"pull_request.head_branch", "deploy/%"}, nil
	}
	var repos []*repo_model.Repository
	if err := db.GetEngine(ctx).Cols("owner_name", "name").
		Where(repo_model.AccessibleRepositoryCondition(doer, unit.TypeCode)).Find(&repos); err != nil {
		return nil, err
	}
	cond := builder.Or(builder.Like{"pull_request.head_branch", fmt.Sprintf("deploy/%%/%%/%d/%%", doer.ID)})
	for _, repo := range repos {
		cond = cond.Or(builder.Like{"pull_request.head_branch", deployBranchPrefix(repo.OwnerName, repo.Name) + "%"})
	}
	return cond, nil
}

// deployRequestSession builds the shared base query for one status (open or
// closed) of the visible deploy requests — factored out so
// loadDeployRequestPage can run it once for the count and once, fresh, for
// the page of rows (an xorm session is single-use).
func deployRequestSession(ctx *context.Context, central *repo_model.Repository, branches builder.Cond, closed bool) *xorm.Session {
	return db.GetEngine(ctx).
		Join("INNER", "issue", "issue.id = pull_request.issue_id").
		Where("pull_request.base_repo_id = ?", central.ID).
		And("pull_request.head_repo_id = ?", central.ID).
		And(branches).
		And("issue.is_closed = ?", closed)
}

// loadDeployRequestPage fetches one page of one status (open/closed) of
// the visible deploy requests, translated into deployRequestView, plus the total
// count across all pages for Pagination. Split out of DeployRequests so
// Requested and Deployed can be queried, counted, and paginated
// independently despite sharing the same underlying PR data.
func loadDeployRequestPage(ctx *context.Context, central *repo_model.Repository, branches builder.Cond, closed bool, page int) ([]*deployRequestView, int64, error) {
	total, err := deployRequestSession(ctx, central, branches, closed).Count(new(issues_model.PullRequest))
	if err != nil {
		return nil, 0, fmt.Errorf("count deploy requests: %w", err)
	}

	var prs []*issues_model.PullRequest
	if err := deployRequestSession(ctx, central, branches, closed).
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
		repo, err := repoFollowingMoves(ctx, ownerName, repoName)
		if err != nil {
			continue // repo deleted since — skip rather than fail the whole list
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
			if view.CanCancel {
				// Same right as cancelling: whoever may take the whole request
				// back may take one item off it.
				view.Items = permissionRequestRows(LoadPermissionRequests(repo.OwnerName, repo.Name, pr.ID))
			}
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

	// Back where it was pressed: the list sends "from_list"; an administrator
	// cancelling from the request page wants the request they are looking at.
	ctx.Redirect(decisionRedirect(ctx, pr))
}

// openDeployRequestFor returns this repository's request that is still open,
// or nil.
//
// The deploy form is where someone goes to ask for something, and it said
// nothing about the request they had already made. So an unanswered request
// looked like no request, and the way to find out was to submit another —
// which supersedes the first and moves it to the back of nobody's queue.
func openDeployRequestFor(ctx *context.Context, deptOwner, deptName string) *issues_model.PullRequest {
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return nil
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		return nil
	}
	var prs []*issues_model.PullRequest
	if err := db.GetEngine(ctx).
		Join("INNER", "issue", "issue.id = pull_request.issue_id").
		Where("pull_request.base_repo_id = ?", central.ID).
		And("pull_request.head_repo_id = ?", central.ID).
		And("pull_request.head_branch LIKE ?", deployBranchPrefix(deptOwner, deptName)+"%").
		And("issue.is_closed = ?", false).
		OrderBy("pull_request.id DESC").
		Limit(1).
		Find(&prs); err != nil || len(prs) == 0 {
		return nil
	}
	if err := prs[0].LoadIssue(ctx); err != nil {
		return nil
	}
	return prs[0]
}
