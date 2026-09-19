// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
	"gitea.dev/modules/translation"
	gitea_context "gitea.dev/services/context"
	files_service "gitea.dev/services/repository/files"

	"go.yaml.in/yaml/v4"
)

// An app leaves the platform the way it arrived: by a request an
// administrator approves. A removal request is a deploy request whose
// snapshot holds nothing for the app — every file of it under the
// department's prefix deleted — and approving it takes the app down,
// removes its files and its policy entry. Only then may the repository be
// deleted: a repository deleted first would leave a running app nobody can
// reach the record of, and a snapshot in the central repository that
// nothing owns.
//
// Marked in the branch name, where the platform keeps what it knows about
// a request (deployBranchName): the snapshot's contents are not a safe
// sign — a request made before the main.py check existed also has none.
const removalBranchSuffix = "-remove"

// isRemovalRequest reports whether a request is one to leave the platform.
func isRemovalRequest(pr *issues_model.PullRequest) bool {
	return strings.HasSuffix(pr.HeadBranch, removalBranchSuffix)
}

// appOnCentral reports whether the central repository still carries this
// app's snapshot — which is what a removal request is there to end.
func appOnCentral(ctx context.Context, owner, repo string) bool {
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return false
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		return false
	}
	_, found := readCentralFile(ctx, central, deployPathPrefix(owner, repo)+"/main.py")
	return found
}

// RequestRemoval opens the removal request for the department's app.
// Mounted as the "remove-request" verb of POST /{owner}/{repo}/_app/{verb}
// (AppControl), so write access to the repository is already required.
func RequestRemoval(ctx *gitea_context.Context) error {
	deptRepo := ctx.Repo.Repository
	owner, name := deptRepo.OwnerName, deptRepo.Name
	if _, deployed := LookupApp(owner, name); !deployed && !appOnCentral(ctx, owner, name) {
		return userKeyError("company.err.not_deployed")
	}
	central, err := centralDeployRepo(ctx)
	if err != nil {
		return err
	}
	centralOwner, err := user_model.GetUserByID(ctx, central.OwnerID)
	if err != nil {
		return err
	}
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, central)
	if err != nil {
		return err
	}
	main, err := gitRepo.GetBranchCommit(ctx, central.DefaultBranch)
	if err != nil {
		return err
	}
	prefix := deployPathPrefix(owner, name)
	livePaths, err := centralPrefixPaths(ctx, gitRepo, main, prefix)
	if err != nil {
		return err
	}
	title := fmt.Sprintf("Remove %s/%s from the platform", owner, name)
	files := make([]*files_service.ChangeRepoFile, 0, len(livePaths)+1)
	for path := range livePaths {
		if prefix+"/"+path == requestLogPath(owner, name) {
			continue // rewritten below with this entry on top
		}
		files = append(files, &files_service.ChangeRepoFile{Operation: "delete", TreePath: prefix + "/" + path})
	}
	files = append(files, buildRequestLogFile(ctx, central, owner, name,
		requestLogEntry(ctx.Doer.Name, title, "removal requested", nil, time.Now())))

	newBranch := deployBranchName(owner, name, ctx.Doer.ID) + removalBranchSuffix
	if _, err := files_service.ChangeRepoFiles(ctx, central, centralOwner, &files_service.ChangeRepoFilesOptions{
		OldBranch: central.DefaultBranch,
		NewBranch: newBranch,
		Message:   title,
		Files:     files,
	}); err != nil {
		return err
	}
	if err := cancelOpenDeployRequests(ctx, central, owner, name, ctx.Doer); err != nil {
		return err
	}
	content := fmt.Sprintf("Requested by @%s (%s).\n\nThis request removes the app from the platform: it stops, its files and its policy entry go, and the repository can then be deleted.", ctx.Doer.Name, deptRepo.FullName())
	pullIssue, err := openPullRequest(ctx, central, central, newBranch, title, content, centralOwner)
	if err != nil {
		return err
	}
	applyDeployLabels(ctx, central, deptRepo, pullIssue, centralOwner)
	NotifyDeployRequest(ctx, deptRepo, ctx.Doer.Name, pullIssue)
	return nil
}

// removeOnMerge is the approval of a removal: the app comes down and its
// files and policy entry go. Called from queueDeployOnMerge in place of a
// deploy when the merged commit holds no app.
func removeOnMerge(ctx context.Context, doer *user_model.User, owner, repo string) {
	actor := "platform"
	if doer != nil {
		actor = doer.Name
	}
	if _, deployed := LookupApp(owner, repo); deployed {
		if err := RemoveApp(ctx, owner, repo, actor); err != nil {
			log.Error("company: removing %s/%s after its removal request was approved: %v", owner, repo, err)
			return
		}
	}
	if doer != nil {
		if err := removeAppPolicy(ctx, doer, owner, repo); err != nil {
			log.Error("company: removing the policy entry of %s/%s: %v", owner, repo, err)
		}
	}
	log.Info("company: %s/%s removed from the platform, approved by %s", owner, repo, actor)
}

// removeAppPolicy drops the app's block from apps.yml, as a commit like
// every other policy change.
func removeAppPolicy(ctx context.Context, doer *user_model.User, owner, repo string) error {
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return err
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := range commitRetries {
		current, existed, err := readAppsConfigFile(ctx, central)
		if err != nil {
			return err
		}
		key := owner + "/" + repo
		if !existed || current.Apps == nil || current.Apps[key] == nil {
			return nil // nothing recorded for it
		}
		delete(current.Apps, key)
		body, err := yaml.Marshal(current)
		if err != nil {
			return err
		}
		message := fmt.Sprintf("chore(apps): remove %s\n\nRemoval approved by %s.", key, doer.Name)
		if attempt > 0 {
			message += fmt.Sprintf("\n\n(retry %d after a concurrent update)", attempt)
		}
		if _, lastErr = files_service.ChangeRepoFiles(ctx, central, doer, &files_service.ChangeRepoFilesOptions{
			OldBranch: central.DefaultBranch,
			NewBranch: central.DefaultBranch,
			Message:   message,
			Files:     []*files_service.ChangeRepoFile{{Operation: "update", TreePath: appsConfigPath, ContentReader: bytes.NewReader(body)}},
		}); lastErr == nil {
			if parsed, perr := ParseAppsConfig(body); perr == nil {
				SetAppsConfig(parsed)
			}
			return nil
		}
	}
	return lastErr
}

// RepoDeleteBlocked says why a repository may not be deleted yet, or "":
// its app is still on the platform, and removal is a request an
// administrator approves, not a side effect of deleting the code. Checked
// on both doors to deletion (routers/web/repo/setting/setting.go,
// routers/api/v1/repo/repo.go).
func RepoDeleteBlocked(ctx context.Context, locale translation.Locale, repo *repo_model.Repository) string {
	if repo == nil {
		return ""
	}
	if _, deployed := LookupApp(repo.OwnerName, repo.Name); !deployed && !appOnCentral(ctx, repo.OwnerName, repo.Name) {
		return ""
	}
	return locale.TrString("company.err.repo_deployed", repo.Link()+"/_app")
}
