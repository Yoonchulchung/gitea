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
	entries, err := os.ReadDir(appSnapshotDirForApp(dataDir))
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
	path := filepath.Join(appSnapshotDirForApp(dataDir), name)
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
	// Files maps a table name to the CSV holding it. The two differ whenever
	// a table name cannot be a filename, which an app can arrange.
	Files map[string]string `json:"files"`
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
	if payload["db"] == nil {
		payload["db"] = filepath.Join(inSandbox, appDataDBName)
	}
	payload["snapshotDir"] = sandboxSnapshotPath

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
	snapshotDir := appSnapshotDirForApp(dataDir)
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return nil, err
	}
	cmd, err := buildSandboxCommand(release, p, settings, dataDir, snapshotDir,
		[]string{"python3", "-", filepath.Join(appRunForProcess(p), "datatool.json")})
	if err != nil {
		return nil, err
	}
	cmd.Stdin = strings.NewReader(dataToolScript)
	cmd.Env = buildEnv(p, "/apps/"+owner+"/"+repo, dataDir, nil, false)

	out, stderr, runErr := runWithContext(runCtx, cmd)
	var result dataToolResult
	if err := json.Unmarshal(out, &result); err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("the data helper could not be run: %w: %s", runErr, stderr)
		}
		return nil, fmt.Errorf("unreadable answer from the data helper: %w: %s", err, stderr)
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
	if dataDir, err := appDataForStart(owner, repo); err == nil {
		pruneSnapshots(dataDir)
	}
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
	db := filepath.Join(dataDir, appDataDBName)
	tmp := db + ".restoring"
	// The copy comes first, before anything else touches the directory. Taking
	// the before-restore snapshot first would run pruneSnapshots, and if the
	// admin picked the oldest snapshot — which is exactly why ten are kept —
	// pruning deletes the very file about to be read.
	if err := copyFile(source, tmp); err != nil {
		return err
	}
	// Restoring is itself destructive, so the thing being replaced is saved
	// before it is replaced. Without this, "I restored the wrong one" has no
	// way back.
	if _, err := CreateSnapshot(ctx, owner, repo, "before-restore"); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("could not save the current data before restoring: %w", err)
	}

	// Sidecars before the swap, not after. A WAL belonging to the database
	// being replaced would be replayed onto the one replacing it, and doing
	// this after the rename leaves exactly that state behind whenever the
	// removal fails or the process dies in between. Everything committed in
	// it is already inside the snapshot just taken.
	for _, sidecar := range []string{db + "-wal", db + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			_ = os.Remove(tmp)
			return fmt.Errorf("the write-ahead log could not be cleared before restoring: %w", err)
		}
	}
	if err := os.Rename(tmp, db); err != nil {
		_ = os.Remove(tmp)
		return err
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
	// Flushed before the rename that follows: without it a host crash can
	// leave the database path pointing at content that was never written,
	// with the original already gone.
	if err := out.Sync(); err != nil {
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

	// Read from the snapshot, not the live database. Otherwise the row
	// counts, the CSVs and the integrity check describe the app as it is
	// now while the app.db in the same zip is the copy taken a moment
	// earlier — a bundle that contradicts its own manifest.
	result, err := runDataTool(ctx, owner, repo, map[string]any{
		"mode": "export",
		"out":  filepath.Join(appRunForProcess(p), "export"),
		"db":   sandboxSnapshotPath + "/" + snapshot,
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
		"files":      result.Files,
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
import csv, json, os, re, sqlite3, sys

def csv_name(name, used):
    # The table name comes from sqlite_master, so it comes from whatever the
    # app created at runtime, and SQLite quoted identifiers accept anything.
    # "../escape" or "/absolute" would steer a platform-owned write out of the
    # export directory, and a legitimate "a/b" would crash the whole export.
    base = re.sub(r"[^A-Za-z0-9._-]", "_", name).strip(".") or "table"
    candidate, n = base, 1
    while candidate in used:
        candidate, n = "%s_%d" % (base, n), n + 1
    used.add(candidate)
    return candidate

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
        # Moved into place only once it has passed, so a backup that died
        # partway never appears in the list as something restorable.
        tmp = path + ".partial"
        dst = sqlite3.connect(tmp)
        conn.backup(dst)                      # pages=-1: one shot, never restarts
        verdict = dst.execute("PRAGMA integrity_check").fetchone()[0]
        dst.close()
        if verdict != "ok":
            os.remove(tmp)
            out["error"] = "the snapshot failed its integrity check: %s" % verdict
            return
        os.replace(tmp, path)
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
        counts, files, used = {}, {}, set()
        for name in tables(conn):
            rows = conn.execute('SELECT * FROM "%s"' % name.replace('"', '""'))
            counts[name] = 0
            leaf = csv_name(name, used) + ".csv"
            files[name] = "tables/" + leaf
            with open(os.path.join(outDir, "tables", leaf), "w", encoding="utf-8", newline="") as fh:
                writer = csv.writer(fh)
                writer.writerow([d[0] for d in rows.description])
                for row in rows:
                    writer.writerow(row)
                    counts[name] += 1
        out["rows"] = counts
        out["files"] = files
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
       "changed": 0, "applied": [], "objects": [], "files": {}}
try:
    run(out)
except Exception as e:
    out["ok"] = False
    if not out["error"]:
        out["error"] = str(e)
print(json.dumps(out))
`
