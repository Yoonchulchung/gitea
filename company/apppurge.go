// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
	files_service "gitea.dev/services/repository/files"
)

// An app leaves the platform by a removal request the department makes and
// an administrator approves (company/appremoval.go). An app whose repository
// is already gone — deleted before that rule existed, or taken with its
// organisation — has no department left to ask, so it would sit in the
// attention list for ever. The administrator ends it directly: the same
// steps the approval of a removal takes, minus the request nobody can open.
// Refused while the repository exists, so this never becomes a way round
// the request.

// AdminPurgeGoneApp is POST /-/admin/company-deploys/{owner}/{repo}/purge.
func AdminPurgeGoneApp(ctx *context.Context) {
	owner, name := ctx.PathParam("owner"), ctx.PathParam("repo")
	if err := purgeGoneApp(ctx, ctx.Doer, owner, name); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	} else {
		ctx.Flash.Success(ctx.Locale.TrString("company.admin.purged", owner+"/"+name))
	}
	ctx.Redirect(setting.AppSubURL+"/-/admin/company-deploys", http.StatusSeeOther)
}

func purgeGoneApp(ctx *context.Context, doer *user_model.User, owner, repo string) error {
	if _, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, repo); err == nil {
		return userKeyError("company.err.purge_repo_exists")
	} else if !repo_model.IsErrRepoNotExist(err) {
		return err
	}
	if _, deployed := LookupApp(owner, repo); deployed {
		if err := RemoveApp(ctx, owner, repo, doer.Name); err != nil {
			return err
		}
	} else if err := os.RemoveAll(appPathsFor(owner, repo).home); err != nil {
		return fmt.Errorf("the app's files could not be removed: %w", err)
	}
	if err := removeAppFromCentral(ctx, doer, owner, repo); err != nil {
		return err
	}
	if err := removeAppPolicy(ctx, doer, owner, repo); err != nil {
		return err
	}
	if err := deleteAppState(owner, repo); err != nil {
		return err
	}
	if err := os.Remove(metricsFile(owner, repo)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("company: %s/%s: metrics file not removed: %v", owner, repo, err)
	}
	log.Info("company: %s/%s purged by %s: its repository was gone", owner, repo, doer.Name)
	return nil
}

// removeAppFromCentral deletes the app's snapshot, as one commit by the
// administrator, with the reason on the request log the way an approved
// removal leaves it.
func removeAppFromCentral(ctx *context.Context, doer *user_model.User, owner, repo string) error {
	central, err := centralDeployRepo(ctx)
	if err != nil {
		return err
	}
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, central)
	if err != nil {
		return err
	}
	head, err := gitRepo.GetBranchCommit(ctx, central.DefaultBranch)
	if err != nil {
		return err
	}
	prefix := deployPathPrefix(owner, repo)
	livePaths, err := centralPrefixPaths(ctx, gitRepo, head, prefix)
	if err != nil {
		return err
	}
	if len(livePaths) == 0 {
		return nil
	}
	title := fmt.Sprintf("Remove %s/%s from the platform", owner, repo)
	files := make([]*files_service.ChangeRepoFile, 0, len(livePaths)+1)
	for path := range livePaths {
		if prefix+"/"+path == requestLogPath(owner, repo) {
			continue // rewritten below with this entry on top
		}
		files = append(files, &files_service.ChangeRepoFile{Operation: "delete", TreePath: prefix + "/" + path})
	}
	files = append(files, buildRequestLogFile(ctx, central, owner, repo,
		requestLogEntry(doer.Name, title, "removed by an administrator: the repository is gone", nil, time.Now())))
	_, err = files_service.ChangeRepoFiles(ctx, central, doer, &files_service.ChangeRepoFilesOptions{
		OldBranch: central.DefaultBranch,
		NewBranch: central.DefaultBranch,
		Message:   title + "\n\nThe repository is gone; removed by " + doer.Name + ".",
		Files:     files,
	})
	return err
}

// deleteAppState forgets an app: the record is what lists it, and after a
// purge there is nothing left for the record to be about.
func deleteAppState(owner, repo string) error {
	file := appStateFile(owner, repo)
	lock := appStateLockFor(file)
	lock.Lock()
	defer lock.Unlock()
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
