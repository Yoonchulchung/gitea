// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
)

// Everything the platform keeps for an app is filed under a hash of its
// name. A repository renamed or transferred in Gitea's own UI therefore
// became two apps: the old one still serving under the old URL, which the
// department could no longer see, stop or deploy to, and a new one that
// had never been deployed. Following the rename is what a non-developer
// expects to happen, so it does.

func (*deployBranchCleanupNotifier) RenameRepository(_ context.Context, _ *user_model.User, repo *repo_model.Repository, oldRepoName string) {
	followRepoRename(repo.OwnerName, oldRepoName, repo.OwnerName, repo.Name)
}

func (*deployBranchCleanupNotifier) TransferRepository(_ context.Context, _ *user_model.User, repo *repo_model.Repository, oldOwnerName string) {
	followRepoRename(oldOwnerName, repo.Name, repo.OwnerName, repo.Name)
}

// DeleteRepository stops the app whose repository is gone. Its files stay
// until an admin removes the app; its data is on the retention clock
// (company/appdata.go), keyed by an ID that outlives the name.
func (*deployBranchCleanupNotifier) DeleteRepository(_ context.Context, doer *user_model.User, repo *repo_model.Repository) {
	if _, ok := LookupApp(repo.OwnerName, repo.Name); !ok {
		return
	}
	actor := "platform"
	if doer != nil {
		actor = doer.Name
	}
	if err := supervisorFor(repo.OwnerName, repo.Name).Stop(actor, AppStateStopped, ReasonRemoved); err != nil {
		log.Error("company: stopping %s/%s after its repository was deleted: %v", repo.OwnerName, repo.Name, err)
	}
	UnregisterApp(repo.OwnerName, repo.Name)
	_ = MutateAppState(repo.OwnerName, repo.Name, func(st *AppState) bool {
		st.Message = "the repository was deleted by " + actor
		st.UserMessageKey = "company.app.removed"
		return true
	})
}

// followRepoRename moves an app's records under its new name and rebuilds
// it there. The environment cannot follow — a virtualenv names its
// interpreter by absolute path — so the app is stopped, re-keyed and
// deployed again from the commit it was running, which is a minute of
// downtime rather than an app nobody can reach.
func followRepoRename(oldOwner, oldRepo, newOwner, newRepo string) {
	oldKey, newKey := appKey(oldOwner, oldRepo), appKey(newOwner, newRepo)
	if oldKey == newKey {
		return
	}
	if _, err := os.Stat(appStateFile(oldOwner, oldRepo)); err != nil {
		return // never an app
	}
	st := LoadAppState(oldOwner, oldRepo)

	s := supervisorFor(oldOwner, oldRepo)
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	s.mu.Lock()
	stopErr := s.stopLocked()
	s.mu.Unlock()
	if stopErr != nil {
		log.Error("company: %s/%s could not be stopped to follow its rename to %s/%s: %v", oldOwner, oldRepo, newOwner, newRepo, stopErr)
		return
	}
	UnregisterApp(oldOwner, oldRepo)
	proxyCache.Delete(oldKey)
	supervisorsMu.Lock()
	delete(supervisors, oldKey)
	supervisorsMu.Unlock()

	for _, dir := range appKeyedDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), oldKey) {
				continue
			}
			from := filepath.Join(dir, entry.Name())
			to := filepath.Join(dir, newKey+strings.TrimPrefix(entry.Name(), oldKey))
			if err := os.Rename(from, to); err != nil {
				log.Error("company: following the rename of %s/%s: %v", oldOwner, oldRepo, err)
			}
		}
	}
	oldHome, newHome := appPathsFor(oldOwner, oldRepo).home, appPathsFor(newOwner, newRepo).home
	repointSymlinks(newHome, oldHome, newHome)
	discardStaleVenvs(newHome)

	_ = MutateAppState(newOwner, newRepo, func(st *AppState) bool {
		st.Owner, st.Repo = newOwner, newRepo
		st.PID = 0
		if st.Actual == AppStateRunning {
			st.Actual = AppStateStopped
		}
		st.AppendHistory(AppHistoryEntry{Status: st.Actual, Reason: ReasonRenamed})
		return true
	})
	log.Info("company: %s/%s is now %s/%s", oldOwner, oldRepo, newOwner, newRepo)

	if st.SHA != "" && st.Desired == AppStateRunning && st.Actual != AppStateSuspended {
		QueueDeploy(newOwner, newRepo, st.SHA, st.PRID)
	}
}
