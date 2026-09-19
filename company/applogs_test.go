// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A failed deploy's output used to live only in AppState.Message, which the
// next deploy overwrote — so the log of the failure someone was trying to
// understand was gone by the time they went looking.
func TestBuildLogSurvivesTheNextDeploy(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")

	appendBuildLog(p, "aaaaaaaaaaaa1111", "FAILED", "ERROR: No matching distribution found for pydantic-core==2.14.1")
	appendBuildLog(p, "bbbbbbbbbbbb2222", "FAILED", "ERROR: No matching distribution found for pydantic-core==2.10.1")
	appendBuildLog(p, "cccccccccccc3333", "OK", "build succeeded")

	lines, _, err := ReadAppLogs("PO", "app", LogQuery{})
	require.NoError(t, err)

	var text string
	for _, l := range lines {
		assert.True(t, l.Build, "everything here came from a deploy")
		text += l.Text + "\n"
	}
	assert.Contains(t, text, "2.14.1", "the first failure is still there")
	assert.Contains(t, text, "2.10.1")
	assert.Contains(t, text, "aaaaaaaaaaaa", "each attempt is labelled with its commit")
	assert.Contains(t, text, "OK")
}

// Build output and the running app's are separate streams with no shared
// clock; build comes first as a block so a reader looking for why a deploy
// failed finds it at the top.
func TestBuildLogsComeBeforeAppLogs(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")
	require.NoError(t, os.MkdirAll(p.logs, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(p.logs, appLogName), []byte("runtime line\n"), 0o600))
	appendBuildLog(p, "sha", "FAILED", "build line")

	lines, _, err := ReadAppLogs("PO", "app", LogQuery{})
	require.NoError(t, err)
	require.NotEmpty(t, lines)
	assert.True(t, lines[0].Build)
	assert.False(t, lines[len(lines)-1].Build)
	assert.Equal(t, "runtime line", lines[len(lines)-1].Text)
}

// Searching has to reach the build half too — "why did it fail" is usually a
// search for a package name.
func TestLogSearchFindsBuildOutput(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")
	appendBuildLog(p, "sha", "FAILED", "ERROR: No matching distribution found for openpyxl==9.9.9")

	lines, _, err := ReadAppLogs("PO", "app", LogQuery{Text: "openpyxl"})
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0].Text, "openpyxl==9.9.9")
}

// Without a time on each line the log answers what happened but never when,
// which is exactly what someone needs to connect a failure to the deploy that
// caused it.
func TestTimestampWriterStampsWholeLines(t *testing.T) {
	var sink closableBuffer
	w := newTimestampWriter(&sink)
	w.now = func() time.Time { return time.Unix(1700000000, 0).UTC() }

	// A partial write must not get its own timestamp mid-line: the child does
	// not write on line boundaries, and a stack trace would end up striped.
	n, err := w.Write([]byte("hello "))
	require.NoError(t, err)
	assert.Equal(t, 6, n, "a short write is an error to exec; report what we were given")
	assert.Empty(t, sink.String(), "nothing is emitted until the line ends")

	_, err = w.Write([]byte("world\nsecond\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	lines := strings.Split(strings.TrimRight(sink.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	for _, line := range lines {
		at, text := splitLogTime(line)
		assert.Equal(t, int64(1700000000), at)
		assert.NotContains(t, text, "1970", "the stamp must be split back off, not left in the text")
	}
	assert.Equal(t, "hello world", func() string { _, s := splitLogTime(lines[0]); return s }())
}

// A crash usually leaves an unterminated line, and it is often the one worth
// reading.
func TestTimestampWriterFlushesPartialLineOnClose(t *testing.T) {
	var sink closableBuffer
	w := newTimestampWriter(&sink)
	_, err := w.Write([]byte("Traceback (most recent call last):"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	_, text := splitLogTime(strings.TrimRight(sink.String(), "\n"))
	assert.Equal(t, "Traceback (most recent call last):", text)
}

// Rotated files written before stamping existed are still in the directory,
// and an app is free to print something that starts with a date of its own.
func TestSplitLogTimeLeavesUnstampedLinesWhole(t *testing.T) {
	for _, line := range []string{
		"INFO:     Uvicorn running",
		"2026-01-01 something that is not our layout",
		"",
	} {
		at, text := splitLogTime(line)
		assert.Zero(t, at, line)
		assert.Equal(t, line, text)
	}
}

type closableBuffer struct{ bytes.Buffer }

func (closableBuffer) Close() error { return nil }

// The files rotate by size, not by day, so a person looking for last week
// picks a period; and a page holds only so many lines, so they can turn back.
func TestLogFilesHaveAPeriodAndPagesTurnBack(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")
	require.NoError(t, os.MkdirAll(p.logs, 0o700))
	stamp := func(day int, line string) string {
		return time.Date(2026, 9, day, 10, 0, 0, 0, time.UTC).Format(logTimeLayout) + " " + line + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(p.logs, appLogName+".1"), []byte(stamp(10, "old one")+stamp(11, "old two")), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(p.logs, appLogName), []byte(stamp(18, "new one")+stamp(19, "new two")+stamp(19, "new three")), 0o600))

	files := AppLogFiles("PO", "app")
	require.Len(t, files, 2)
	assert.Equal(t, appLogName+".1", files[0].Name, "oldest first")
	assert.Equal(t, time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC).Unix(), files[0].From)
	assert.Equal(t, time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC).Unix(), files[0].To)
	assert.True(t, files[1].Live)

	lines, _, err := ReadAppLogs("PO", "app", LogQuery{File: appLogName + ".1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"old one", "old two"}, texts(lines), "one file, chosen by period")

	lines, truncated, err := ReadAppLogs("PO", "app", LogQuery{Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{"new two", "new three"}, texts(lines))
	assert.True(t, truncated, "there is more before this page")

	lines, truncated, err = ReadAppLogs("PO", "app", LogQuery{Limit: 2, Before: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{"old two", "new one"}, texts(lines), "turned back one page")
	assert.True(t, truncated)

	lines, truncated, err = ReadAppLogs("PO", "app", LogQuery{Limit: 2, Before: 4})
	require.NoError(t, err)
	assert.Equal(t, []string{"old one"}, texts(lines), "the last page is what is left")
	assert.False(t, truncated)

	_, _, err = ReadAppLogs("PO", "app", LogQuery{File: "../../etc/passwd"})
	require.NoError(t, err)
}
