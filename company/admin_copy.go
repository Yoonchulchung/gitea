// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	gitea_context "gitea.dev/services/context"
	repo_service "gitea.dev/services/repository"
)

// An administrator testing a department's app needs it somewhere of their own
// with the real code and the real rows: personal repositories are closed to
// everyone else, and testing on the department's own copy touches what the
// department is serving. The policy in apps.yml is not copied — the copy asks
// for and is granted its own through a deploy request like any other app.

// CopyRepoOptions says where a copy goes and what it carries beyond the code.
type CopyRepoOptions struct {
	Owner    *user_model.User
	Name     string
	WithData bool
	WithEnv  bool
}

// CopyRepoResult is what the copy ended up with, for the flash.
type CopyRepoResult struct {
	Repo    *repo_model.Repository
	Data    bool
	EnvVars int
}

// CopyDepartmentRepo copies every branch, tag and LFS object of src and,
// optionally, the app's database and environment variables.
func CopyDepartmentRepo(ctx context.Context, doer *user_model.User, src *repo_model.Repository, opts CopyRepoOptions) (*CopyRepoResult, error) {
	// A fork is the one path that clones the whole history and LFS with
	// branches and tags synced; it is a plain repository the moment after.
	dst, err := repo_service.ForkRepository(ctx, doer, opts.Owner, repo_service.ForkRepoOptions{
		BaseRepo:    src,
		Name:        opts.Name,
		Description: src.Description,
	})
	if err != nil {
		return nil, err
	}
	if err := repo_service.ConvertForkToNormalRepository(ctx, dst); err != nil {
		return nil, err
	}
	result := &CopyRepoResult{Repo: dst}

	if opts.WithEnv {
		vars, _, err := LoadAppEnv(src.OwnerName, src.Name)
		if err != nil {
			return result, err
		}
		if len(vars) > 0 {
			if err := SaveAppEnv(dst.OwnerName, dst.Name, doer.Name, vars, nil); err != nil {
				return result, err
			}
		}
		result.EnvVars = len(vars)
	}

	if opts.WithData {
		copied, err := copyAppData(ctx, src, dst)
		if err != nil {
			return result, err
		}
		result.Data = copied
	}
	log.Info("company: %s copied %s to %s (data: %v, env: %d)", doer.Name, src.FullName(), dst.FullName(), result.Data, result.EnvVars)
	return result, nil
}

// copyAppData puts a consistent copy of src's database where dst's app will
// look for it. Reports false when src has no database to copy.
func copyAppData(ctx context.Context, src, dst *repo_model.Repository) (bool, error) {
	srcDir, ok := AppDataDirIfPresent(ctx, src.OwnerName, src.Name)
	if !ok {
		return false, nil
	}
	srcDB := filepath.Join(srcDir, appDataDBName)
	if !isRegularFile(srcDB) {
		return false, nil
	}

	files := []string{srcDB}
	if IsAppRunning(src.OwnerName, src.Name) {
		// A file copy under a live writer is torn; a backup is a point in the
		// write stream, and it leaves nothing in a WAL to carry over.
		name, err := CreateSnapshot(ctx, src.OwnerName, src.Name, "copy")
		if err != nil {
			return false, fmt.Errorf("could not take a consistent copy of the running app's data: %w", err)
		}
		files = []string{filepath.Join(appSnapshotDirForApp(srcDir), name)}
	} else if isRegularFile(srcDB + "-wal") {
		files = append(files, srcDB+"-wal") // committed rows not yet checkpointed into app.db
	}

	dstDir, err := ensureAppDataDir(dst.ID, dst.OwnerName, dst.Name)
	if err != nil {
		return false, err
	}
	dstDB := filepath.Join(dstDir, appDataDBName)
	for _, from := range files {
		to := dstDB
		if strings.HasSuffix(from, "-wal") {
			to += "-wal"
		}
		if err := copyFile(from, to); err != nil {
			return false, err
		}
	}
	return true, nil
}

// isRegularFile refuses symlinks: the data directory is the app's to write,
// and a link there would have this copy read whatever host file it names.
func isRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// AdminCopyRepo handles the copy form on the admin app page.
func AdminCopyRepo(ctx *gitea_context.Context) {
	ownerName, repoName := ctx.PathParam("owner"), ctx.PathParam("repo")
	back := setting.AppSubURL + "/-/admin/company-deploys/" + ownerName + "/" + repoName

	src, err := repo_model.GetRepositoryByOwnerAndName(ctx, ownerName, repoName)
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		ctx.Redirect(back)
		return
	}
	owner, err := user_model.GetUserByID(ctx, ctx.FormInt64("owner_id"))
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		ctx.Redirect(back)
		return
	}
	name := strings.TrimSpace(ctx.FormString("name"))
	if err := repo_model.IsUsableRepoName(name); err != nil {
		ctx.Flash.Error(ctx.Locale.TrString("company.adminapp.copy_bad_name", name))
		ctx.Redirect(back)
		return
	}

	result, err := CopyDepartmentRepo(ctx, ctx.Doer, src, CopyRepoOptions{
		Owner:    owner,
		Name:     name,
		WithData: ctx.FormBool("with_data"),
		WithEnv:  ctx.FormBool("with_env"),
	})
	switch {
	case err != nil && repo_model.IsErrRepoAlreadyExist(err):
		ctx.Flash.Error(ctx.Locale.TrString("company.adminapp.copy_exists", owner.Name+"/"+name))
	case err != nil && result != nil:
		// The repository exists; what failed was carrying the data over, and
		// the admin needs to know which half they have.
		ctx.Flash.Error(ctx.Locale.TrString("company.adminapp.copy_partial", result.Repo.FullName(), AdminErrorL(ctx.Locale, err)))
	case err != nil:
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	default:
		msg := ctx.Locale.TrString("company.adminapp.copy_done", result.Repo.FullName())
		if result.Data {
			msg += " " + ctx.Locale.TrString("company.adminapp.copy_done_data")
		}
		if result.EnvVars > 0 {
			msg += " " + ctx.Locale.TrString("company.adminapp.copy_done_env", result.EnvVars)
		}
		ctx.Flash.Success(msg)
		ctx.Redirect(result.Repo.Link())
		return
	}
	ctx.Redirect(back)
}
