// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"fmt"
	"time"

	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
	gitea_context "gitea.dev/services/context"
	files_service "gitea.dev/services/repository/files"
)

// A deploy request that can no longer be merged — the central repository
// moved on since it was made, usually because a later request for the same
// app was approved first — used to hand the administrator git's own
// conflict message and no way through it but a developer.
//
// The request is a snapshot of one app's files, so the resolution is
// mechanical: put exactly those files on today's main, as a new branch and
// a new request, and close the old one as superseded. Nothing is merged by
// hand and nothing of the request changes.

// rebaseFilesFromCommit is the request's app files as they are in its
// snapshot, against what main holds for that app now: every file of the
// snapshot written, every file main has that the snapshot does not removed.
func rebaseFilesFromCommit(ctx *gitea_context.Context, gitRepo *git.Repository, head, main *git.Commit, prefix string) ([]*files_service.ChangeRepoFile, error) {
	tree, err := head.SubTree(ctx, gitRepo, prefix)
	if err != nil {
		return nil, fmt.Errorf("the request holds nothing for %s: %w", prefix, err)
	}
	entries, err := tree.ListEntriesRecursiveFast(ctx, gitRepo)
	if err != nil {
		return nil, err
	}
	files := make([]*files_service.ChangeRepoFile, 0, len(entries))
	kept := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.IsSubModule() {
			continue
		}
		kept[entry.Name()] = true
		blob := entry.Blob(gitRepo)
		content, err := blob.GetBlobBytes(ctx, blob.Size(ctx))
		if err != nil {
			return nil, fmt.Errorf("read blob for %s: %w", entry.Name(), err)
		}
		files = append(files, &files_service.ChangeRepoFile{
			Operation:     "upload",
			TreePath:      prefix + "/" + entry.Name(),
			ContentReader: bytes.NewReader(content),
		})
	}
	livePaths, err := centralPrefixPaths(ctx, gitRepo, main, prefix)
	if err != nil {
		return nil, err
	}
	for _, path := range deletePathsFor(kept, livePaths) {
		files = append(files, &files_service.ChangeRepoFile{Operation: "delete", TreePath: prefix + "/" + path})
	}
	return files, nil
}

// DeployRequestRebase puts a conflicting request on today's main as a new
// request with the same files, permissions, title and message, and closes
// the old one. Mounted at POST /company/deploy-request/{id}/rebase
// (company/routes.go), administrators only.
func DeployRequestRebase(ctx *gitea_context.Context) {
	owner, repo, pr, ok := deployRequestTarget(ctx)
	if !ok {
		return
	}
	central := pr.BaseRepo
	back := fmt.Sprintf("%s/pulls/%d", central.Link(), pr.Issue.Index)
	centralOwner, err := user_model.GetUserByID(ctx, central.OwnerID)
	if err != nil {
		ctx.ServerError("GetUserByID", err)
		return
	}
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, central)
	if err != nil {
		ctx.ServerError("RepositoryFromRequestContextOrOpen", err)
		return
	}
	head, err := gitRepo.GetBranchCommit(ctx, pr.HeadBranch)
	if err != nil {
		ctx.Flash.Error(ctx.Locale.TrString("company.err.version_gone"))
		ctx.Redirect(back)
		return
	}
	main, err := gitRepo.GetBranchCommit(ctx, central.DefaultBranch)
	if err != nil {
		ctx.ServerError("GetBranchCommit", err)
		return
	}
	prefix := deployPathPrefix(owner, repo)
	files, err := rebaseFilesFromCommit(ctx, gitRepo, head, main, prefix)
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		ctx.Redirect(back)
		return
	}
	_, _, requesterID, _ := parseDeployBranchName(pr.HeadBranch)
	requests := LoadPermissionRequests(owner, repo, pr.ID)
	// Always one more file, so the commit is never empty even when main
	// already holds every file of the snapshot — the same reason DeployPost
	// gives (company/deploy.go).
	files = append(files, buildRequestLogFile(ctx, central, owner, repo,
		requestLogEntry(ctx.Doer.Name, pr.Issue.Title, "rebased onto "+central.DefaultBranch+" by @"+ctx.Doer.Name, requests, time.Now())))

	newBranch := deployBranchName(owner, repo, requesterID)
	if _, err := files_service.ChangeRepoFiles(ctx, central, centralOwner, &files_service.ChangeRepoFilesOptions{
		OldBranch: central.DefaultBranch,
		NewBranch: newBranch,
		Message:   pr.Issue.Title,
		Files:     files,
	}); err != nil {
		ctx.ServerError("ChangeRepoFiles", err)
		return
	}
	// The old request first, as DeployPost does for a superseded one: two
	// open requests for one app would be one too many for the person deciding.
	if err := cancelOpenDeployRequests(ctx, central, owner, repo, ctx.Doer); err != nil {
		ctx.ServerError("cancelOpenDeployRequests", err)
		return
	}
	pullIssue, err := openPullRequest(ctx, central, central, newBranch, pr.Issue.Title, pr.Issue.Content, centralOwner)
	if err != nil {
		ctx.ServerError("openPullRequest", err)
		return
	}
	deptRepo, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, repo)
	if err == nil {
		applyDeployLabels(ctx, central, deptRepo, pullIssue, centralOwner)
	}
	if len(requests) > 0 {
		if err := SavePermissionRequests(owner, repo, pullIssue.ID, requests); err != nil {
			log.Error("company: carrying permission requests to the rebased request for %s/%s: %v", owner, repo, err)
		}
	}
	ctx.Flash.Success(ctx.Locale.TrString("company.review.rebased", pr.Issue.Index))
	ctx.Redirect(fmt.Sprintf("%s/pulls/%d", central.Link(), pullIssue.Index))
}
