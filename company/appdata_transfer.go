// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"gitea.dev/modules/log"
	gitea_context "gitea.dev/services/context"
)

// A snapshot is a plain SQLite file, so it can leave the platform and come
// back — from this app, from another one, or from any SQLite tool.

const sqliteMagic = "SQLite format 3\x00"

// serveSnapshot sends one snapshot as a file any SQLite tool opens.
func serveSnapshot(ctx *gitea_context.Context, owner, repo string) {
	dataDir, ok := AppDataDirIfPresent(ctx, owner, repo)
	if !ok {
		ctx.NotFound()
		return
	}
	name := ctx.FormString("name")
	path, err := resolveSnapshot(dataDir, name)
	if err != nil {
		ctx.NotFound()
		return
	}
	f, err := os.Open(path)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	ctx.Resp.Header().Set("Content-Type", "application/vnd.sqlite3")
	ctx.Resp.Header().Set("Cache-Control", "no-store") // personal data may be in it, same as the export bundle
	ctx.Resp.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s-%s"`, owner, repo, name))
	http.ServeContent(ctx.Resp, ctx.Req, "", info.ModTime(), f)
}

// ImportSnapshot adds an uploaded SQLite file to the app's snapshots, to be
// put in place with the usual restore. The file is only copied here; the
// sandboxed helper is what opens it (docs/company/app-data.md §3c).
func ImportSnapshot(ctx context.Context, owner, repo string, r io.Reader) (string, error) {
	dataDir, err := appDataForStart(owner, repo)
	if err != nil {
		return "", err
	}
	if dataDir == "" {
		return "", userKeyError("company.err.data_disabled")
	}
	dir := appSnapshotDirForApp(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// ".partial" so pruneSnapshots clears one an interrupted upload leaves behind.
	staged, err := os.CreateTemp(dir, ".upload-*.partial")
	if err != nil {
		return "", err
	}
	defer os.Remove(staged.Name())

	limit := appDataQuotaBytes(SettingsFor(owner, repo))
	n, err := io.Copy(staged, io.LimitReader(r, limit+1))
	if cerr := staged.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n > limit {
		return "", userKeyError("company.err.upload_too_large", limit/(1024*1024))
	}
	if !hasSQLiteHeader(staged.Name()) {
		return "", userKeyError("company.err.upload_not_sqlite")
	}

	result, err := runDataTool(ctx, owner, repo, map[string]any{
		"mode": "adopt",
		"src":  filepath.Join(appSnapshotDirForProcess(dataDir), filepath.Base(staged.Name())),
		"name": snapshotName("upload"),
	})
	if err != nil {
		if result != nil {
			log.Info("company: %s/%s: uploaded snapshot refused: %v", owner, repo, err)
			return "", userKeyError("company.err.upload_not_sqlite")
		}
		return "", err
	}
	pruneSnapshots(dataDir)
	return filepath.Base(result.Snapshot), nil
}

func hasSQLiteHeader(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	header := make([]byte, len(sqliteMagic))
	_, err = io.ReadFull(f, header)
	return err == nil && bytes.Equal(header, []byte(sqliteMagic))
}

func importUploadedSnapshot(ctx *gitea_context.Context, owner, repo string) (string, error) {
	file, _, err := ctx.Req.FormFile("file")
	if err != nil {
		return "", userKeyError("company.err.upload_missing")
	}
	defer file.Close()
	return ImportSnapshot(ctx, owner, repo, file)
}

// AppDataSnapshot is a department's download of one of its own snapshots.
func AppDataSnapshot(ctx *gitea_context.Context) {
	serveSnapshot(ctx, ctx.Repo.Owner.Name, ctx.Repo.Repository.Name)
}

func AdminAppDataSnapshot(ctx *gitea_context.Context) {
	st, _, ok := adminAppContext(ctx)
	if !ok {
		return
	}
	serveSnapshot(ctx, st.Owner, st.Repo)
}
