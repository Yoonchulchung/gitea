// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
)

// Server-side staging for in-progress, not-yet-Saved edits — a real "tmp
// folder" under Gitea's own app data dir, not the browser. Every open
// tab's content is asynchronously (fire-and-forget, debounced) mirrored
// here as the person types; reopening the same branch later restores from
// here instead of losing whatever wasn't Saved yet. Cleared the moment
// the real Save actually lands (WorkspaceSave, company/workspace.go).
//
// This replaces the old localStorage-only draft feature (removed after it
// caused a real bug — see docs/company/ai-agent.md), but avoids its
// specific failure mode differently: that feature applied a draft
// whenever ANY tab opened, regardless of relevance. This one only ever
// gets consulted once, right after the page loads, to recover exactly
// what that person had in progress — never mid-session, and cleared the
// instant a real Save supersedes it, so there's no window for stale data
// to quietly outrank fresh content later.
//
// Deliberately filesystem-backed, not a DB table — per-request writes are
// just a file write (no schema, no migration, no query planner), and a
// tmp draft can be arbitrarily large without hitting a TEXT column's
// practical size limit the way models/user/setting.go's user_setting
// table would for a big file.

// workspaceTmpEntry is what's actually written to disk for one (user,
// repo, branch, path). CreatedAt is set client-side, at the moment the
// edit that produced this content happened — not server receipt time —
// specifically so out-of-order delivery (two async writes in flight,
// arriving out of send order) can be detected and the older one dropped
// rather than clobbering the newer: see saveTmpEntry's own check.
type workspaceTmpEntry struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"createdAt"` // unix milliseconds, client clock
	// Cleared marks this entry as an explicit clearTmpEntry sentinel, not
	// real staged content — kept as its own field rather than inferring
	// "cleared" from an empty Content, which a genuinely empty staged file
	// (a brand-new file created with no content yet) would satisfy just as
	// well, making it indistinguishable from a real clear and silently
	// unrecoverable after a refresh even though nothing ever cleared it.
	Cleared bool `json:"cleared,omitempty"`
	// BaseSha is the blob SHA the edit that produced this draft started
	// from (OpenTab.baseSha client-side; empty for a brand-new file).
	// Stored so recovery-after-refresh can still tell that someone else
	// saved a newer version while this draft sat here — without it, a
	// refresh would silently rebase the draft onto whatever's newest and
	// the next Save would overwrite that person's change with no conflict.
	BaseSha string `json:"baseSha,omitempty"`
	// Removed marks this entry as a staged FILE DELETION — the path itself
	// is proposed to go away on the next real Save, as opposed to Cleared
	// above (which means "nothing staged here, ignore entirely"). Added
	// because pendingDeletes (company-workspace.ts) used to live in the
	// browser tab's memory only: a delete queued but not yet Saved was
	// silently undone by a refresh, a gap that went unnoticed while every
	// delete required an explicit confirm-dialog click (naturally followed
	// by Save soon after) but became easy to hit once the AI could queue a
	// delete_file call mid-conversation with no such prompt in the way.
	// Content is meaningless for a Removed entry.
	Removed bool `json:"removed,omitempty"`
}

// workspaceTmpMaxAge is how long an orphaned tmp entry (page closed
// mid-edit, never returned to) survives before being treated as gone —
// long enough to matter (survive a multi-day break), short enough that
// disk usage from abandoned edits doesn't grow forever. Purged lazily,
// when listWorkspaceTmpEntries runs across an expired one, rather than a
// separate sweep job.
const workspaceTmpMaxAge = 7 * 24 * time.Hour

// workspaceTmpLocks serializes read-modify-write access to one entry's
// file — saveTmpEntry has to read the existing CreatedAt before deciding
// whether to accept an incoming write, and two of that same file's async
// writes could otherwise race each other between the read and the write.
// Keyed by the entry's own file path; entries are never removed from this
// map (bounded by how many distinct files this instance has ever staged
// a draft for in its lifetime — negligible for this scale, and simpler
// than reference-counted cleanup).
var (
	workspaceTmpLocksMu sync.Mutex
	workspaceTmpLocks   = map[string]*sync.Mutex{}
)

func workspaceTmpLockFor(file string) *sync.Mutex {
	workspaceTmpLocksMu.Lock()
	defer workspaceTmpLocksMu.Unlock()
	l, ok := workspaceTmpLocks[file]
	if !ok {
		l = &sync.Mutex{}
		workspaceTmpLocks[file] = l
	}
	return l
}

// workspaceTmpDir is where one (user, repo, branch)'s staged drafts live —
// one file per repo-relative path, named by its hash (paths can contain
// "/" and arbitrary characters; hashing sidesteps needing to recreate
// that whole directory structure or escape anything). The real path is
// stored inside the file itself, so listing just means reading every file
// in this directory.
func workspaceTmpDir(userID, repoID int64, branch string) string {
	branchHash := sha256.Sum256([]byte(branch))
	return filepath.Join(setting.AppDataPath, "tmp", "company-workspace",
		fmt.Sprintf("%d", userID), fmt.Sprintf("%d", repoID), hex.EncodeToString(branchHash[:])[:16])
}

func workspaceTmpFile(dir, path string) string {
	h := sha256.Sum256([]byte(path))
	return filepath.Join(dir, hex.EncodeToString(h[:])+".json")
}

// saveTmpEntry writes one file's staged content, but only if entry is at
// least as new as whatever's already there — an async write that got
// delayed in flight and arrives after a newer one for the same path is
// silently dropped rather than winning by arrival order.
func saveTmpEntry(userID, repoID int64, branch string, entry workspaceTmpEntry) error {
	if entry.Path == "" {
		return fmt.Errorf("path required")
	}
	dir := workspaceTmpDir(userID, repoID, branch)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file := workspaceTmpFile(dir, entry.Path)

	lock := workspaceTmpLockFor(file)
	lock.Lock()
	defer lock.Unlock()

	if existing, err := readTmpEntry(file); err == nil && existing.CreatedAt > entry.CreatedAt {
		return nil // a newer write already landed — this one arrived late, drop it
	}

	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file) // atomic on the same filesystem — readers never see a half-written file
}

func readTmpEntry(file string) (*workspaceTmpEntry, error) {
	body, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var entry workspaceTmpEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

// listTmpEntries returns every staged draft for (userID, repoID, branch),
// silently skipping (and deleting) anything past workspaceTmpMaxAge.
func listTmpEntries(userID, repoID int64, branch string) ([]workspaceTmpEntry, error) {
	dir := workspaceTmpDir(userID, repoID, branch)
	files, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-workspaceTmpMaxAge).UnixMilli()
	entries := make([]workspaceTmpEntry, 0, len(files))
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		full := filepath.Join(dir, f.Name())
		entry, err := readTmpEntry(full)
		if err != nil {
			continue // corrupt/partial file — ignore rather than fail the whole list
		}
		if entry.CreatedAt < cutoff {
			_ = os.Remove(full) // best-effort — a failed cleanup just gets retried next list
			continue
		}
		if entry.Cleared {
			continue // a clearTmpEntry sentinel (see below) — nothing to recover, not a real staged draft
		}
		entries = append(entries, *entry)
	}
	return entries, nil
}

// clearTmpEntry marks one path's staged draft cleared — called once a real
// Save actually lands for that path (WorkspaceSave, company/workspace.go)
// or someone explicitly discards a tab's changes (closeTab/deleteActiveFile,
// company-workspace.ts). clearedAt should be a timestamp guaranteed to be
// at or after any legitimate pre-clear write's own CreatedAt — the caller
// decides what that means for its own case (WorkspaceSave uses the
// server's own clock at the moment the save just succeeded; an explicit
// discard uses the browser's clock at the moment of discarding, same
// clock the debounced writes it needs to outrank were stamped with).
//
// This writes a Cleared sentinel through the normal saveTmpEntry path
// rather than deleting the file outright — deleting would leave nothing
// behind to reject a write that's still in flight at clear time (the
// debounced autosave fired moments before Save was clicked, lands after
// the clear) from silently resurrecting exactly the content Save just
// superseded. listTmpEntries above treats a Cleared entry as "nothing to
// recover," so this stays invisible to recovery either way — Content is
// left empty too, but that's incidental now, not what recovery actually
// checks (a real staged file can legitimately have empty content, e.g. a
// brand-new file created with nothing written to it yet).
func clearTmpEntry(userID, repoID int64, branch, path string, clearedAt int64) {
	_ = saveTmpEntry(userID, repoID, branch, workspaceTmpEntry{Path: path, Cleared: true, CreatedAt: clearedAt}) // best-effort — an orphaned real entry just ages out via workspaceTmpMaxAge instead
}

// ---- HTTP handlers ----

type workspaceTmpSaveRequest struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"createdAt"`
	BaseSha   string `json:"baseSha,omitempty"` // see workspaceTmpEntry.BaseSha
	// Deleted, when true, clears this path's staged draft outright instead
	// of writing one — an explicit "버릴까요?" discard (closeTab,
	// company-workspace.ts) needs this too, not just a Save: otherwise the
	// very content someone just chose to throw away would quietly come
	// back the next time they opened this branch. Not to be confused with
	// Removed below — Deleted discards a staged edit (there's no pending
	// change here anymore); Removed stages that the real file itself
	// should be deleted (there IS a pending change: "delete this").
	Deleted bool `json:"deleted,omitempty"`
	// Removed stages path itself as a pending file deletion — see
	// workspaceTmpEntry.Removed.
	Removed bool `json:"removed,omitempty"`
}

// WorkspaceTmpSave is the fire-and-forget target the editor's own debounced
// autosave posts to (web_src/js/features/company-workspace.ts) — a plain
// file write, no git involved, so it stays fast enough not to matter that
// the frontend never waits on it before letting someone keep typing or
// close the tab. Mounted at /{owner}/{repo}/_edits_tmp/{branch}, alongside
// Workspace/WorkspaceSave (routers/web/web.go).
func WorkspaceTmpSave(ctx *context.Context) {
	if !requireWorkspaceWrite(ctx) {
		return
	}
	var req workspaceTmpSaveRequest
	if err := json.NewDecoder(ctx.Req.Body).Decode(&req); err != nil {
		ctx.HTTPError(http.StatusBadRequest, "invalid request")
		return
	}
	if req.Deleted {
		clearTmpEntry(ctx.Doer.ID, ctx.Repo.Repository.ID, ctx.Repo.BranchName, req.Path, req.CreatedAt)
		ctx.JSON(http.StatusOK, map[string]any{"ok": true})
		return
	}
	if err := saveTmpEntry(ctx.Doer.ID, ctx.Repo.Repository.ID, ctx.Repo.BranchName, workspaceTmpEntry{
		Path: req.Path, Content: req.Content, CreatedAt: req.CreatedAt, BaseSha: req.BaseSha, Removed: req.Removed,
	}); err != nil {
		ctx.ServerError("saveTmpEntry", err)
		return
	}
	ctx.JSON(http.StatusOK, map[string]any{"ok": true})
}

// WorkspaceTmpList returns every staged draft for this person on this
// repo/branch — fetched once, client-side, right after the page loads
// (never mid-session), to recover whatever wasn't Saved before they last
// left.
func WorkspaceTmpList(ctx *context.Context) {
	if !requireWorkspaceWrite(ctx) {
		return
	}
	entries, err := listTmpEntries(ctx.Doer.ID, ctx.Repo.Repository.ID, ctx.Repo.BranchName)
	if err != nil {
		ctx.ServerError("listTmpEntries", err)
		return
	}
	ctx.JSON(http.StatusOK, map[string]any{"entries": entries})
}
