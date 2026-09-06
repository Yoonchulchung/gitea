// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"testing"

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
