// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
)

// Everything an administrator can do to an app's data by hand.
//
// One rule shapes the whole file: the *contents* of that database were
// written by a department's app, and parsing them inside Gitea's own process
// would walk the untrusted data straight through the boundary the sandbox
// exists to hold. So anything that reads SQL structure runs in the sandbox,
// with the app's own interpreter and the app's own limits, and only bytes
// cross back. Copying a file is the exception, because copying does not
// parse. See docs/company/app-data.md §8.

// Snapshot is one saved copy of an app's database.
type Snapshot struct {
	Name  string
	Bytes int64
	At    time.Time
}

// ListSnapshots is newest first, which is the order someone restoring wants.
func ListSnapshots(dataDir string) []Snapshot {
	entries, err := os.ReadDir(filepath.Join(dataDir, appDataSnapshotDir))
	if err != nil {
		return nil
	}
	var out []Snapshot
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, Snapshot{Name: entry.Name(), Bytes: info.Size(), At: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// snapshotName is always built here, never from anything a person typed.
// A caller-supplied name is how a path escapes the directory it belongs to.
func snapshotName(label string) string {
	return fmt.Sprintf("%d-%s.db", time.Now().Unix(), label)
}

// resolveSnapshot turns a name from a form back into a path, refusing
// anything that is not a plain file sitting in this app's snapshot directory.
func resolveSnapshot(dataDir, name string) (string, error) {
	if name == "" || name != filepath.Base(name) || !strings.HasSuffix(name, ".db") {
		return "", userKeyError("company.err.snapshot_unknown", name)
	}
	path := filepath.Join(dataDir, appDataSnapshotDir, name)
	info, err := os.Lstat(path) // Lstat: a symlink here would point out of the directory
	if err != nil || !info.Mode().IsRegular() {
		return "", userKeyError("company.err.snapshot_unknown", name)
	}
	return path, nil
}

// dataToolResult is what the sandboxed helper reports.
type dataToolResult struct {
	OK        bool             `json:"ok"`
	Error     string           `json:"error"`
	Snapshot  string           `json:"snapshot"`
	Integrity string           `json:"integrity"`
	Rows      map[string]int   `json:"rows"`
	Changed   int              `json:"changed"`
	Applied   []map[string]any `json:"applied"`
	Objects   []string         `json:"objects"`
}

// runDataTool executes one helper mode inside the app's sandbox.
func runDataTool(ctx context.Context, owner, repo string, payload map[string]any) (*dataToolResult, error) {
	dataDir, err := appDataForStart(owner, repo)
	if err != nil {
		return nil, err
	}
	if dataDir == "" {
		return nil, userKeyError("company.err.data_disabled")
	}
	p := appPathsFor(owner, repo)
	release, err := os.Readlink(p.current)
	if err != nil {
		// The helper runs on the release's interpreter, so an app that has
		// never deployed has nothing to run it with.
		return nil, errNoRelease
	}
	if err := os.MkdirAll(p.run, 0o700); err != nil {
		return nil, err
	}

	inSandbox := appDataDirForProcess(dataDir)
	payload["db"] = filepath.Join(inSandbox, appDataDBName)
	payload["snapshotDir"] = filepath.Join(inSandbox, appDataSnapshotDir)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	payloadFile := filepath.Join(p.run, "datatool.json")
	if err := os.WriteFile(payloadFile, body, 0o600); err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(payloadFile) }()

	runCtx, cancel := context.WithTimeout(ctx, migrateTimeout())
	defer cancel()

	settings := SettingsFor(owner, repo)
	cmd, err := buildSandboxCommand(release, p, settings, dataDir,
		[]string{"python3", "-", filepath.Join(appRunForProcess(p), "datatool.json")})
	if err != nil {
		return nil, err
	}
	cmd.Stdin = strings.NewReader(dataToolScript)
	cmd.Env = buildEnv(p, "/apps/"+owner+"/"+repo, dataDir, nil, false)

	out, runErr := runWithContext(runCtx, cmd)
	var result dataToolResult
	if err := json.Unmarshal(out, &result); err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("the data helper could not be run: %w", runErr)
		}
		return nil, fmt.Errorf("unreadable answer from the data helper: %w", err)
	}
	if !result.OK {
		return &result, fmt.Errorf("%s", result.Error)
	}
	return &result, nil
}

// CreateSnapshot takes a verified copy of the app's database. Safe while the
// app is running: a backup is a consistent point in the write stream, and
// uncommitted work is not in it.
func CreateSnapshot(ctx context.Context, owner, repo, label string) (string, error) {
	result, err := runDataTool(ctx, owner, repo, map[string]any{
		"mode": "snapshot",
		"name": snapshotName(label),
	})
	if err != nil {
		return "", err
	}
	dataDir, _ := appDataForStart(owner, repo)
	pruneSnapshots(dataDir)
	return filepath.Base(result.Snapshot), nil
}

// RestoreSnapshot puts a saved copy back.
//
// A file copy, not a query, so it happens here rather than in the sandbox.
// Two things make it safe and both are easy to forget: the app has to be
// stopped, because swapping the file under a live writer is corruption; and
// the -wal and -shm files have to go, because SQLite would otherwise replay
// a write-ahead log belonging to the database that was just replaced.
func RestoreSnapshot(ctx context.Context, owner, repo, name, actor string) error {
	if IsAppRunning(owner, repo) {
		return userKeyError("company.err.restore_running")
	}
	dataDir, err := appDataForStart(owner, repo)
	if err != nil {
		return err
	}
	source, err := resolveSnapshot(dataDir, name)
	if err != nil {
		return err
	}
	// Restoring is itself destructive, so the thing being replaced is saved
	// first. Without this, "I restored the wrong one" has no way back.
	if _, err := CreateSnapshot(ctx, owner, repo, "before-restore"); err != nil {
		return fmt.Errorf("could not save the current data before restoring: %w", err)
	}

	db := filepath.Join(dataDir, appDataDBName)
	tmp := db + ".restoring"
	if err := copyFile(source, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, db); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	for _, sidecar := range []string{db + "-wal", db + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("the replaced write-ahead log could not be removed: %w", err)
		}
	}
	log.Info("company: %s/%s: data restored from %s by %s", owner, repo, name, actor)
	return MutateAppState(owner, repo, func(st *AppState) bool {
		st.AppendHistory(AppHistoryEntry{Status: st.Actual, Actor: actor, Reason: "data restored from " + name})
		return true
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// AdminSQLResult is what came back from a statement an administrator ran.
type AdminSQLResult struct {
	Changed  int
	Snapshot string
}

// RunAdminSQL executes one administrator-written statement against an app's
// database.
//
// The app must be stopped and a snapshot is taken first, automatically. That
// pairing is the whole safety story: the platform cannot know whether a
// statement is right, so instead it guarantees there is a way back from one
// that was wrong, and that nothing is serving requests while it runs.
func RunAdminSQL(ctx context.Context, owner, repo, statement, actor string) (*AdminSQLResult, error) {
	if companySetting("APP_DATA_SQL_CONSOLE") != "true" {
		return nil, userKeyError("company.err.sql_console_off")
	}
	if strings.TrimSpace(statement) == "" {
		return nil, userKeyError("company.err.sql_empty")
	}
	if IsAppRunning(owner, repo) {
		return nil, userKeyError("company.err.sql_running")
	}
	snapshot, err := CreateSnapshot(ctx, owner, repo, "before-sql")
	if err != nil {
		return nil, fmt.Errorf("could not save the data before running SQL: %w", err)
	}

	result, err := runDataTool(ctx, owner, repo, map[string]any{"mode": "sql", "sql": statement})
	if err != nil {
		return nil, err
	}
	log.Info("company: %s/%s: %s ran admin SQL, %d row(s) changed (snapshot %s)",
		owner, repo, actor, result.Changed, snapshot)
	_ = MutateAppState(owner, repo, func(st *AppState) bool {
		st.AppendHistory(AppHistoryEntry{
			Status: st.Actual, Actor: actor,
			Reason: fmt.Sprintf("ran SQL against the app data (%d row(s) changed)", result.Changed),
		})
		return true
	})
	return &AdminSQLResult{Changed: result.Changed, Snapshot: snapshot}, nil
}

// WriteExportBundle streams a zip of everything needed to take this app's
// data elsewhere.
//
// Built from a snapshot rather than the live database: the snapshot is
// already a consistent point, and nothing here has to hold a lock on an app
// that may be serving.
func WriteExportBundle(ctx context.Context, owner, repo string, w io.Writer) error {
	snapshot, err := CreateSnapshot(ctx, owner, repo, "export")
	if err != nil {
		return err
	}
	dataDir, err := appDataForStart(owner, repo)
	if err != nil {
		return err
	}
	p := appPathsFor(owner, repo)
	outDir := filepath.Join(p.run, "export")
	_ = os.RemoveAll(outDir)
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(outDir) }()

	result, err := runDataTool(ctx, owner, repo, map[string]any{
		"mode": "export",
		"out":  filepath.Join(appRunForProcess(p), "export"),
	})
	if err != nil {
		return err
	}

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	snapshotPath, err := resolveSnapshot(dataDir, snapshot)
	if err != nil {
		return err
	}
	if err := addFileToZip(zw, "app.db", snapshotPath); err != nil {
		return err
	}
	// Whatever the helper produced, under the names it produced them with.
	_ = filepath.WalkDir(outDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil //nolint:nilerr // a missing optional file is not a failed export
		}
		rel, relErr := filepath.Rel(outDir, path)
		if relErr != nil {
			return nil
		}
		if err := addFileToZip(zw, filepath.ToSlash(rel), path); err != nil {
			log.Warn("company: export: adding %s: %v", rel, err)
		}
		return nil
	})
	// The migrations as they were applied, so the schema's history travels
	// with the rows rather than only its end state.
	if release, err := os.Readlink(p.current); err == nil {
		if files, err := loadMigrations(filepath.Join(release, "app")); err == nil {
			for _, m := range files {
				name := fmt.Sprintf("migrations/%03d_%s.sql", m.Version, m.Name)
				if err := addBytesToZip(zw, name, []byte(m.SQL)); err != nil {
					return err
				}
			}
		}
	}

	manifest, err := json.MarshalIndent(map[string]any{
		"owner":      owner,
		"repo":       repo,
		"exportedAt": time.Now().UTC().Format(time.RFC3339),
		"integrity":  result.Integrity,
		"rows":       result.Rows,
		"objects":    result.Objects,
		"migrations": result.Applied,
	}, "", "  ")
	if err != nil {
		return err
	}
	return addBytesToZip(zw, "manifest.json", manifest)
}

func addFileToZip(zw *zip.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	entry, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, f)
	return err
}

func addBytesToZip(zw *zip.Writer, name string, body []byte) error {
	entry, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = entry.Write(body)
	return err
}

// The helper. Runs in the app's sandbox with the app's interpreter, because
// what it parses was written by the app.
const dataToolScript = `
import csv, json, os, sqlite3, sys

def objects(conn):
    return ["%s %s" % (t, n) for t, n in conn.execute(
        "SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name")]

def tables(conn):
    return [r[0] for r in conn.execute(
        "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")]

def run(out):
    payload = json.load(open(sys.argv[1]))
    mode, db = payload["mode"], payload["db"]
    if not os.path.exists(db):
        out["error"] = "this app has no database yet"
        return
    conn = sqlite3.connect(db, timeout=30, isolation_level=None)
    conn.execute("PRAGMA busy_timeout=30000")

    if mode == "snapshot":
        os.makedirs(payload["snapshotDir"], exist_ok=True)
        path = os.path.join(payload["snapshotDir"], payload["name"])
        dst = sqlite3.connect(path)
        conn.backup(dst)                      # pages=-1: one shot, never restarts
        verdict = dst.execute("PRAGMA integrity_check").fetchone()[0]
        dst.close()
        if verdict != "ok":
            os.remove(path)
            out["error"] = "the snapshot failed its integrity check: %s" % verdict
            return
        out["snapshot"], out["integrity"] = path, verdict
        out["ok"] = True
        return

    if mode == "sql":
        cur = conn.cursor()
        cur.execute("BEGIN")
        try:
            cur.execute(payload["sql"])
            out["changed"] = cur.rowcount if cur.rowcount and cur.rowcount > 0 else 0
            cur.execute("COMMIT")
        except Exception:
            cur.execute("ROLLBACK")
            raise
        out["ok"] = True
        return

    if mode == "export":
        outDir = payload["out"]
        os.makedirs(os.path.join(outDir, "tables"), exist_ok=True)
        with open(os.path.join(outDir, "dump.sql"), "w", encoding="utf-8") as fh:
            for line in conn.iterdump():
                fh.write(line + "\n")
        counts = {}
        for name in tables(conn):
            rows = conn.execute('SELECT * FROM "%s"' % name.replace('"', '""'))
            counts[name] = 0
            with open(os.path.join(outDir, "tables", name + ".csv"), "w", encoding="utf-8", newline="") as fh:
                writer = csv.writer(fh)
                writer.writerow([d[0] for d in rows.description])
                for row in rows:
                    writer.writerow(row)
                    counts[name] += 1
        out["rows"] = counts
        out["objects"] = objects(conn)
        out["integrity"] = conn.execute("PRAGMA integrity_check").fetchone()[0]
        try:
            out["applied"] = [
                {"version": v, "name": n, "checksum": c, "appliedAt": a, "sha": s, "destructive": bool(d)}
                for v, n, c, a, s, d in conn.execute(
                    "SELECT version,name,checksum,applied_at,sha,destructive FROM _schema_migrations ORDER BY version")]
        except sqlite3.Error:
            out["applied"] = []
        out["ok"] = True
        return

    out["error"] = "unknown mode %s" % mode

out = {"ok": False, "error": "", "snapshot": "", "integrity": "", "rows": {},
       "changed": 0, "applied": [], "objects": []}
try:
    run(out)
except Exception as e:
    out["ok"] = False
    if not out["error"]:
        out["error"] = str(e)
print(json.dumps(out))
`
