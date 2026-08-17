// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gitea.dev/models/unit"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
	files_service "gitea.dev/services/repository/files"
)

const tplWorkspace templates.TplName = "company/workspace"

// requireWorkspaceWrite is the shared gate for both handlers below — same
// "direct commit" semantics as the non-admin commit_form
// (custom/templates/repo/editor/commit_form.tmpl), so read-only access
// isn't enough. Mounted inside Gitea's own repo-code route group
// (routers/web/web.go, alongside _new/_edit/_upload), which has already
// run context.RepoAssignment + reqUnitCodeReader + a branch-parsing
// context.RepoRefByType — ctx.Repo.Repository/.Permission/.BranchName are
// populated for free, no separate owner/repo lookup needed here.
func requireWorkspaceWrite(ctx *context.Context) bool {
	if !ctx.Repo.Permission.CanWrite(unit.TypeCode) {
		ctx.NotFound(nil)
		return false
	}
	return true
}

// Workspace renders the multi-file editor shell for the branch named in
// the URL (/{owner}/{repo}/_edits/{branch}). The file tree and each open
// file's content are fetched client-side from Gitea's own existing routes
// (/{owner}/{repo}/tree-list/branch/{branch} and
// /{owner}/{repo}/raw/{branch}/{path}, both already reachable through
// company/gate.go's isRepoScopedAllow) — this handler just hands the page
// the repo/branch to point that JS at, and reuses Gitea's own bundled
// CodeMirror setup (web_src/js/modules/codeeditor) for the actual editing,
// so there's no new editor to build or new frontend dependency to add.
// See docs/company/repo-ui.md.
func Workspace(ctx *context.Context) {
	if !requireWorkspaceWrite(ctx) {
		return
	}
	ctx.Data["Title"] = "편집"
	ctx.Data["Repo"] = ctx.Repo.Repository
	ctx.Data["Branch"] = ctx.Repo.BranchName
	// Chat sidebar (company/workspace_ai.go, web_src/js/features/company-ai-chat.ts)
	// only renders once this person has their own AI key saved
	// (/user/settings/ai) — see docs/company/ai-agent.md.
	ctx.Data["AIEnabled"] = AIConfiguredFor(ctx, ctx.Doer.ID)
	ctx.HTML(http.StatusOK, tplWorkspace)
}

type workspaceFile struct {
	Path     string `json:"path"`
	FromPath string `json:"fromPath,omitempty"` // set when this path was renamed/moved from FromPath since it was opened
	Content  string `json:"content"`
	Deleted  bool   `json:"deleted,omitempty"`
}

type workspaceSaveRequest struct {
	Files []workspaceFile `json:"files"`
}

// WorkspaceSave commits every changed, moved, and deleted file the editor
// is holding open in one commit, straight to the branch named in the URL —
// no new branch, no PR, same rule the simplified single-file editor
// follows. Non-delete entries use operation "upload" (not
// "update"/"create"): "update" requires the path to already exist
// (services/repository/files/update.go's handleCheckErrors errors out
// otherwise) and "create" rejects a path that already exists — "upload"
// is the one operation that upserts either way, which is what lets the
// frontend not have to track which open tabs are new files. A moved file
// additionally gets a "rename" entry ahead of its upload, so the old path
// is actually removed rather than left behind as an orphan copy.
func WorkspaceSave(ctx *context.Context) {
	if !requireWorkspaceWrite(ctx) {
		return
	}
	target := ctx.Repo.Repository
	branch := ctx.Repo.BranchName

	var req workspaceSaveRequest
	if err := json.NewDecoder(ctx.Req.Body).Decode(&req); err != nil {
		ctx.HTTPError(http.StatusBadRequest, "invalid request")
		return
	}
	if len(req.Files) == 0 {
		ctx.HTTPError(http.StatusBadRequest, "no files to save")
		return
	}

	files := make([]*files_service.ChangeRepoFile, 0, len(req.Files)+1)
	var edited, deleted, moved int
	for _, f := range req.Files {
		treePath := strings.TrimPrefix(f.Path, "/")
		if treePath == "" {
			ctx.HTTPError(http.StatusBadRequest, "empty file path")
			return
		}
		if f.Deleted {
			deleted++
			files = append(files, &files_service.ChangeRepoFile{
				Operation: "delete",
				TreePath:  treePath,
			})
			continue
		}
		if f.FromPath != "" && f.FromPath != f.Path {
			moved++
			files = append(files, &files_service.ChangeRepoFile{
				Operation:    "rename",
				FromTreePath: strings.TrimPrefix(f.FromPath, "/"),
				TreePath:     treePath,
			})
		}
		edited++
		files = append(files, &files_service.ChangeRepoFile{
			Operation:     "upload",
			TreePath:      treePath,
			ContentReader: strings.NewReader(f.Content),
		})
	}

	message := commitMessageFor(files, edited, deleted, moved)

	if _, err := files_service.ChangeRepoFiles(ctx, target, ctx.Doer, &files_service.ChangeRepoFilesOptions{
		OldBranch: branch,
		NewBranch: branch,
		Message:   message,
		Files:     files,
	}); err != nil {
		ctx.ServerError("ChangeRepoFiles", err)
		return
	}

	// This Save just became the authoritative content for every path
	// involved — any staged draft (company/workspace_tmp.go) for one of
	// them is now not just redundant but actively stale, so it's cleared
	// rather than left to linger until its own expiry. Server clock, not
	// anything client-supplied: it only needs to be at or after whatever
	// CreatedAt an in-flight-at-click-time debounced tmp-write (fired just
	// before Save, still landing after it) could possibly carry, and this
	// request handling this Save is causally after that write was sent
	// from the very same browser tab.
	clearedAt := time.Now().UnixMilli()
	for _, f := range req.Files {
		clearTmpEntry(ctx.Doer.ID, target.ID, branch, strings.TrimPrefix(f.Path, "/"), clearedAt)
		if f.FromPath != "" && f.FromPath != f.Path {
			clearTmpEntry(ctx.Doer.ID, target.ID, branch, strings.TrimPrefix(f.FromPath, "/"), clearedAt)
		}
	}

	ctx.JSON(http.StatusOK, map[string]any{"ok": true})
}

// commitMessageFor summarizes one save as a single commit message —
// counted separately since "3개 파일 편집" would be misleading if one of
// those three was actually moved or removed. edited double-counts moved
// files (each move also emits an upload entry), so it's not used to
// report a moved file as also "edited" here.
func commitMessageFor(files []*files_service.ChangeRepoFile, edited, deleted, moved int) string {
	edited -= moved
	var parts []string
	if edited == 1 {
		parts = append(parts, firstTreePathFor(files, "upload")+" 편집")
	} else if edited > 0 {
		parts = append(parts, fmt.Sprintf("%d개 파일 편집", edited))
	}
	if moved == 1 {
		parts = append(parts, firstTreePathFor(files, "rename")+" 이동")
	} else if moved > 0 {
		parts = append(parts, fmt.Sprintf("%d개 파일 이동", moved))
	}
	if deleted == 1 {
		parts = append(parts, firstTreePathFor(files, "delete")+" 삭제")
	} else if deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d개 파일 삭제", deleted))
	}
	if len(parts) == 0 {
		return "편집"
	}
	return strings.Join(parts, ", ")
}

func firstTreePathFor(files []*files_service.ChangeRepoFile, op string) string {
	for _, f := range files {
		if f.Operation == op {
			return f.TreePath
		}
	}
	return ""
}
