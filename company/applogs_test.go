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
