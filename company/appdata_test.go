// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func repoGone(int64) (bool, error)  { return false, nil }
func repoLives(int64) (bool, error) { return true, nil }

// The whole point of keying by repository ID: the department renames its
// repo, every other directory the platform owns is re-keyed, and the data
// stays exactly where it was.
func TestAppDataDirSurvivesRename(t *testing.T) {
	withTempAppData(t)

	before, err := ensureAppDataDir(42, "PO", "old-name")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(before, "app.db"), []byte("rows"), 0o600))

	after, err := ensureAppDataDir(42, "PO", "new-name")
	require.NoError(t, err)

	assert.Equal(t, before, after)
	body, err := os.ReadFile(filepath.Join(after, "app.db"))
	require.NoError(t, err)
	assert.Equal(t, "rows", string(body), "the data must not be re-created empty")

	meta, err := loadAppDataMeta(after)
	require.NoError(t, err)
	assert.Equal(t, "new-name", meta.Repo, "the recorded name follows the repo, the location does not")
}

// Soft delete keeps the data; redeploying inside the window is the undelete,
// and it must not need a separate call to happen.
func TestAppDataSoftDeleteAndRevival(t *testing.T) {
	withTempAppData(t)
	dir, err := ensureAppDataDir(7, "PO", "app")
	require.NoError(t, err)

	meta, err := loadAppDataMeta(dir)
	require.NoError(t, err)
	meta.RemovedAt = time.Now().Add(-time.Hour).Unix()
	require.NoError(t, saveAppDataMeta(dir, meta))

	sweepAppData(time.Now(), 90*24*time.Hour, repoGone)
	assert.DirExists(t, dir, "still inside the retention window")

	_, err = ensureAppDataDir(7, "PO", "app")
	require.NoError(t, err)
	meta, err = loadAppDataMeta(dir)
	require.NoError(t, err)
	assert.Zero(t, meta.RemovedAt, "using the directory clears the deletion clock")
}

func TestSweepAppData(t *testing.T) {
	retention := 90 * 24 * time.Hour
	now := time.Now()

	seed := func(t *testing.T, id, removedAt int64) string {
		t.Helper()
		dir, err := ensureAppDataDir(id, "PO", "app")
		require.NoError(t, err)
		if removedAt != 0 {
			meta, err := loadAppDataMeta(dir)
			require.NoError(t, err)
			meta.RemovedAt = removedAt
			require.NoError(t, saveAppDataMeta(dir, meta))
		}
		return dir
	}

	t.Run("expired data is deleted", func(t *testing.T) {
		withTempAppData(t)
		dir := seed(t, 1, now.Add(-91*24*time.Hour).Unix())
		sweepAppData(now, retention, repoGone)
		assert.NoDirExists(t, dir)
	})

	t.Run("a live app is left alone", func(t *testing.T) {
		withTempAppData(t)
		dir := seed(t, 2, 0)
		sweepAppData(now, retention, repoLives)
		assert.DirExists(t, dir)
		meta, err := loadAppDataMeta(dir)
		require.NoError(t, err)
		assert.Zero(t, meta.RemovedAt)
	})

	// A deleted repository leaves data nothing will ever ask for again, so
	// the clock has to start without anyone pressing anything.
	t.Run("a vanished repo starts the clock", func(t *testing.T) {
		withTempAppData(t)
		dir := seed(t, 3, 0)
		sweepAppData(now, retention, repoGone)
		assert.DirExists(t, dir, "the clock starts; the data does not go yet")
		meta, err := loadAppDataMeta(dir)
		require.NoError(t, err)
		assert.NotZero(t, meta.RemovedAt)
	})

	// The failure that would be worst here: the database blinks, every
	// lookup fails, and the sweep reads that as "all of these are orphans".
	t.Run("a lookup failure changes nothing", func(t *testing.T) {
		withTempAppData(t)
		dir := seed(t, 4, 0)
		sweepAppData(now, retention, func(int64) (bool, error) { return false, errors.New("db down") })
		meta, err := loadAppDataMeta(dir)
		require.NoError(t, err)
		assert.Zero(t, meta.RemovedAt, "no clock may start because a lookup failed")
	})

	t.Run("a directory we did not write is not ours to delete", func(t *testing.T) {
		withTempAppData(t)
		stray := filepath.Join(appDataRoot(), "5")
		require.NoError(t, os.MkdirAll(stray, 0o700))
		sweepAppData(now, retention, repoGone)
		assert.DirExists(t, stray)
	})
}

// Read literally, zero means "delete everything now" — so it is refused
// rather than honoured.
func TestAppDataRetentionIgnoresNonPositive(t *testing.T) {
	for _, raw := range []string{"0", "-1", "ninety", ""} {
		cfg, err := setting.NewConfigProviderFromData("[company]\nAPP_DATA_RETENTION_DAYS = " + raw)
		require.NoError(t, err)
		prev := setting.CfgProvider
		setting.CfgProvider = cfg
		got := appDataRetention()
		setting.CfgProvider = prev
		assert.Equal(t, time.Duration(appDataRetentionDaysDefault)*24*time.Hour, got, "raw=%q", raw)
	}
}

// The app runs as this same user and can drop a symlink in its own data
// directory; charging the target's size to this app would be the platform
// following it.
func TestAppDataBytesIgnoresSymlinks(t *testing.T) {
	withTempAppData(t)
	dir, err := ensureAppDataDir(9, "PO", "app")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.db"), make([]byte, 100), 0o600))

	outside := filepath.Join(setting.AppDataPath, "elsewhere")
	require.NoError(t, os.WriteFile(outside, make([]byte, 5000), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "sneaky")))

	assert.Less(t, appDataBytes(dir), int64(1000), "the symlink target must not be counted")
}

// The app is handed a path, never asked to build one. Under bubblewrap the
// host path and the in-sandbox path are different names for one directory,
// and an app that hardcoded either would be wrong somewhere.
func TestAppDataEnvIsSelfConsistent(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")
	host := appDataDirFor(11)

	env := buildEnv(p, "/apps/PO/app", host, nil, false)
	inSandbox := appDataDirForProcess(host)

	assert.Contains(t, env, "DATA_DIR="+inSandbox)
	assert.Contains(t, env, "DB_PATH="+filepath.Join(inSandbox, appDataDBName))
	if mode, _ := sandboxMode(); mode == SandboxBubblewrap {
		assert.Equal(t, sandboxDataPath, inSandbox, "bubblewrap mounts it at a fixed point")
	} else {
		assert.Equal(t, host, inSandbox, "without a mount namespace the app sees the real path")
	}
}

// Off by default means genuinely absent, not empty: an app that reads
// DB_PATH and finds "" would create a database in its own release tree.
func TestAppDataEnvAbsentWhenDisabled(t *testing.T) {
	withTempAppData(t)
	for _, entry := range buildEnv(appPathsFor("PO", "app"), "/apps/PO/app", "", nil, false) {
		assert.NotContains(t, entry, "DATA_DIR=")
		assert.NotContains(t, entry, "DB_PATH=")
	}
}

// Letting an app point DB_PATH somewhere else would put its data outside
// everything that backs it up, sizes it and deletes it on schedule.
func TestAppDataEnvNamesAreReserved(t *testing.T) {
	withTempAppData(t)
	host := appDataDirFor(12)
	env := buildEnv(appPathsFor("PO", "app"), "/apps/PO/app", host, map[string]string{
		"DB_PATH":  "/tmp/mine.db",
		"DATA_DIR": "/tmp",
	}, false)

	assert.NotContains(t, env, "DB_PATH=/tmp/mine.db")
	assert.NotContains(t, env, "DATA_DIR=/tmp")
	assert.Contains(t, env, "DB_PATH="+filepath.Join(appDataDirForProcess(host), appDataDBName))
	assert.True(t, isReservedEnvName("db_path"), "case must not be a way around it")
}

// The probe measures; the setting overrides, because a measurement can be
// wrong and an operator has to be able to say so.
func TestDataJournalModeOverride(t *testing.T) {
	for raw, want := range map[string]string{"wal": "wal", "truncate": "truncate", "WAL": "wal"} {
		cfg, err := setting.NewConfigProviderFromData("[company]\nAPP_DATA_JOURNAL_MODE = " + raw)
		require.NoError(t, err)
		prev := setting.CfgProvider
		setting.CfgProvider = cfg
		got := DataJournalMode()
		setting.CfgProvider = prev
		assert.Equal(t, want, got, "raw=%q", raw)
	}
}

// A department that needed more room should not pin every other app to
// whatever the instance-wide default was that day.
func TestAppDataQuotaPerApp(t *testing.T) {
	cfg, err := setting.NewConfigProviderFromData("[company]\nAPP_DATA_QUOTA_MB = 256")
	require.NoError(t, err)
	prev := setting.CfgProvider
	setting.CfgProvider = cfg
	defer func() { setting.CfgProvider = prev }()

	assert.Equal(t, int64(256)<<20, appDataQuotaBytes(AppSettings{}))
	assert.Equal(t, int64(2048)<<20, appDataQuotaBytes(AppSettings{Limits: AppLimits{DataMB: 2048}}))
}

// Full stops deploys but not the app: a database at its limit still answers
// every read, and stopping it would turn a full disk into an outage.
func TestAppDataUsageThresholds(t *testing.T) {
	cfg, err := setting.NewConfigProviderFromData("[company]\nAPP_DATA_WARN_PCT = 80")
	require.NoError(t, err)
	prev := setting.CfgProvider
	setting.CfgProvider = cfg
	defer func() { setting.CfgProvider = prev }()

	quota := int64(100)
	for _, tc := range []struct {
		bytes      int64
		warn, full bool
	}{
		{bytes: 10},
		{bytes: 79},
		{bytes: 80, warn: true},
		{bytes: 99, warn: true},
		{bytes: 100, warn: true, full: true},
		{bytes: 900, warn: true, full: true},
	} {
		u := AppDataUsage{Bytes: tc.bytes, QuotaBytes: quota, Percent: int(tc.bytes * 100 / quota)}
		assert.Equal(t, tc.warn, u.Warning(), "bytes=%d", tc.bytes)
		assert.Equal(t, tc.full, u.Full(), "bytes=%d", tc.bytes)
	}
}

// Every one of these is a limit, so a non-positive value read literally means
// "no space" or "delete now" — always a typo, never an intent.
func TestCompanySettingPositiveIntRejectsNonPositive(t *testing.T) {
	for _, raw := range []string{"0", "-5", "lots", ""} {
		cfg, err := setting.NewConfigProviderFromData("[company]\nAPP_DATA_QUOTA_MB = " + raw)
		require.NoError(t, err)
		prev := setting.CfgProvider
		setting.CfgProvider = cfg
		got := companySettingPositiveInt("APP_DATA_QUOTA_MB", 512)
		setting.CfgProvider = prev
		assert.Equal(t, 512, got, "raw=%q", raw)
	}
}
