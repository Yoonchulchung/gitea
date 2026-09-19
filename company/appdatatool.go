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
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".db") {
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

	// Browse mode. Cells rather than "rows", which the export mode already
	// uses for its table-to-count map.
	Tables    []BrowseTable  `json:"tables"`
	Columns   []string       `json:"columns"`
	Cells     [][]string     `json:"cells"`
	RowIDs    []int64        `json:"rowids"` // one per row of Cells; empty for a table without a rowid
	More      bool           `json:"more"`
	Schema    []BrowseColumn `json:"schema"`
	CreateSQL string         `json:"createSQL"`
	Indexes   []BrowseIndex  `json:"indexes"`
}

// BrowseColumn is one column as PRAGMA table_info describes it.
type BrowseColumn struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"notnull"`
	Default string `json:"default"`
	PK      bool   `json:"pk"`
	Auto    bool   `json:"-"` // an INTEGER PRIMARY KEY, which SQLite assigns
}

// BrowseIndex is one index on the table being looked at.
type BrowseIndex struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

// BrowseTable is one table as the data console lists it. Rows is -1 when it
// could not be counted — a view, or a table the app has broken.
type BrowseTable struct {
	Name string `json:"name"`
	Rows int    `json:"rows"`
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
	payload["snapshotDir"] = appSnapshotDirForProcess(dataDir)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	payloadFile, err := writeRunnerPayload(p, "datatool.json", body)
	if err != nil {
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
		[]string{"python3", "-", filepath.Join(appCtlForProcess(p), "datatool.json")})
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
	// Staged in the snapshot tree, which the app cannot reach, and renamed in:
	// a temporary name inside the data directory could be planted as a
	// symlink, and the copy would land wherever it pointed.
	tmp := filepath.Join(appSnapshotDirForApp(dataDir), ".restoring")
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
		st.AppendHistory(AppHistoryEntry{Status: st.Actual, Actor: actor, Reason: ReasonRestored})
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

// BrowseResult is one page of a table, plus what tables exist.
type BrowseResult struct {
	Tables    []BrowseTable
	Table     string
	Columns   []string
	Rows      [][]string
	RowIDs    []int64 // parallel to Rows; empty when the rows cannot be addressed for editing
	Offset    int
	More      bool
	Schema    []BrowseColumn
	CreateSQL string
	Indexes   []BrowseIndex
}

// BrowseAppData reads an app's database without changing it.
//
// Separate from RunAdminSQL, and not behind APP_DATA_SQL_CONSOLE, because
// they are different acts. That console edits data: it needs the app stopped,
// it takes a snapshot first, and it is switched off by default. Looking is
// none of those things — the connection is opened read-only so the engine
// refuses a write whatever the statement says, which means it is safe while
// the app is serving requests, and an administrator answering "what does this
// app actually have in it" should not have to stop it to find out.
func BrowseAppData(ctx context.Context, owner, repo, table, statement string, offset int) (*BrowseResult, error) {
	payload := map[string]any{"mode": "browse", "limit": browsePageSize, "offset": offset}
	if statement = strings.TrimSpace(statement); statement != "" {
		payload["sql"] = statement
	} else if table != "" {
		payload["table"] = table
	}
	result, err := runDataTool(ctx, owner, repo, payload)
	if err != nil {
		// The table listing still comes back on a failed query, so the page
		// can show what is there alongside the error.
		if result != nil {
			// "no such column: naem" is the reader's own typo and the only
			// useful thing to say back, whichever audience is reading — so it
			// is shown rather than replaced by the generic sentence. Redacted
			// anyway: an error raised while opening the file names its path.
			return &BrowseResult{Tables: result.Tables, Table: table, Offset: offset},
				userErrorf("%s", RedactServerPaths(result.Error))
		}
		return nil, err
	}
	return &BrowseResult{
		Tables: result.Tables, Table: table, Columns: result.Columns,
		Rows: result.Cells, RowIDs: result.RowIDs, Offset: offset, More: result.More,
		Schema: result.Schema, CreateSQL: result.CreateSQL, Indexes: result.Indexes,
	}, nil
}

// browsePageSize is one screenful. Small on purpose: this is for looking at
// data, and anyone who needs all of it wants the export instead.
const browsePageSize = 50

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
	// In the snapshot tree, which the helper may write and the app cannot
	// reach: written into the run directory, the app could replace what the
	// helper produced with symlinks to the server's own files, and the walk
	// below would zip those up for the administrator to download.
	outDir := filepath.Join(appSnapshotDirForApp(dataDir), ".export")
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
		"out":  appSnapshotDirForProcess(dataDir) + "/.export",
		"db":   appSnapshotDirForProcess(dataDir) + "/" + snapshot,
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
	if release, err := os.Readlink(appPathsFor(owner, repo).current); err == nil {
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
	// Never through a symlink: the bundle leaves the server, and a link to
	// app.ini would leave with it.
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
	}
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
import csv, json, os, re, sqlite3, sys, urllib.parse

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

def cell(v, full=False):
    # Rendered as text, never as the value's own repr: a BLOB is usually an
    # image or a hash and printing it fills the page with bytes, and a long
    # text column would push every other column off the screen — except
    # when the value is about to be edited or exported, which needs all of it.
    if v is None:
        return ""
    if isinstance(v, (bytes, bytearray, memoryview)):
        return "<%d bytes>" % len(bytes(v))
    s = v if isinstance(v, str) else str(v)
    return s if full or len(s) <= 200 else s[:200] + "\u2026"

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
    if mode == "browse":
        # Read-only enforced by the engine, not by inspecting the statement:
        # deciding whether some SQL writes is a parser's job, and getting it
        # wrong here means an admin browsing data silently changed it. The
        # fallback covers a database left with an uncheckpointed WAL, which a
        # mode=ro connection cannot always open.
        try:
            conn = sqlite3.connect("file:%s?mode=ro" % urllib.parse.quote(db), uri=True, timeout=30)
        except sqlite3.Error:
            conn = sqlite3.connect(db, timeout=30)
            conn.execute("PRAGMA query_only=1")
    else:
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

    if mode == "browse":
        # Read-only stops writes, not reads of other files: ATTACH would open
        # any database the process can reach and SELECT out of it, which on a
        # department's own page is an arbitrary file read. PRAGMA goes with it
        # because pragma_database_list answers with the server's paths and
        # browsing never needs one. Installed here rather than at connect
        # time, which is still setting this connection's own pragmas.
        ATTACH = getattr(sqlite3, "SQLITE_ATTACH", 24)
        DETACH = getattr(sqlite3, "SQLITE_DETACH", 25)
        PRAGMA = getattr(sqlite3, "SQLITE_PRAGMA", 19)
        # Named rather than "every pragma": the schema view needs table_info
        # and index_list, while database_list answers with the server's paths
        # and temp_store_directory writes one.
        readable = {"table_info", "table_xinfo", "index_list", "index_info", "foreign_key_list"}

        def guard(action, arg1, arg2, dbname, source):
            if action == PRAGMA:
                return 0 if (arg1 or "").lower() in readable else 1
            return 1 if action in (ATTACH, DETACH) else 0  # 1 = SQLITE_DENY

        conn.set_authorizer(guard)

        # The table list is always returned, so the page can show what is
        # there even when the asked-for table has since been dropped.
        listing = []
        for name in tables(conn):
            try:
                n = conn.execute('SELECT COUNT(*) FROM "%s"' % name.replace('"', '""')).fetchone()[0]
            except sqlite3.Error:
                n = -1  # a view or a table the app broke; still worth listing
            listing.append({"name": name, "rows": n})
        out["tables"] = listing

        limit = max(1, min(int(payload.get("limit") or 50), 200))
        offset = max(0, int(payload.get("offset") or 0))
        full = bool(payload.get("full"))
        sql, table = payload.get("sql"), payload.get("table")
        rowids = None
        if sql:
            cur = conn.execute(sql)
        elif table:
            if table not in [t["name"] for t in listing]:
                out["error"] = "no such table"
                return
            quoted = table.replace('"', '""')
            # The rowid comes along so a row can be edited or deleted by
            # identity rather than by matching its values. A WITHOUT ROWID
            # table has none, and its rows are shown but not editable.
            try:
                if payload.get("rowid") is not None:
                    cur = conn.execute('SELECT rowid, * FROM "%s" WHERE rowid = ?' % quoted, (int(payload["rowid"]),))
                else:
                    cur = conn.execute('SELECT rowid, * FROM "%s" LIMIT ? OFFSET ?' % quoted, (limit, offset))
                rowids = []
            except sqlite3.OperationalError:
                cur = conn.execute('SELECT * FROM "%s" LIMIT ? OFFSET ?' % quoted, (limit, offset))
        else:
            out["ok"] = True   # nothing asked for: the listing is the answer
            return
        columns = [d[0] for d in (cur.description or [])]
        rows = cur.fetchmany(limit)
        if rowids is not None:
            rowids = [r[0] for r in rows]
            rows = [r[1:] for r in rows]
            columns = columns[1:]
        out["columns"] = columns
        out["cells"] = [[cell(v, full) for v in row] for row in rows]
        out["rowids"] = rowids or []
        out["more"] = len(cur.fetchmany(1)) > 0

        # The shape of the table, alongside its contents. Reading a column
        # called "status" tells you nothing about whether it holds 0/1 or
        # "open"/"closed", and the CREATE statement is where the defaults, the
        # types and the constraints actually are.
        if table:
            quoted = table.replace('"', '""')
            out["schema"] = [
                {"name": r[1], "type": r[2] or "", "notnull": bool(r[3]),
                 "default": "" if r[4] is None else str(r[4]), "pk": bool(r[5])}
                for r in conn.execute('PRAGMA table_info("%s")' % quoted)
            ]
            row = conn.execute(
                "SELECT sql FROM sqlite_master WHERE name = ?", (table,)).fetchone()
            out["createSQL"] = (row[0] or "") if row else ""
            out["indexes"] = [
                {"name": r[0], "sql": r[1] or ""}
                for r in conn.execute(
                    "SELECT name, sql FROM sqlite_master WHERE type='index' AND tbl_name = ?"
                    " AND name NOT LIKE 'sqlite_%' ORDER BY name", (table,))
            ]
        out["ok"] = True
        return

    if mode == "edit":
        # Statements the platform built from a form, with every value a
        # parameter. Guarded all the same: nothing here may open another file.
        ATTACH, DETACH = getattr(sqlite3, "SQLITE_ATTACH", 24), getattr(sqlite3, "SQLITE_DETACH", 25)
        PRAGMA = getattr(sqlite3, "SQLITE_PRAGMA", 19)
        conn.set_authorizer(lambda action, *_: 1 if action in (ATTACH, DETACH, PRAGMA) else 0)
        cur = conn.cursor()
        cur.execute("BEGIN")
        try:
            changed = 0
            for st in payload["statements"]:
                cur.execute(st["sql"], st.get("params") or [])
                changed += cur.rowcount if cur.rowcount and cur.rowcount > 0 else 0
            out["changed"] = changed
            cur.execute("COMMIT")
        except Exception:
            cur.execute("ROLLBACK")
            raise
        out["ok"] = True
        return

    if mode == "dump":
        # Every row of one table, untruncated, for a file someone will edit
        # in a spreadsheet and bring back. Capped so a runaway table cannot
        # hold the helper for minutes.
        table = payload["table"]
        if table not in tables(conn):
            out["error"] = "no such table"
            return
        cur = conn.execute('SELECT * FROM "%s" LIMIT 200001' % table.replace('"', '""'))
        out["columns"] = [d[0] for d in (cur.description or [])]
        rows = cur.fetchall()
        out["more"] = len(rows) > 200000
        out["cells"] = [[cell(v, True) for v in row] for row in rows[:200000]]
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
                    # A cell starting with = + - @ is a formula to a spreadsheet,
                    # and these files are opened in one by an administrator.
                    writer.writerow([("'" + v) if isinstance(v, str) and v[:1] in "=+-@\t\r" else v for v in row])
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
