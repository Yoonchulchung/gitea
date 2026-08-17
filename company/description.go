// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"encoding/json"
	"net/http"

	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/services/context"
)

// resolveWritableRepo loads the {owner}/{repo} in the URL and confirms the
// doer has Write access — this endpoint is mounted under /company (not
// Gitea's own repo route group, unlike Workspace/WorkspaceSave in
// workspace.go), so it has to do its own repo lookup and permission check
// rather than reading them off ctx.Repo.
func resolveWritableRepo(ctx *context.Context) *repo_model.Repository {
	owner, repoName := ctx.PathParam("owner"), ctx.PathParam("repo")
	target, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, repoName)
	if err != nil {
		ctx.NotFound(err)
		return nil
	}
	perm, err := access_model.GetDoerRepoPermission(ctx, target, ctx.Doer)
	if err != nil {
		ctx.ServerError("GetDoerRepoPermission", err)
		return nil
	}
	if !perm.CanWrite(unit.TypeCode) {
		ctx.NotFound(nil)
		return nil
	}
	return target
}

// UpdateDescription lets a non-admin with Write access change just the repo
// description — native Gitea only offers that on the repo Settings page,
// which requires actual repo Admin permission (routers/web/web.go's
// reqRepoAdmin), out of reach for a regular department-team member.
// See docs/company/repo-ui.md.
func UpdateDescription(ctx *context.Context) {
	target := resolveWritableRepo(ctx)
	if ctx.Written() {
		return
	}

	var req struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(ctx.Req.Body).Decode(&req); err != nil {
		ctx.HTTPError(http.StatusBadRequest, "invalid request")
		return
	}
	if len(req.Description) > 2048 { // same limit as forms.RepoSettingForm.Description
		ctx.HTTPError(http.StatusBadRequest, "description too long")
		return
	}

	target.Description = req.Description
	if err := repo_model.UpdateRepositoryColsWithAutoTime(ctx, target, "description"); err != nil {
		ctx.ServerError("UpdateRepositoryColsWithAutoTime", err)
		return
	}

	ctx.JSON(http.StatusOK, map[string]any{"ok": true})
}
