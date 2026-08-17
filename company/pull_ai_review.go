// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"net/http"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
	issue_service "gitea.dev/services/issue"
)

// verifyDeployRequestPR confirms pr is actually a Deploy Request PR on the
// currently configured central deploy repo — same-repo PR, deploy/-shaped
// branch, and the repo it's really on matches [company] CENTRAL_DEPLOY_REPO
// — and returns the department repo (owner/name) it came from. Shared by
// SetDeployRequestAIReviewData (decides whether to show the button) and
// TriggerDeployRequestAIReview (re-verifies before actually acting on a
// click) so both apply exactly the same rule.
func verifyDeployRequestPR(ctx *context.Context, pr *issues_model.PullRequest) (deptOwner, deptName string, ok bool) {
	if pr.BaseRepoID != pr.HeadRepoID {
		return "", "", false
	}
	deptOwner, deptName, _, ok = parseDeployBranchName(pr.HeadBranch)
	if !ok {
		return "", "", false
	}
	if err := pr.LoadIssue(ctx); err != nil {
		return "", "", false
	}
	if pr.Issue.IsClosed {
		// closed means deployBranchCleanupNotifier (company/deploy_notifier.go)
		// already deleted this branch — there's no diff left to review either way
		return "", "", false
	}

	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return "", "", false
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return "", "", false
	}
	if pr.BaseRepo.OwnerName != centralOwner || pr.BaseRepo.Name != centralName {
		return "", "", false
	}
	return deptOwner, deptName, true
}

// SetDeployRequestAIReviewData runs ahead of Gitea's native repo.ViewIssue
// on every /{owner}/{repo}/pulls/{index} view (routers/web/web.go) — cheap
// on the vast majority of PRs it'll see (bails on the first check that
// fails), and only ever adds data for the template to conditionally render
// something from; never blocks or alters the native page otherwise.
//
// Deliberately admin-only: reuses the viewing admin's own AI settings
// (company/settings_ai.go), not a shared one. Whether that admin has one
// set up yet changes what shows, not whether anything does — an admin who
// hasn't configured AI still sees a prompt pointing at Settings instead of
// the review button silently not being there with no explanation.
func SetDeployRequestAIReviewData(ctx *context.Context) {
	if ctx.Doer == nil || !ctx.Doer.IsAdmin || ctx.Repo.Repository == nil {
		return
	}
	index := ctx.PathParamInt64("index")
	pr, err := issues_model.GetPullRequestByIndex(ctx, ctx.Repo.Repository.ID, index)
	if err != nil {
		return // not a PR, or doesn't exist — repo.ViewIssue itself will 404 as usual
	}
	if _, _, ok := verifyDeployRequestPR(ctx, pr); !ok {
		return
	}
	if !AIConfiguredFor(ctx, ctx.Doer.ID) {
		ctx.Data["ShowDeployAIReviewSetup"] = true
		ctx.Data["DeployAIReviewSetupURL"] = fmt.Sprintf("%s/user/settings/ai", setting.AppSubURL)
		return
	}
	ctx.Data["ShowDeployAIReview"] = true
	ctx.Data["DeployAIReviewURL"] = fmt.Sprintf("%s/company/deploy-request/%d/ai-review", setting.AppSubURL, pr.ID)
}

// TriggerDeployRequestAIReview posts a fresh AI review comment on-demand,
// run as whoever clicked the button (their own AI settings, not
// centralOwner's the way the automatic on-submit review is) — see
// generateAIReview (company/deploy.go) for the shared prompt/diff logic.
// Re-verifies everything SetDeployRequestAIReviewData already checked
// before showing the button: a hidden button is not the same as an
// unauthorized action, and this is reachable directly by URL regardless of
// what any page happened to render. Mounted at
// /company/deploy-request/{id}/ai-review (company/routes.go), keyed by
// pull_request.id the same way CancelDeployRequest is.
func TriggerDeployRequestAIReview(ctx *context.Context) {
	if !ctx.Doer.IsAdmin {
		ctx.NotFound(nil)
		return
	}

	id := ctx.PathParamInt64("id")
	pr, err := issues_model.GetPullRequestByID(ctx, id)
	if err != nil {
		ctx.NotFound(err)
		return
	}
	deptOwner, deptName, ok := verifyDeployRequestPR(ctx, pr) // loads pr.Issue itself, see there
	if !ok {
		ctx.NotFound(nil)
		return
	}
	if !AIConfiguredFor(ctx, ctx.Doer.ID) {
		ctx.HTTPError(http.StatusBadRequest, "AI isn't configured for your account — see Settings")
		return
	}

	central := pr.BaseRepo
	deptRepo, err := repo_model.GetRepositoryByOwnerAndName(ctx, deptOwner, deptName)
	if err != nil {
		// department repo renamed/deleted since — still worth reviewing, just with a plainer label
		deptRepo = &repo_model.Repository{OwnerName: deptOwner, Name: deptName}
	}

	// A large diff comes back as several reviews (generateAIReviews chunks
	// by file) — each becomes its own comment below. If even the first
	// chunk fails, there's nothing to show for the click at all, so that's
	// a real error; a later chunk failing after earlier ones already
	// succeeded is posted-so-far, logged, not a failed request — the admin
	// still gets real, already-landed comments instead of losing them to
	// a 500 page.
	reviews, err := generateAIReviews(ctx, ctx.Doer.ID, central, pr.HeadBranch, deptRepo.FullName(), pr.Issue.Title)
	if err != nil && len(reviews) == 0 {
		ctx.ServerError("generateAIReviews", err)
		return
	}
	for _, review := range reviews {
		if _, cerr := issue_service.CreateIssueComment(ctx, ctx.Doer, central, pr.Issue, aiReviewCommentMarker+" (requested by @"+ctx.Doer.Name+")\n\n"+review, nil); cerr != nil {
			ctx.ServerError("CreateIssueComment", cerr)
			return
		}
	}
	if err != nil {
		log.Error("company: TriggerDeployRequestAIReview: %v", err)
	}

	ctx.Redirect(fmt.Sprintf("%s/pulls/%d", central.Link(), pr.Issue.Index))
}
