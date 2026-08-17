// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/modules/git"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
	pull_svc "gitea.dev/services/pull"
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

// RedirectToWorkspaceIfEmpty sends a brand-new, file-less repo straight to
// its own workspace editor instead of Gitea's native "empty repository"
// landing page (git clone instructions, a "create README" nudge, etc.) —
// none of that means anything to someone who's never touched git, and
// bouncing away to the owner's page instead (an earlier version of this
// function) left the repo completely unreachable through the UI — no
// path existed to ever add its first file. The editor itself used to
// panic against a zero-commit repo (no branch ref yet for native
// tree-list, repo/treelist.go's TreeList, to list) — company-workspace.ts's
// own tree-list fetch now tolerates that failure (treats it as "empty
// tree", same as any other empty folder) instead of relying on a native
// fix, so landing here works correctly.
// Mounted right before repo.Home on the bare repo route (routers/web/web.go)
// so ctx.Repo.Repository/.BranchName are already resolved (context.RepoAssignment
// + context.RepoRefByType, which sets BranchName to the repo's own
// DefaultBranch for an empty repo specifically — see services/context/repo.go).
// Admins keep the native page (git clone instructions etc.) — same "full
// native access" reasoning as everywhere else, see docs/company/repo-ui.md.
func RedirectToWorkspaceIfEmpty(ctx *context.Context) {
	if !ctx.Repo.Repository.IsEmpty || (ctx.Doer != nil && ctx.Doer.IsAdmin) || !ctx.Repo.Permission.CanWrite(unit.TypeCode) {
		return
	}
	ctx.Redirect(ctx.Repo.Repository.Link() + "/_edits/" + url.PathEscape(ctx.Repo.BranchName))
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
	ctx.Data["Title"] = string(ctx.Locale.Tr("company.workspace.title"))
	ctx.Data["Repo"] = ctx.Repo.Repository
	ctx.Data["Branch"] = ctx.Repo.BranchName
	// The breadcrumb's "back" link normally goes to the repo's own home
	// page — but for a still-empty repo that page just redirects right
	// back here (RedirectToWorkspaceIfEmpty), making "back" a dead loop.
	// Send it to the owner's dashboard instead in that case, same as
	// clicking away from a place there's nothing to go back to yet.
	if ctx.Repo.Repository.IsEmpty {
		ctx.Data["BackLink"] = ctx.Repo.Repository.Owner.DashboardLink()
	} else {
		ctx.Data["BackLink"] = ctx.Repo.Repository.Link()
	}
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
	// BaseSHA is the blob SHA this edit started from — captured
	// client-side from the raw-content endpoint's ETag header when the
	// file was opened (web_src/js/features/company-workspace.ts). Passed
	// straight through as ChangeRepoFile.SHA below, so Gitea's own
	// optimistic-lock check (services/repository/files/update.go's
	// handleCheckErrors) can tell whether someone else saved a newer
	// version of this exact file while it was being edited here. Left
	// empty for a file that never existed before this edit — nothing to
	// compare against, and handleCheckErrors already treats an empty SHA
	// as "no check needed" for a brand-new upload.
	BaseSHA string `json:"baseSha,omitempty"`
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
				SHA:       f.BaseSHA,
			})
			continue
		}
		if f.FromPath != "" && f.FromPath != f.Path {
			moved++
			files = append(files, &files_service.ChangeRepoFile{
				Operation:    "rename",
				FromTreePath: strings.TrimPrefix(f.FromPath, "/"),
				TreePath:     treePath,
				SHA:          f.BaseSHA,
			})
		}
		edited++
		files = append(files, &files_service.ChangeRepoFile{
			Operation:     "upload",
			TreePath:      treePath,
			ContentReader: strings.NewReader(f.Content),
			SHA:           f.BaseSHA,
		})
	}

	message := commitMessageFor(files, edited, deleted, moved)

	result, err := files_service.ChangeRepoFiles(ctx, target, ctx.Doer, &files_service.ChangeRepoFilesOptions{
		OldBranch: branch,
		NewBranch: branch,
		Message:   message,
		Files:     files,
	})
	if err != nil {
		// Someone else saved a newer version of one of these files while
		// this person was still editing it — report it as a conflict to
		// resolve instead of a generic failure, git-merge-conflict style
		// (see docs/company/ai-agent.md). Only the first conflicting path
		// is ever reported (that's as far as Gitea's own check gets before
		// stopping) — resolving it and saving again surfaces the next one,
		// if there is one.
		var shaErr pull_svc.ErrSHADoesNotMatch
		if errors.As(err, &shaErr) {
			conflict, cerr := buildWorkspaceConflict(ctx, target, branch, shaErr)
			if cerr != nil {
				ctx.ServerError("buildWorkspaceConflict", cerr)
				return
			}
			ctx.JSON(http.StatusConflict, map[string]any{"conflict": conflict})
			return
		}
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

	// The blob SHA every changed path now has becomes the new baseline for
	// each tab's *next* edit (OpenTab.baseSha) — without this, a
	// following Save would still send the pre-this-Save SHA, which no
	// longer matches what's on the branch (this Save itself moved it),
	// incorrectly flagging a conflict against the client's own change.
	newShas := make(map[string]string, len(result.Files))
	for _, f := range result.Files {
		if f != nil {
			newShas[f.Path] = f.SHA
		}
	}
	ctx.JSON(http.StatusOK, map[string]any{"ok": true, "shas": newShas})
}

// workspaceConflict is everything the client needs to show a git-conflict-
// style resolution view for one file: what's actually on the branch right
// now (content, blob SHA, the commit that put it there and when/who) — see
// WorkspaceSave's ErrSHADoesNotMatch handling above.
type workspaceConflict struct {
	Path          string `json:"path"`
	ServerContent string `json:"serverContent"`
	ServerSHA     string `json:"serverSha"`
	ServerMessage string `json:"serverMessage"`
	ServerAuthor  string `json:"serverAuthor"`
	ServerDate    int64  `json:"serverDate"` // unix seconds
	// BaseContent is the content of the blob this editor's stale copy
	// started from (shaErr.GivenSHA) — the common ancestor that lets the
	// client run a real 3-way merge and mark only genuinely conflicting
	// lines instead of the whole file (company-conflict.ts). Omitted when
	// that blob can't be read; the client then falls back to a 2-way diff.
	BaseContent string `json:"baseContent,omitempty"`
}

func buildWorkspaceConflict(ctx *context.Context, repo *repo_model.Repository, branch string, shaErr pull_svc.ErrSHADoesNotMatch) (*workspaceConflict, error) {
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, repo)
	if err != nil {
		return nil, err
	}
	commit, err := gitRepo.GetBranchCommit(ctx, branch)
	if err != nil {
		return nil, err
	}
	entry, err := commit.GetTreeEntryByPath(ctx, gitRepo, shaErr.Path)
	if err != nil {
		return nil, fmt.Errorf("GetTreeEntryByPath for conflicting path %s: %w", shaErr.Path, err)
	}
	blob := entry.Blob(gitRepo)
	content, err := blob.GetBlobBytes(ctx, blob.Size(ctx))
	if err != nil {
		return nil, err
	}
	lastCommit, err := commit.GetCommitByPath(ctx, gitRepo, shaErr.Path)
	if err != nil {
		return nil, err
	}
	var baseContent []byte
	if shaErr.GivenSHA != "" {
		// best-effort — the blob is normally still reachable through the
		// commit this editor opened from, but a missing one only costs the
		// client its 3-way merge, not the conflict UI itself
		if baseBlob, err := gitRepo.GetBlob(shaErr.GivenSHA); err == nil {
			baseContent, _ = baseBlob.GetBlobBytes(ctx, baseBlob.Size(ctx))
		}
	}
	return &workspaceConflict{
		Path:          shaErr.Path,
		ServerContent: string(content),
		ServerSHA:     shaErr.CurrentSHA,
		ServerMessage: lastCommit.CommitMessage.MessageTitle(),
		ServerAuthor:  lastCommit.Author.Name,
		ServerDate:    lastCommit.Author.When.Unix(),
		BaseContent:   string(baseContent),
	}, nil
}

// commitMessageFor summarizes one save as a single commit message —
// counted separately since "3 files edited" would be misleading if one of
// those three was actually moved or removed. edited double-counts moved
// files (each move also emits an upload entry), so it's not used to
// report a moved file as also "edited" here. Deliberately plain, fixed
// English regardless of the person's own UI language — unlike page text,
// a commit message becomes permanent shared history everyone reads
// afterward, not something to render differently per viewer.
func commitMessageFor(files []*files_service.ChangeRepoFile, edited, deleted, moved int) string {
	edited -= moved
	var parts []string
	if edited == 1 {
		parts = append(parts, firstTreePathFor(files, "upload")+" edited")
	} else if edited > 0 {
		parts = append(parts, fmt.Sprintf("%d files edited", edited))
	}
	if moved == 1 {
		parts = append(parts, firstTreePathFor(files, "rename")+" moved")
	} else if moved > 0 {
		parts = append(parts, fmt.Sprintf("%d files moved", moved))
	}
	if deleted == 1 {
		parts = append(parts, firstTreePathFor(files, "delete")+" deleted")
	} else if deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d files deleted", deleted))
	}
	if len(parts) == 0 {
		return "Edit"
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
