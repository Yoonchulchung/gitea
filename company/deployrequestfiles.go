// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"strings"

	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/modules/git"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
	git_service "gitea.dev/services/git"
	"gitea.dev/services/gitdiff"
)

const tplDeployRequestFiles templates.TplName = "company/deploy_request_files"

// DeployRequestFiles shows just the changed-file list for one deploy
// request — not Gitea's own PR page (no comments, no merge button, no
// commit history), since the viewer usually can't even read the central
// deploy repo the underlying PR lives on (it's private — see
// docs/company/architecture.md on why). Authorized by write access to the
// department repo the request came FROM — the same bar DeployPost itself
// uses — so this works without granting any access to the central repo.
// The department repo is no longer pr.HeadRepo (that's central-deploy too,
// now that DeployPost opens same-repo PRs — see deploy.go); it's parsed out
// of the branch name instead (parseDeployBranchName).
// Mounted at /org/{org}/dashboard/deploy-requests/{id}, nested under the
// same org-membership-gated group as the list itself (routers/web/web.go),
// not a standalone /company/... URL.
func DeployRequestFiles(ctx *context.Context) {
	id := ctx.PathParamInt64("id")
	pr, err := issues_model.GetPullRequestByID(ctx, id)
	if err != nil {
		ctx.NotFound(err)
		return
	}
	if err := pr.LoadHeadRepo(ctx); err != nil {
		ctx.ServerError("LoadHeadRepo", err)
		return
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		ctx.ServerError("LoadBaseRepo", err)
		return
	}

	ownerName, repoName, _, ok := parseDeployBranchName(pr.HeadBranch)
	if !ok {
		ctx.NotFound(nil)
		return
	}
	deptRepo, err := repo_model.GetRepositoryByOwnerAndName(ctx, ownerName, repoName)
	if err != nil {
		ctx.NotFound(err)
		return
	}

	if !ctx.Doer.IsAdmin {
		perm, err := access_model.GetDoerRepoPermission(ctx, deptRepo, ctx.Doer)
		if err != nil {
			ctx.ServerError("GetDoerRepoPermission", err)
			return
		}
		if !perm.CanWrite(unit.TypeCode) {
			ctx.NotFound(nil)
			return
		}
	}

	headGitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, pr.HeadRepo)
	if err != nil {
		ctx.ServerError("RepositoryFromRequestContextOrOpen", err)
		return
	}

	// The normal path: the branch still exists (this request is still
	// open), so resolve both sides by ref the way any other compare would.
	baseRef := git.RefNameFromBranch(pr.BaseBranch)
	headRef := git.RefNameFromBranch(pr.HeadBranch)
	var beforeCommitID, afterCommitID string
	if ci, err := git_service.GetCompareInfo(ctx, pr.BaseRepo, pr.HeadRepo, headGitRepo, baseRef, headRef, false, true); err == nil {
		beforeCommitID, afterCommitID = ci.CompareBase, ci.HeadCommitID
	} else {
		// Once this PR is closed, deployBranchCleanupNotifier
		// (company/deploy_notifier.go) has already deleted its branch — this
		// is the expected, common case for anything not still pending, not
		// an error to surface. Fall back to what it recorded right before
		// deleting: pr.MergeBase is already a durable DB column (no ref
		// needed), and loadDeploySnapshot recovers the head side the same
		// way. If even the commit objects themselves are gone (pruned),
		// this still fails below — handled as "no longer available", not a
		// 500.
		snapshotID, ok := loadDeploySnapshot(pr.ID)
		if !ok {
			ctx.ServerError("GetCompareInfo", err)
			return
		}
		beforeCommitID, afterCommitID = pr.MergeBase, snapshotID
	}

	diff, diffErr := gitdiff.GetDiffForRender(ctx, pr.HeadRepo.Link(), headGitRepo, &gitdiff.DiffOptions{
		BeforeCommitID:     beforeCommitID,
		AfterCommitID:      afterCommitID,
		MaxLines:           setting.Git.MaxGitDiffLines,
		MaxLineCharacters:  setting.Git.MaxGitDiffLineCharacters,
		MaxFiles:           setting.Git.MaxGitDiffFiles,
		WhitespaceBehavior: nil,
	})
	// A failure here (commit objects themselves pruned, not just the ref)
	// still renders the page — just without the diff — rather than a 500,
	// since the record (status, comments, who/when) is still worth showing
	// even without file content.
	filesUnavailable := diffErr != nil

	if err := pr.LoadIssue(ctx); err != nil {
		ctx.ServerError("LoadIssue", err)
		return
	}

	// Whatever the admin (or the requester, cancelling) wrote alongside
	// closing this — Gitea's native PR page lets a status change and a
	// comment land together in one "Close with comment" submission, which
	// is the normal way a rejection reason gets recorded; nothing marks
	// which comment specifically "was" the reason, so this shows the whole
	// conversation rather than guessing at one. CancelDeployRequest's own
	// "Cancelled: ..." comment (company/deployrequests.go) shows up here
	// too, which is correct — it's exactly this kind of context.
	comments, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{
		IssueID: pr.Issue.ID,
		Type:    issues_model.CommentTypeComment,
	})
	if err != nil {
		ctx.ServerError("FindComments", err)
		return
	}
	if err := comments.LoadPosters(ctx); err != nil {
		ctx.ServerError("LoadPosters", err)
		return
	}

	// Names on disk carry the deployPathPrefix (deploy.go) so two
	// departments' files never collide in the central repo — redundant
	// once the viewer already knows which department this request is
	// for, so strip it back off for display.
	if !filesUnavailable {
		prefix := deployPathPrefix(ownerName, repoName) + "/"
		for _, f := range diff.Files {
			f.Name = strings.TrimPrefix(f.Name, prefix)
			if f.OldName != "" {
				f.OldName = strings.TrimPrefix(f.OldName, prefix)
			}
		}
	}

	status, err := deployStatusFor(ctx, pr)
	if err != nil {
		ctx.ServerError("deployStatusFor", err)
		return
	}

	ctx.Data["Title"] = "Deploy Request Files"
	ctx.Data["PullRequest"] = pr
	ctx.Data["DeptRepo"] = deptRepo
	ctx.Data["Diff"] = diff
	ctx.Data["FilesUnavailable"] = filesUnavailable
	ctx.Data["Comments"] = comments
	ctx.Data["DeployRequestStatus"] = status
	ctx.HTML(http.StatusOK, tplDeployRequestFiles)
}
