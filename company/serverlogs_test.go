// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serverLogDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := setting.Log.RootPath
	setting.Log.RootPath = dir
	t.Cleanup(func() { setting.Log.RootPath = old })
	return dir
}

func writeGz(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	gz := gzip.NewWriter(f)
	_, err = gz.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	require.NoError(t, f.Close())
}

func TestServerLogFilesOrder(t *testing.T) {
	dir := serverLogDir(t)
	for _, name := range []string{
		"gitea.log", "gitea.log.2026-09-18.001.gz", "gitea.log.2026-09-17.001.gz",
		"access.log", "app.ini", "notalog.txt",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	// Rotated first and in date order, the live file last, loggers grouped.
	// Anything that is not plainly a log file is not listed at all.
	assert.Equal(t, []string{
		"access.log",
		"gitea.log.2026-09-17.001.gz", "gitea.log.2026-09-18.001.gz", "gitea.log",
	}, ServerLogFiles())
}

func TestReadServerLogs(t *testing.T) {
	dir := serverLogDir(t)
	writeGz(t, filepath.Join(dir, "gitea.log.2026-09-17.001.gz"),
		"2026/09/17 10:00:00 router [I] yesterday\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gitea.log"),
		[]byte("2026/09/18 10:00:00 router [I] hello\n"+
			"2026/09/18 10:00:01 mailer [E] send failed\n"+
			"\tat some/frame.go:12\n"+
			"unstamped line\n"), 0o600))

	lines, truncated, err := ReadServerLogs(ServerLogQuery{})
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Equal(t, []string{"router yesterday", "router hello", "mailer send failed", "\tat some/frame.go:12", "unstamped line"},
		serverTexts(lines))
	// Rotated files are read through gzip, and the caller field survives into
	// the text rather than being eaten with the prefix.
	assert.Equal(t, "gitea.log.2026-09-17.001.gz", lines[0].File)
	assert.Equal(t, "error", lines[2].Level)
	assert.NotZero(t, lines[1].At)

	// A stack frame under an error is kept with it: it carries no level of its
	// own, and dropping it would leave the error with no detail.
	lines, _, err = ReadServerLogs(ServerLogQuery{MinLevel: "error"})
	require.NoError(t, err)
	assert.Equal(t, []string{"mailer send failed", "\tat some/frame.go:12", "unstamped line"}, serverTexts(lines))

	lines, _, err = ReadServerLogs(ServerLogQuery{File: "gitea.log", Text: "HELLO"})
	require.NoError(t, err)
	assert.Equal(t, []string{"router hello"}, serverTexts(lines))

	_, _, err = ReadServerLogs(ServerLogQuery{Text: "(", Regexp: true})
	assert.Error(t, err, "a bad regexp is reported, not ignored")
}

// A file name arrives from a query string, so it must never become a path.
func TestReadServerLogsRejectsPathEscape(t *testing.T) {
	dir := serverLogDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gitea.log"), []byte("2026/09/18 10:00:00 x [I] in\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.log"), []byte("2026/09/18 10:00:00 x [I] out\n"), 0o600))

	for _, name := range []string{"../secret.log", "/etc/passwd", "gitea.log.", "secret.log"} {
		lines, _, err := ReadServerLogs(ServerLogQuery{File: name})
		require.NoError(t, err)
		assert.Empty(t, lines, name)
	}
}

func serverTexts(lines []ServerLogLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}
