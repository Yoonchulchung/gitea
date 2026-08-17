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

	baseRef := git.RefNameFromBranch(pr.BaseBranch)
	headRef := git.RefNameFromBranch(pr.HeadBranch)
	ci, err := git_service.GetCompareInfo(ctx, pr.BaseRepo, pr.HeadRepo, headGitRepo, baseRef, headRef, false, true)
	if err != nil {
		ctx.ServerError("GetCompareInfo", err)
		return
	}

	diff, err := gitdiff.GetDiffForRender(ctx, pr.HeadRepo.Link(), headGitRepo, &gitdiff.DiffOptions{
		BeforeCommitID:     ci.CompareBase,
		AfterCommitID:      ci.HeadCommitID,
		MaxLines:           setting.Git.MaxGitDiffLines,
		MaxLineCharacters:  setting.Git.MaxGitDiffLineCharacters,
		MaxFiles:           setting.Git.MaxGitDiffFiles,
		WhitespaceBehavior: nil,
	})
	if err != nil {
		ctx.ServerError("GetDiffForRender", err)
		return
	}

	if err := pr.LoadIssue(ctx); err != nil {
		ctx.ServerError("LoadIssue", err)
		return
	}

	// Names on disk carry the deployPathPrefix (deploy.go) so two
	// departments' files never collide in the central repo — redundant
	// once the viewer already knows which department this request is
	// for, so strip it back off for display.
	prefix := deployPathPrefix(ownerName, repoName) + "/"
	for _, f := range diff.Files {
		f.Name = strings.TrimPrefix(f.Name, prefix)
		if f.OldName != "" {
			f.OldName = strings.TrimPrefix(f.OldName, prefix)
		}
	}

	ctx.Data["Title"] = "Deploy Request Files"
	ctx.Data["PullRequest"] = pr
	ctx.Data["DeptRepo"] = deptRepo
	ctx.Data["Diff"] = diff
	ctx.HTML(http.StatusOK, tplDeployRequestFiles)
}
