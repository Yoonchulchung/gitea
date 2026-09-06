// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"

	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
	notify_service "gitea.dev/services/notify"
	repo_service "gitea.dev/services/repository"
)

// deployBranchCleanupNotifier deletes a Deploy Request's branch on the
// central deploy repo the moment its PR closes — merged (an admin
// approved it) or not (rejected, or auto-superseded by
// cancelOpenDeployRequests, company/deployrequests.go). Each request
// creates a brand-new timestamped branch carrying a full snapshot of the
// department repo's files (deployBranchName, company/deploy.go); left
// alone, central-deploy's branch list would only ever grow, one branch per
// submission, most of them short-lived and superseded within minutes once
// cancelOpenDeployRequests is in the picture.
//
// Implemented as a notify.Notifier (registered below via this file's own
// init(), the same pattern services/webhook and friends use) rather than
// deleting the branch inline wherever a deploy-request PR gets closed:
// closing happens through three different code paths — an admin's native
// "Close" button, CancelDeployRequest, and cancelOpenDeployRequests — and
// merging is a fourth, entirely separate one again (services/pull/merge.go
// notifies MergePullRequest, never IssueChangeStatus, and updates the
// issue's closed state directly rather than through
// issues_model.CloseIssue). Hooking all three relevant notification points
// covers every one of them, including the native admin UI, without
// touching any core file.
type deployBranchCleanupNotifier struct {
	notify_service.NullNotifier
}

func init() {
	notify_service.RegisterNotifier(&deployBranchCleanupNotifier{})
}

// IssueChangeStatus fires for every issue/PR close or reopen instance-wide
// — closeOrReopen is true for a close, false for a reopen (and for a
// merge, this doesn't fire at all — see MergePullRequest below).
func (*deployBranchCleanupNotifier) IssueChangeStatus(ctx context.Context, _ *user_model.User, _ string, issue *issues_model.Issue, _ *issues_model.Comment, closeOrReopen bool) {
	if !closeOrReopen || !issue.IsPull {
		return
	}
	if err := issue.LoadPullRequest(ctx); err != nil {
		log.Error("company: deployBranchCleanupNotifier: LoadPullRequest: %v", err)
		return
	}
	cleanupDeployBranch(ctx, issue.PullRequest)
}

func (*deployBranchCleanupNotifier) MergePullRequest(ctx context.Context, doer *user_model.User, pr *issues_model.PullRequest) {
	queueDeployOnMerge(ctx, doer, pr)
	cleanupDeployBranch(ctx, pr)
}

func (*deployBranchCleanupNotifier) AutoMergePullRequest(ctx context.Context, doer *user_model.User, pr *issues_model.PullRequest) {
	queueDeployOnMerge(ctx, doer, pr)
	cleanupDeployBranch(ctx, pr)
}

// queueDeployOnMerge marks the department's app as queued the moment an
// admin merges its Deploy Request.
//
// Deliberately only on the merge paths, never from IssueChangeStatus: that
// one also fires for a rejected or withdrawn request, and those must not
// look like a pending deploy.
//
// Recording this here rather than when a worker eventually picks the job up
// buys two things. The staff badge flips to "배포 중" at the instant of
// approval instead of whenever a worker happens to be free. And a record
// still sitting at `queued` minutes later is a *detectable* "nothing picked
// this up" — otherwise that failure mode is invisible, which matters most
// right now, when no deploy worker exists yet at all.
//
// Best-effort throughout: this runs inside the merge notification, and a
// bookkeeping failure must never make the merge itself look broken. The
// same reasoning the AI review already follows (docs/company/ai-agent.md).
func queueDeployOnMerge(ctx context.Context, doer *user_model.User, pr *issues_model.PullRequest) {
	if pr.BaseRepoID != pr.HeadRepoID {
		return
	}
	owner, repo, _, ok := parseDeployBranchName(pr.HeadBranch)
	if !ok {
		return
	}
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		log.Error("company: queueDeployOnMerge: LoadBaseRepo: %v", err)
		return
	}
	if pr.BaseRepo.OwnerName != centralOwner || pr.BaseRepo.Name != centralName {
		return // deploy-shaped branch on some other repo — not ours
	}

	actor := ""
	if doer != nil {
		actor = doer.Name
	}
	if err := MutateAppState(owner, repo, func(st *AppState) bool {
		st.Desired = AppStateRunning // an approved request is a request to be running
		st.Actual = AppStateQueued
		st.PRID = pr.ID
		st.SHA = pr.MergedCommitID
		st.Reason, st.Message = "", "" // a new attempt clears the previous failure
		st.AppendHistory(AppHistoryEntry{
			SHA:    pr.MergedCommitID,
			Status: AppStateQueued,
			Actor:  actor,
		})
		return true
	}); err != nil {
		log.Error("company: queueDeployOnMerge: %s/%s: %v", owner, repo, err)
	}
	// The permissions this request asked for take effect before the deploy
	// that needs them is built — a package approved in the same merge has to
	// be installable by the worker that picks the job up next.
	if doer != nil {
		ApplyPermissionsOnMerge(ctx, doer, owner, repo, pr.ID)
	}
	enqueueDeploy(owner, repo, pr.MergedCommitID, pr.ID)
}

// cleanupDeployBranch deletes pr.HeadBranch on the central deploy repo,
// after confirming pr is actually a Deploy Request PR there — everything
// here is a defensive check, run instance-wide on every PR close/merge, so
// anything not shaped exactly like one of our own deploy branches on the
// currently configured central-deploy repo is left alone.
func cleanupDeployBranch(ctx context.Context, pr *issues_model.PullRequest) {
	if pr.BaseRepoID != pr.HeadRepoID {
		return // every Deploy Request PR is same-repo (company/deploy.go's openPullRequest call) — a cross-repo PR never is one
	}
	if _, _, _, ok := parseDeployBranchName(pr.HeadBranch); !ok {
		return
	}

	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		log.Error("company: deployBranchCleanupNotifier: %v", err)
		return
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		log.Error("company: deployBranchCleanupNotifier: LoadBaseRepo: %v", err)
		return
	}
	central := pr.BaseRepo
	if central.OwnerName != centralOwner || central.Name != centralName {
		return // shaped like a deploy branch, but not actually on the configured central-deploy repo — leave it alone
	}

	gitRepo, err := git.OpenRepository(ctx, central)
	if err != nil {
		log.Error("company: deployBranchCleanupNotifier: OpenRepository: %v", err)
		return
	}
	defer gitRepo.Close()

	// Record the branch's own head commit while it still resolves, so
	// DeployRequestFiles (company/deployrequestfiles.go) can still show
	// this request's file diff after the branch itself is gone below —
	// best-effort, and deliberately not fatal to the actual cleanup: a
	// failed snapshot only costs that one historical view later, not the
	// branch getting deleted now.
	if commit, err := gitRepo.GetBranchCommit(ctx, pr.HeadBranch); err != nil {
		log.Error("company: deployBranchCleanupNotifier: GetBranchCommit %s: %v", pr.HeadBranch, err)
	} else {
		saveDeploySnapshot(pr.ID, commit.ID.String())
	}

	// Deleting as central's own owner (not doer, who closed/merged this —
	// typically an admin, but CancelDeployRequest also lets the original
	// requester close their own still-pending one, and that person
	// normally has no write access to central-deploy at all to satisfy
	// repo_service.DeleteBranch's own permission check).
	centralOwnerUser, err := user_model.GetUserByID(ctx, central.OwnerID)
	if err != nil {
		log.Error("company: deployBranchCleanupNotifier: GetUserByID: %v", err)
		return
	}
	if err := repo_service.DeleteBranch(ctx, centralOwnerUser, central, gitRepo, pr.HeadBranch); err != nil {
		log.Error("company: deployBranchCleanupNotifier: DeleteBranch %s: %v", pr.HeadBranch, err)
	}
}
