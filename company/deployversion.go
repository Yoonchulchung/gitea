// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
)

// "이전 버전으로" goes back exactly one step, because it follows a symlink
// that only ever holds one. But every version this app has ever run is still
// in the deploy repository — that is what a git-backed record is for — so
// being able to reach only the most recent one is a limit of the mechanism
// rather than of what is possible.
//
// This deploys any commit in that history by name. It is the same path as any
// other deploy: the source is read from git, the environment is rebuilt, the
// health check decides whether it goes live, and a failure leaves the running
// version alone. What it is not is a restore of the past — the build uses
// today's policy, today's base packages and today's interpreter, so a version
// that failed a year ago on an unapproved package succeeds now that the
// package is approved, and one that worked may not still. Saying that on the
// screen matters more than the button.

// DeployVersion builds and activates a specific commit.
func DeployVersion(ctx context.Context, owner, repo, sha, actor string, isAdmin bool) error {
	st := LoadAppState(owner, repo)
	// Same gate as a redeploy: this replaces what is serving, so it is refused
	// in exactly the states a redeploy is.
	if ok, why := st.CanTransition("redeploy", isAdmin); !ok {
		return userKeyError(why)
	}

	full, err := resolveDeployCommit(ctx, sha)
	if err != nil {
		return err
	}
	if full == CurrentReleaseSHA(owner, repo) && st.Actual == AppStateRunning {
		// Otherwise this stops a working app and starts the same code again,
		// which looks like a deploy and changes nothing.
		return userKeyError("company.err.version_already_running")
	}

	if err := MutateAppState(owner, repo, func(s *AppState) bool {
		s.Desired = AppStateRunning
		s.Actual = AppStateQueued
		s.Reason, s.Message, s.UserMessage = "", "", ""
		s.AppendHistory(AppHistoryEntry{
			Status: AppStateQueued, SHA: full, Actor: actor,
			Reason: ReasonVersionPinned,
		})
		return true
	}); err != nil {
		return err
	}
	log.Info("company: %s/%s: %s asked for version %s", owner, repo, actor, full)

	// PRID is left at whatever the state holds: it identifies the request that
	// approved this app's permissions, and choosing an old version is not a
	// new request.
	enqueueDeploy(owner, repo, full, st.PRID)
	return nil
}

// resolveDeployCommit checks the commit is still there and returns its full
// id.
//
// The history page shows shortened ids, and a release that has been cleaned up
// leaves its entry in the build log behind — so "deploy this one" has to be
// able to say "that version is no longer in the repository" rather than fail
// later, inside a build, with a message about git.
func resolveDeployCommit(ctx context.Context, sha string) (string, error) {
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return "", err
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		return "", err
	}
	gitRepo, err := git.OpenRepository(ctx, central)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()

	commit, err := gitRepo.GetCommit(ctx, sha)
	if err != nil {
		return "", audienceKeyError("company.err.version_gone", "company.err.version_gone.admin", sha)
	}
	return commit.ID.String(), nil
}
