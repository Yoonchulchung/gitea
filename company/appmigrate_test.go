// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.dev/modules/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeMigrations(t *testing.T, files map[string]string) string {
	t.Helper()
	appDir := t.TempDir()
	dir := filepath.Join(appDir, migrationsSubdir)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	return appDir
}

// DeployVersion can build any commit in the repository's history, so "the
// previous release still works" is not enough — every past release has to.
// Only an additive schema gives that, so the guard refuses the rest.
func TestGuardRefusesWhatBreaksOlderReleases(t *testing.T) {
	for name, sql := range map[string]string{
		"001_drop_table.sql":  "DROP TABLE records;",
		"002_drop_column.sql": "ALTER TABLE records DROP COLUMN note;",
		"003_rename.sql":      "ALTER TABLE records RENAME TO entries;",
		"004_rename_col.sql":  "ALTER TABLE records RENAME COLUMN a TO b;",
		"005_not_null.sql":    "ALTER TABLE records ADD COLUMN owner TEXT NOT NULL;",
	} {
		_, err := loadMigrations(writeMigrations(t, map[string]string{name: sql}))
		assert.Error(t, err, "%s must be refused", name)
	}
}

func TestGuardAllowsAdditiveAndDeclaredDestructive(t *testing.T) {
	appDir := writeMigrations(t, map[string]string{
		"001_create.sql":   "CREATE TABLE records (id INTEGER PRIMARY KEY, body TEXT NOT NULL);",
		"002_add.sql":      "ALTER TABLE records ADD COLUMN note TEXT;",
		"003_add_dflt.sql": "ALTER TABLE records ADD COLUMN kind TEXT NOT NULL DEFAULT 'x';",
		"004_declared.sql": destructiveMarker + "\nDROP TABLE records;",
	})
	files, err := loadMigrations(appDir)
	require.NoError(t, err)
	require.Len(t, files, 4)
	assert.Equal(t, []int{1, 2, 3, 4}, []int{files[0].Version, files[1].Version, files[2].Version, files[3].Version},
		"applied in numeric order, not directory order")
	assert.True(t, files[3].Destructive)
}

// The guard reads SQL with regular expressions, so a comment explaining that
// nothing is dropped must not be read as a DROP.
func TestGuardIgnoresComments(t *testing.T) {
	_, err := loadMigrations(writeMigrations(t, map[string]string{
		"001_add.sql": "-- we never DROP TABLE here\n/* nor RENAME TO anything */\nALTER TABLE records ADD COLUMN note TEXT;",
	}))
	assert.NoError(t, err)
}

// Two files claiming one version would run in whatever order the directory
// happened to be listed in.
func TestLoadMigrationsRejectsDuplicateVersions(t *testing.T) {
	_, err := loadMigrations(writeMigrations(t, map[string]string{
		"001_a.sql": "CREATE TABLE a(x);",
		"001_b.sql": "CREATE TABLE b(x);",
	}))
	assert.ErrorContains(t, err, "001")
}

func TestLoadMigrationsNoDirectoryIsFine(t *testing.T) {
	files, err := loadMigrations(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, files, "an app with no schema is a normal app")
}

// runScript drives the real runner against a real SQLite, which is the only
// way to know the generated SQL, the transactions and the snapshot actually
// work rather than merely compile.
func runScript(t *testing.T, dir string, files []migrationFile, sha string) migrateResult {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to exercise the migration runner")
	}
	payload, err := json.Marshal(map[string]any{
		"db":          filepath.Join(dir, appDataDBName),
		"snapshotDir": filepath.Join(dir, appDataSnapshotDir),
		"journalMode": "wal",
		"migrations":  files,
		"sha":         sha,
	})
	require.NoError(t, err)
	payloadFile := filepath.Join(dir, "payload.json")
	require.NoError(t, os.WriteFile(payloadFile, payload, 0o600))

	cmd := exec.Command(python, "-", payloadFile)
	cmd.Stdin = strings.NewReader(migrateScript)
	out, err := cmd.Output()
	require.NoError(t, err, "runner crashed")

	var result migrateResult
	require.NoError(t, json.Unmarshal(out, &result), "runner output: %s", out)
	return result
}

func TestMigrationRunner(t *testing.T) {
	create := migrationFile{Version: 1, Name: "create", SQL: "CREATE TABLE records (id INTEGER PRIMARY KEY, body TEXT);"}
	create.Checksum = migrationChecksum([]byte(create.SQL))
	add := migrationFile{Version: 2, Name: "add", SQL: "ALTER TABLE records ADD COLUMN note TEXT;"}
	add.Checksum = migrationChecksum([]byte(add.SQL))

	t.Run("applies in order and records what it did", func(t *testing.T) {
		dir := t.TempDir()
		got := runScript(t, dir, []migrationFile{create, add}, "abc123def456")
		require.True(t, got.OK, got.Error)
		assert.Equal(t, []int{1, 2}, got.Applied)
		assert.NotEmpty(t, got.Snapshot, "a snapshot is taken before anything is applied")

		// Re-running is a no-op: the record, not the file list, decides.
		again := runScript(t, dir, []migrationFile{create, add}, "abc123def456")
		require.True(t, again.OK, again.Error)
		assert.Empty(t, again.Applied)
		assert.Empty(t, again.Snapshot, "nothing pending means nothing to snapshot")
	})

	// "Even when versioning is updated, it must not be shaky": editing an
	// applied migration means the chain no longer describes this database.
	t.Run("refuses an applied migration that was edited", func(t *testing.T) {
		dir := t.TempDir()
		require.True(t, runScript(t, dir, []migrationFile{create}, "sha1").OK)

		edited := create
		edited.SQL = "CREATE TABLE records (id INTEGER PRIMARY KEY, body TEXT, extra TEXT);"
		edited.Checksum = migrationChecksum([]byte(edited.SQL))
		got := runScript(t, dir, []migrationFile{edited}, "sha2")

		assert.False(t, got.OK)
		assert.Equal(t, "checksum", got.ErrorKind)
	})

	// Deploying an older commit is allowed while everything it has not heard
	// of is additive — that is what makes rollback a symlink swap.
	t.Run("an older release is allowed past additive changes", func(t *testing.T) {
		dir := t.TempDir()
		require.True(t, runScript(t, dir, []migrationFile{create, add}, "sha1").OK)

		got := runScript(t, dir, []migrationFile{create}, "sha-old")
		require.True(t, got.OK, got.Error)
		assert.Equal(t, []int{2}, got.Missing, "the database is ahead, and that is fine")
	})

	// ...but not past one that removed something it still expects.
	t.Run("an older release is refused past a destructive change", func(t *testing.T) {
		dir := t.TempDir()
		drop := migrationFile{
			Version: 2, Name: "drop", Destructive: true,
			SQL: destructiveMarker + "\nALTER TABLE records DROP COLUMN body;",
		}
		drop.Checksum = migrationChecksum([]byte(drop.SQL))
		require.True(t, runScript(t, dir, []migrationFile{create, drop}, "sha1").OK)

		got := runScript(t, dir, []migrationFile{create}, "sha-old")
		assert.False(t, got.OK)
		assert.Equal(t, "floor", got.ErrorKind)
	})

	// A failing migration must leave the database exactly as it was, or
	// "the previous release keeps serving" is not true.
	t.Run("a failure applies nothing", func(t *testing.T) {
		dir := t.TempDir()
		bad := migrationFile{
			Version: 2, Name: "bad",
			SQL: "CREATE TABLE ok_so_far(x);\nTHIS IS NOT SQL;",
		}
		bad.Checksum = migrationChecksum([]byte(bad.SQL))
		require.True(t, runScript(t, dir, []migrationFile{create}, "sha1").OK)

		got := runScript(t, dir, []migrationFile{create, bad}, "sha2")
		assert.False(t, got.OK)

		after := runScript(t, dir, []migrationFile{create}, "sha3")
		require.True(t, after.OK, after.Error)
		assert.Empty(t, after.Missing, "the half-run migration recorded nothing")
	})
}
