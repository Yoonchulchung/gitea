// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
)

// Schema changes are the platform's job, not the app's.
//
// An app that creates its own tables on import does it once per process, with
// no record of what ran, no way to tell a fresh database from a half-migrated
// one, and no moment at which a failure can stop the deploy. Migrations here
// are numbered files the platform applies before the release goes live, in a
// transaction each, recorded in the database they changed.
//
// The rule that makes rollback work is in the guard below: additive only.
// DeployVersion can build *any* commit in the repository's history
// (company/deployversion.go), so "the previous release still works" is not
// enough — every past release has to. A schema that only ever grows gives
// that for free, because code that does not know about a column cannot be
// broken by it. See docs/company/app-data.md §7.

const (
	migrationsSubdir      = "migrations"
	migrateTimeoutDefault = 120 * time.Second
	// The escape hatch, spelled out rather than inferred. A migration that
	// drops something is sometimes genuinely right; it just must not happen
	// because nobody noticed.
	destructiveMarker = "-- platform: destructive"
)

// migrationFile is one NNN_name.sql from the release.
type migrationFile struct {
	Version     int    `json:"version"`
	Name        string `json:"name"`
	SQL         string `json:"sql"`
	Checksum    string `json:"checksum"`
	Destructive bool   `json:"destructive"`
}

var migrationNamePattern = regexp.MustCompile(`^(\d{3,})_([A-Za-z0-9._-]+)\.sql$`)

// loadMigrations reads and validates the release's migration files.
func loadMigrations(appDir string) ([]migrationFile, error) {
	dir := filepath.Join(appDir, migrationsSubdir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil // an app with no schema is a normal app
	}
	if err != nil {
		return nil, err
	}

	var out []migrationFile
	seen := map[int]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		match := migrationNamePattern.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, userKeyError("company.err.migration_name", entry.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil || version <= 0 {
			return nil, userKeyError("company.err.migration_name", entry.Name())
		}
		if other, dup := seen[version]; dup {
			// Two files claiming one version means the order they run in
			// depends on how the directory happens to be listed.
			return nil, userKeyError("company.err.migration_duplicate", strconv.Itoa(version), other, entry.Name())
		}
		seen[version] = entry.Name()

		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		file := migrationFile{
			Version:     version,
			Name:        match[2],
			SQL:         string(body),
			Checksum:    migrationChecksum(body),
			Destructive: strings.Contains(string(body), destructiveMarker),
		}
		if err := guardMigration(entry.Name(), file); err != nil {
			return nil, err
		}
		out = append(out, file)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func migrationChecksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

var (
	dropTablePattern  = regexp.MustCompile(`(?is)\bdrop\s+table\b`)
	dropColumnPattern = regexp.MustCompile(`(?is)\bdrop\s+column\b`)
	renamePattern     = regexp.MustCompile(`(?is)\brename\s+(to|column)\b`)
	addColumnPattern  = regexp.MustCompile(`(?is)\badd\s+(column\s+)?`)
	notNullPattern    = regexp.MustCompile(`(?is)\bnot\s+null\b`)
	defaultPattern    = regexp.MustCompile(`(?is)\bdefault\b`)
	alterTablePattern = regexp.MustCompile(`(?is)\balter\s+table\b`)
)

// guardMigration refuses schema changes that would break an older release.
//
// The check reads SQL with regular expressions, which cannot be exact — so
// it is deliberately tuned to over-refuse. A false refusal costs one comment
// line (destructiveMarker) and an administrator's attention; a false pass
// costs a department their data the next time somebody deploys last month's
// commit to get out of trouble.
func guardMigration(filename string, file migrationFile) error {
	if file.Destructive {
		return nil // declared, and an admin approves it with the deploy request
	}
	body := stripSQLComments(file.SQL)

	switch {
	case dropTablePattern.MatchString(body):
		return userKeyError("company.err.migration_destructive", filename, "DROP TABLE")
	case dropColumnPattern.MatchString(body):
		return userKeyError("company.err.migration_destructive", filename, "DROP COLUMN")
	case renamePattern.MatchString(body):
		return userKeyError("company.err.migration_destructive", filename, "RENAME")
	}

	// A NOT NULL column added without a default has no value for the rows
	// that already exist — SQLite refuses it outright, and an older release
	// that does not write the column would fail on every insert even if it
	// did not.
	for statement := range strings.SplitSeq(body, ";") {
		if !alterTablePattern.MatchString(statement) || !addColumnPattern.MatchString(statement) {
			continue
		}
		if notNullPattern.MatchString(statement) && !defaultPattern.MatchString(statement) {
			return userKeyError("company.err.migration_not_null", filename)
		}
	}
	return nil
}

// stripSQLComments removes comments so the guard does not read the word
// "DROP" in a sentence explaining why nothing is dropped.
func stripSQLComments(sql string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(sql, "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	out := b.String()
	for {
		start := strings.Index(out, "/*")
		if start < 0 {
			return out
		}
		end := strings.Index(out[start:], "*/")
		if end < 0 {
			return out[:start]
		}
		out = out[:start] + " " + out[start+end+2:]
	}
}

func migrateTimeout() time.Duration {
	raw := companySetting("APP_DATA_MIGRATE_TIMEOUT")
	if raw == "" {
		return migrateTimeoutDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Warn("company: APP_DATA_MIGRATE_TIMEOUT=%q is not a duration; using %s", raw, migrateTimeoutDefault)
		return migrateTimeoutDefault
	}
	return d
}

// migrateResult is what the runner reports back.
type migrateResult struct {
	OK        bool     `json:"ok"`
	Error     string   `json:"error"`
	ErrorKind string   `json:"errorKind"`
	Applied   []int    `json:"applied"`
	Missing   []int    `json:"missing"`
	Snapshot  string   `json:"snapshot"`
	Added     []string `json:"added"`   // objects the app created outside migrations
	Removed   []string `json:"removed"` // objects that disappeared the same way
	Rows      int      `json:"rows"`
}

// runMigrations applies the release's schema changes to the app's database.
//
// Called before the symlink swap and before the old process is stopped: the
// changes are additive, so the release still serving cannot see them, and a
// failure here leaves that release running and untouched.
func runMigrations(ctx context.Context, owner, repo, release, dataDir string, settings AppSettings, sha string) (*migrateResult, error) {
	if dataDir == "" {
		return &migrateResult{OK: true}, nil // app data is switched off
	}
	files, err := loadMigrations(filepath.Join(release, "app"))
	if err != nil {
		return nil, err
	}

	p := appPathsFor(owner, repo)
	if err := os.MkdirAll(p.run, 0o700); err != nil {
		return nil, err
	}
	inSandbox := appDataDirForProcess(dataDir)
	payload, err := json.Marshal(map[string]any{
		"db":          filepath.Join(inSandbox, appDataDBName),
		"snapshotDir": filepath.Join(inSandbox, appDataSnapshotDir),
		"journalMode": DataJournalMode(),
		"migrations":  files,
		"sha":         sha,
	})
	if err != nil {
		return nil, err
	}
	// Through the run directory, which is scratch by design, rather than
	// through the data directory this is about to change.
	payloadFile := filepath.Join(p.run, "migrate.json")
	if err := os.WriteFile(payloadFile, payload, 0o600); err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(payloadFile) }()

	runCtx, cancel := context.WithTimeout(ctx, migrateTimeout())
	defer cancel()

	cmd, err := buildSandboxCommand(release, p, settings, dataDir,
		[]string{"python3", "-", filepath.Join(appRunForProcess(p), "migrate.json")})
	if err != nil {
		return nil, err
	}
	cmd.Stdin = strings.NewReader(migrateScript)
	cmd.Env = buildEnv(p, "/apps/"+owner+"/"+repo, dataDir, nil, false)

	out, runErr := runWithContext(runCtx, cmd)
	var result migrateResult
	if jsonErr := json.Unmarshal(out, &result); jsonErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("the schema migration could not be run: %w", runErr)
		}
		return nil, fmt.Errorf("unreadable answer from the schema migration: %w", jsonErr)
	}
	if !result.OK {
		return &result, migrateError(result)
	}

	if len(result.Removed) > 0 {
		// Not adopted, and not repaired: something the app deleted outside a
		// migration is the one case where guessing is worse than saying so.
		log.Error("company: %s/%s: schema objects disappeared outside migrations: %s",
			owner, repo, strings.Join(result.Removed, ", "))
	}
	if len(result.Added) > 0 {
		log.Info("company: %s/%s: adopted %d schema object(s) the app created itself: %s",
			owner, repo, len(result.Added), strings.Join(result.Added, ", "))
	}
	pruneSnapshots(dataDir)
	return &result, nil
}

// migrateError turns the runner's refusal into the right sentence for each
// audience — the two that stop a deploy are the ones worth naming precisely.
func migrateError(result migrateResult) error {
	switch result.ErrorKind {
	case "checksum":
		return audienceKeyError("company.err.migration_changed", "company.err.migration_changed.admin", result.Error)
	case "floor":
		return audienceKeyError("company.err.migration_floor", "company.err.migration_floor.admin", result.Error)
	default:
		return audienceKeyError("company.err.migration_failed", "company.err.migration_failed.admin", result.Error)
	}
}

// runWithContext runs a prepared command under a deadline.
//
// The command is already built (sandbox flags, binds, limits), so it cannot
// go through process.CommandContext — this adds the one thing that was
// missing, a migration that hangs must not hold the deploy forever.
func runWithContext(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil && stderr.Len() > 0 {
			return stdout.Bytes(), fmt.Errorf("%w: %s", err, lastLines(stderr.String(), 5))
		}
		return stdout.Bytes(), err
	case <-ctx.Done():
		// The whole group: a sandboxed run is bwrap plus what it started, and
		// killing only the parent leaves the migration running on the data.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done
		return stdout.Bytes(), ctx.Err()
	}
}

// pruneSnapshots keeps the snapshot directory inside both limits — a count
// and an age — because either one alone fails: a busy app would keep a
// hundred of them inside the age window, and a quiet one would keep its last
// ten forever.
func pruneSnapshots(dataDir string) {
	dir := filepath.Join(dataDir, appDataSnapshotDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type snap struct {
		path string
		at   time.Time
	}
	var snaps []snap
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		snaps = append(snaps, snap{filepath.Join(dir, entry.Name()), info.ModTime()})
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].at.After(snaps[j].at) })

	keep := companySettingPositiveInt("APP_DATA_SNAPSHOT_KEEP", appDataSnapshotKeepDefault)
	cutoff := time.Now().Add(-appDataRetention())
	for i, s := range snaps {
		if i < keep && s.at.After(cutoff) {
			continue
		}
		if err := os.Remove(s.path); err != nil {
			log.Warn("company: removing old snapshot %s: %v", filepath.Base(s.path), err)
		}
	}
}

// The runner. Python rather than Go because the interpreter is already there
// and its sqlite3 is the same one the app will use — two SQLite libraries
// that could disagree about a schema is a problem worth not having.
const migrateScript = `
import hashlib, json, os, sqlite3, sys, time

def fingerprint(conn):
    rows = conn.execute(
        "SELECT type, name, COALESCE(sql, '') FROM sqlite_master "
        "WHERE name NOT LIKE 'sqlite_%' AND name NOT LIKE '\\_schema\\_%' ESCAPE '\\' "
        "ORDER BY type, name").fetchall()
    h = hashlib.sha256()
    names = []
    for t, n, s in rows:
        h.update(("%s\x1f%s\x1f%s\x1e" % (t, n, s)).encode("utf-8"))
        names.append("%s %s" % (t, n))
    return h.hexdigest(), names

def run(out):
    payload = json.load(open(sys.argv[1]))
    db, journal = payload["db"], payload["journalMode"]
    present = {m["version"]: m for m in (payload["migrations"] or [])}

    os.makedirs(os.path.dirname(db), exist_ok=True)
    conn = sqlite3.connect(db, timeout=30, isolation_level=None)
    # Outside any transaction, and only these two: journal_mode is persisted
    # in the file, foreign_keys must be off for the table rebuild an ALTER
    # sometimes needs, and neither can be set once a transaction is open.
    conn.execute("PRAGMA journal_mode=%s" % journal)
    conn.execute("PRAGMA busy_timeout=30000")
    conn.execute("PRAGMA foreign_keys=OFF")
    conn.execute("CREATE TABLE IF NOT EXISTS _schema_migrations ("
                 "version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL,"
                 "applied_at INTEGER NOT NULL, sha TEXT NOT NULL, destructive INTEGER NOT NULL DEFAULT 0)")
    conn.execute("CREATE TABLE IF NOT EXISTS _schema_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)")

    applied = {r[0]: {"checksum": r[1], "destructive": r[2], "name": r[3]}
               for r in conn.execute("SELECT version, checksum, destructive, name FROM _schema_migrations")}

    # A file that changed after it was applied means the chain no longer
    # describes this database. Nothing below can be trusted, so stop.
    for v, rec in applied.items():
        m = present.get(v)
        if m and m["checksum"] != rec["checksum"]:
            out["errorKind"] = "checksum"
            out["error"] = "migration %03d_%s was edited after it had already been applied" % (v, rec["name"])
            return

    # This release is older than the database. Additive changes it has never
    # heard of are harmless; a destructive one is not.
    missing = sorted(v for v in applied if v not in present)
    out["missing"] = missing
    blocking = [v for v in missing if applied[v]["destructive"]]
    if blocking:
        out["errorKind"] = "floor"
        out["error"] = "this version predates schema change %03d_%s, which removed something it still expects" % (
            blocking[0], applied[blocking[0]]["name"])
        return

    before_fp, before_names = fingerprint(conn)
    recorded = conn.execute("SELECT value FROM _schema_state WHERE key='fingerprint'").fetchone()
    if recorded:
        known = json.loads(recorded[0])
        out["added"] = [n for n in before_names if n not in known]
        out["removed"] = [n for n in known if n not in before_names]

    pending = [present[v] for v in sorted(present) if v not in applied]

    if pending:
        os.makedirs(payload["snapshotDir"], exist_ok=True)
        path = os.path.join(payload["snapshotDir"],
                            "%d-%s.db" % (int(time.time()), (payload["sha"] or "unknown")[:12]))
        dst = sqlite3.connect(path)
        # Default pages=-1: one shot. Copying in chunks lets SQLite restart
        # the whole backup whenever the source is written, which on a busy
        # app need never finish.
        conn.backup(dst)
        verdict = dst.execute("PRAGMA integrity_check").fetchone()[0]
        dst.close()
        if verdict != "ok":
            os.remove(path)
            out["error"] = "the pre-migration snapshot failed its integrity check: %s" % verdict
            return
        out["snapshot"] = path

    for m in pending:
        try:
            conn.executescript(
                "BEGIN;\n" + m["sql"] +
                "\n;INSERT INTO _schema_migrations(version,name,checksum,applied_at,sha,destructive)"
                " VALUES(%d,'%s','%s',%d,'%s',%d);\nCOMMIT;" % (
                    m["version"], m["name"].replace("'", "''"), m["checksum"],
                    int(time.time()), (payload["sha"] or "").replace("'", "''"),
                    1 if m["destructive"] else 0))
        except Exception as e:
            try:
                conn.execute("ROLLBACK")
            except Exception:
                pass
            out["error"] = "migration %03d_%s failed: %s" % (m["version"], m["name"], e)
            return
        violations = conn.execute("PRAGMA foreign_key_check").fetchall()
        if violations:
            out["error"] = "migration %03d_%s left %d broken foreign key reference(s)" % (
                m["version"], m["name"], len(violations))
            return
        out["applied"].append(m["version"])

    after_fp, after_names = fingerprint(conn)
    conn.execute("INSERT INTO _schema_state(key,value) VALUES('fingerprint',?) "
                 "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (json.dumps(after_names),))
    conn.execute("INSERT INTO _schema_state(key,value) VALUES('fingerprintHash',?) "
                 "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (after_fp,))
    out["rows"] = len(after_names)
    conn.execute("PRAGMA foreign_keys=ON")
    conn.close()
    out["ok"] = True

out = {"ok": False, "error": "", "errorKind": "", "applied": [], "missing": [],
       "snapshot": "", "added": [], "removed": [], "rows": 0}
try:
    run(out)
except Exception as e:
    out["ok"] = False
    if not out["error"]:
        out["error"] = str(e)
print(json.dumps(out))
`

// migrateForDeploy is the deploy's view of the runner: resolve the data
// directory, apply what is pending, and write what happened into the build
// log the department reads.
func migrateForDeploy(ctx context.Context, owner, repo string, p appPaths, release, sha string, settings AppSettings) error {
	dataDir, err := appDataForStart(owner, repo)
	if err != nil {
		return err
	}
	result, err := runMigrations(ctx, owner, repo, release, dataDir, settings, sha)
	if err != nil {
		return err
	}
	if result == nil || (len(result.Applied) == 0 && len(result.Added) == 0 && len(result.Removed) == 0) {
		return nil
	}

	var note strings.Builder
	if len(result.Applied) > 0 {
		fmt.Fprintf(&note, "applied %d schema migration(s): %v", len(result.Applied), result.Applied)
		if result.Snapshot != "" {
			fmt.Fprintf(&note, "\nsnapshot taken and verified before applying: %s", filepath.Base(result.Snapshot))
		}
	}
	if len(result.Added) > 0 {
		fmt.Fprintf(&note, "\nadopted %d object(s) created outside migrations: %s",
			len(result.Added), strings.Join(result.Added, ", "))
	}
	if len(result.Removed) > 0 {
		fmt.Fprintf(&note, "\nWARNING: %d object(s) disappeared outside migrations: %s",
			len(result.Removed), strings.Join(result.Removed, ", "))
	}
	appendBuildLog(p, sha, "OK", note.String())
	return nil
}
