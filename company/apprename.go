// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	files_service "gitea.dev/services/repository/files"

	"go.yaml.in/yaml/v4"
)

// Everything the platform keeps for an app is filed under a hash of its
// name. A repository renamed or transferred in Gitea's own UI therefore
// became two apps: the old one still serving under the old URL, which the
// department could no longer see, stop or deploy to, and a new one that
// had never been deployed. Following the rename is what a non-developer
// expects to happen, so it does.

func (*deployBranchCleanupNotifier) RenameRepository(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, oldRepoName string) {
	followCentralRepoMove(ctx, repo.OwnerName, oldRepoName, repo.OwnerName, repo.Name)
	followPolicyRename(ctx, doer, repo.OwnerName, oldRepoName, repo.OwnerName, repo.Name)
	followRepoRename(repo.OwnerName, oldRepoName, repo.OwnerName, repo.Name)
}

func (*deployBranchCleanupNotifier) TransferRepository(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, oldOwnerName string) {
	followCentralRepoMove(ctx, oldOwnerName, repo.Name, repo.OwnerName, repo.Name)
	followPolicyRename(ctx, doer, oldOwnerName, repo.Name, repo.OwnerName, repo.Name)
	followRepoRename(oldOwnerName, repo.Name, repo.OwnerName, repo.Name)
	// Moved out of the projects organization, it is nobody's project any more.
	if !IsProjectRepo(ctx, repo) {
		if err := deleteProjectOwner(ctx, repo.ID); err != nil {
			log.Error("company: forgetting the owner of %s after its transfer: %v", repo.FullName(), err)
		}
	}
}

// DeleteRepository stops the app whose repository is gone. Its files stay
// until an admin removes the app; its data is on the retention clock
// (company/appdata.go), keyed by an ID that outlives the name.
func (*deployBranchCleanupNotifier) DeleteRepository(ctx context.Context, doer *user_model.User, repo *repo_model.Repository) {
	if err := deleteProjectOwner(ctx, repo.ID); err != nil {
		log.Error("company: forgetting the owner of deleted %s: %v", repo.FullName(), err)
	}
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

// followPolicyRename moves the app's block in apps.yml to its new name.
// Without it a moved app lost every package, host and limit an administrator
// had approved for it, and its first deploy afterwards failed for reasons
// nobody had changed.
func followPolicyRename(ctx context.Context, doer *user_model.User, oldOwner, oldRepo, newOwner, newRepo string) {
	oldKey, newKey := oldOwner+"/"+oldRepo, newOwner+"/"+newRepo
	if oldKey == newKey {
		return
	}
	if cfg, _, _ := AppsConfigSnapshot(); cfg == nil || cfg.Apps[oldKey] == nil {
		return // almost every rename on the instance: nothing to carry
	}
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		log.Error("company: carrying the policy of %s to %s: %v", oldKey, newKey, err)
		return
	}
	current, _, err := readAppsConfigFile(ctx, central)
	if err != nil {
		log.Error("company: carrying the policy of %s to %s: %v", oldKey, newKey, err)
		return
	}
	entry := current.Apps[oldKey]
	if entry == nil {
		return
	}
	delete(current.Apps, oldKey)
	current.Apps[newKey] = entry
	body, err := yaml.Marshal(current)
	if err != nil {
		log.Error("company: carrying the policy of %s to %s: %v", oldKey, newKey, err)
		return
	}
	if doer == nil || doer.ID <= 0 {
		if doer, err = user_model.GetAdminUser(ctx); err != nil {
			log.Error("company: carrying the policy of %s to %s: %v", oldKey, newKey, err)
			return
		}
	}
	if _, err := files_service.ChangeRepoFiles(ctx, central, doer, &files_service.ChangeRepoFilesOptions{
		OldBranch: central.DefaultBranch,
		NewBranch: central.DefaultBranch,
		Message:   fmt.Sprintf("chore(apps): %s is now %s\n\nThe repository was moved by %s; its policy follows it.", oldKey, newKey, doer.Name),
		Files: []*files_service.ChangeRepoFile{{
			Operation:     "update",
			TreePath:      appsConfigPath,
			ContentReader: bytes.NewReader(body),
		}},
	}); err != nil {
		log.Error("company: carrying the policy of %s to %s: %v", oldKey, newKey, err)
		return
	}
	if parsed, err := ParseAppsConfig(body); err == nil {
		SetAppsConfig(parsed)
	}
	log.Info("company: the policy of %s now applies to %s", oldKey, newKey)
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
	// The narrowing a project chose for its app goes with it; left behind,
	// the app would be open to whatever apps.yml allows until a restart.
	if mode, ok := departmentAccess.LoadAndDelete(oldKey); ok {
		departmentAccess.Store(newKey, mode)
	}
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
