// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"path"

	"gitea.dev/modules/fileicon"
	"gitea.dev/modules/git"
	"gitea.dev/services/context"
)

// WorkspaceFileIcon returns the same per-extension file icon
// (modules/fileicon, the "material" icon set used by the native file
// tree) for a filename that doesn't exist in the repo yet — a brand-new
// tab (New File, an AI-created file, a dropped file) has nothing for the
// native tree-view endpoint (repo.TreeViewNodes) to match against, since
// that one only knows about paths actually in the git tree. Reuses the
// exact same icon-matching logic instead of guessing at one client-side,
// fed a synthetic entry (only BaseName/EntryMode matter for the lookup —
// no real blob is needed). Mounted at /{owner}/{repo}/_edits_icon/{branch}
// alongside the other _edits* actions; the {branch} segment is unused,
// kept only for URL-shape consistency with those.
func WorkspaceFileIcon(ctx *context.Context) {
	if !requireWorkspaceWrite(ctx) {
		return
	}
	name := ctx.Req.URL.Query().Get("name")
	if name == "" {
		ctx.HTTPError(http.StatusBadRequest, "name required")
		return
	}
	pool := fileicon.NewRenderedIconPool()
	iconHTML := fileicon.RenderEntryIconHTML(pool, &fileicon.EntryInfo{BaseName: path.Base(name), EntryMode: git.EntryModeBlob})
	ctx.JSON(http.StatusOK, map[string]any{"icon": string(iconHTML), "renderedIconPool": pool.IconSVGs})
}
