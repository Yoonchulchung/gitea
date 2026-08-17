// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"encoding/json"
	"fmt"
	"net/http"

	user_model "gitea.dev/models/user"
	"gitea.dev/services/context"
)

// Which tree folders someone left expanded in the workspace editor's
// sidebar — remembered per (user, repo), not per branch: the same person
// editing different branches of the same repo almost always cares about
// the same folders, and scoping by branch too would mean every new branch
// starts back at "everything collapsed" for no real benefit. Stored via
// the small generic user_setting JSON helpers (same mechanism
// company/ai.go's per-user AI settings use) — a handful of folder paths
// is nowhere near user_setting's practical size limit the way full file
// content would be (see company/workspace_tmp.go's own comment on why
// that one is filesystem-backed instead).
func userSettingExpandedFoldersKey(repoID int64) string {
	return fmt.Sprintf("company-workspace-expanded-folders:%d", repoID)
}

type workspaceExpandedFoldersRequest struct {
	Paths []string `json:"paths"`
}

// WorkspaceExpandedFolders returns which folders this person last left
// expanded in this repo's workspace sidebar — fetched once, client-side,
// right when the page loads, same timing as recoverPendingEdits.
// Mounted at /{owner}/{repo}/_edits_folders/{branch} alongside
// Workspace/WorkspaceSave — the {branch} segment in the URL is unused
// (kept only so this sits next to the other _edits* routes using the same
// URL shape) since the setting itself is branch-independent.
func WorkspaceExpandedFolders(ctx *context.Context) {
	if !requireWorkspaceWrite(ctx) {
		return
	}
	paths, err := user_model.GetUserSettingJSON(ctx, ctx.Doer.ID, userSettingExpandedFoldersKey(ctx.Repo.Repository.ID), []string{})
	if err != nil {
		ctx.ServerError("GetUserSettingJSON", err)
		return
	}
	ctx.JSON(http.StatusOK, map[string]any{"paths": paths})
}

// WorkspaceExpandedFoldersSave persists the sidebar's current expand/
// collapse state — fire-and-forget from the client (web_src/js/features/
// company-workspace.ts), same as the tmp-staging saves, so a slow save
// here never blocks clicking around the tree.
func WorkspaceExpandedFoldersSave(ctx *context.Context) {
	if !requireWorkspaceWrite(ctx) {
		return
	}
	var req workspaceExpandedFoldersRequest
	if err := json.NewDecoder(ctx.Req.Body).Decode(&req); err != nil {
		ctx.HTTPError(http.StatusBadRequest, "invalid request")
		return
	}
	if err := user_model.SetUserSettingJSON(ctx, ctx.Doer.ID, userSettingExpandedFoldersKey(ctx.Repo.Repository.ID), req.Paths); err != nil {
		ctx.ServerError("SetUserSettingJSON", err)
		return
	}
	ctx.JSON(http.StatusOK, map[string]any{"ok": true})
}
