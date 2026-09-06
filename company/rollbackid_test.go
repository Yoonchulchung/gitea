// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// After a rollback the version on screen has to be the version running, and
// the two rollback paths move the symlinks differently: an automatic one
// restores the release that was already there, a deliberate one swaps the two
// so pressing it twice returns to where you started. Both are exercised
// against the same reporting the screens use.
func TestRollbackReportsTheVersionActuallyServing(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")
	require.NoError(t, os.MkdirAll(p.releases, 0o700))

	const (
		shaOld = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaNew = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	mk := func(sha string) string {
		dir := releaseDir(p, sha)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, writeReleaseSHA(dir, sha))
		return dir
	}
	oldDir, newDir := mk(shaOld), mk(shaNew)

	// A deploy: the new release goes live, the old one becomes the target.
	require.NoError(t, swapSymlink(p.current, newDir))
	require.NoError(t, swapSymlink(p.previous, oldDir))
	assert.Equal(t, shaNew, CurrentRelease("PO", "app").SHA)
	assert.Equal(t, shaOld, PreviousRelease("PO", "app").SHA)

	// The deliberate rollback (RollbackApp): current and previous trade
	// places, so pressing it again comes back here rather than stranding the
	// app on a version with nowhere to go.
	require.NoError(t, swapSymlink(p.current, oldDir))
	require.NoError(t, swapSymlink(p.previous, newDir))
	assert.Equal(t, shaOld, CurrentRelease("PO", "app").SHA, "the screen must name what is serving")
	assert.Equal(t, shaNew, PreviousRelease("PO", "app").SHA)

	// The state follows the disk, not the deploy that was intended — this is
	// what stops "다시 배포" rebuilding the version that just failed.
	st := &AppState{SHA: shaNew}
	adoptCurrentReleaseSHA(st, "PO", "app")
	assert.Equal(t, shaOld, st.SHA)

	// The automatic rollback (activateRelease's failure path): the new release
	// never took, so the previous one is put back and the rollback target is
	// cleared — leaving it would point at the release now running and make the
	// next rollback restart the same version while appearing to do nothing.
	require.NoError(t, swapSymlink(p.current, newDir))
	require.NoError(t, swapSymlink(p.previous, oldDir))
	require.NoError(t, swapSymlink(p.current, oldDir))
	require.NoError(t, os.Remove(p.previous))
	assert.Equal(t, shaOld, CurrentRelease("PO", "app").SHA)
	assert.False(t, PreviousRelease("PO", "app").Exists, "no target, so no rollback button is offered")

	// The history page marks the row that built what is running, which after
	// a rollback is not the newest attempt.
	attempts := []deployAttempt{{SHA: shaNew[:12], Failed: true}, {SHA: shaOld[:12]}}
	markCurrent(attempts, CurrentRelease("PO", "app").SHA)
	assert.False(t, attempts[0].Current)
	assert.True(t, attempts[1].Current)
}

// A release that has been cleaned up must not be reported as serving: the
// screens use Exists to decide whether to offer the rollback at all.
func TestRollbackTargetGoneIsReportedAsMissing(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")
	require.NoError(t, os.MkdirAll(p.releases, 0o700))
	require.NoError(t, swapSymlink(p.previous, filepath.Join(p.releases, "deleted")))

	assert.False(t, PreviousRelease("PO", "app").Exists)
	assert.Empty(t, PreviousRelease("PO", "app").SHA)
}

// A `previous` that points at the release already running is nowhere to go.
// The state is reachable — a deploy sets previous to whatever was current a
// moment before — and offering it produces a button that stops the app,
// starts the same version, and reports success. Whoever pressed it is left
// believing they went back a version.
func TestPreviousPointingAtCurrentIsNotARollbackTarget(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")
	require.NoError(t, os.MkdirAll(p.releases, 0o700))

	const sha = "cccccccccccccccccccccccccccccccccccccccc"
	dir := releaseDir(p, sha)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, writeReleaseSHA(dir, sha))
	require.NoError(t, swapSymlink(p.current, dir))
	require.NoError(t, swapSymlink(p.previous, dir))

	assert.Equal(t, sha, CurrentRelease("PO", "app").SHA)
	assert.False(t, PreviousRelease("PO", "app").Exists, "no button is offered")

	// And the action itself refuses, because hiding a button does not stop a
	// form from being submitted.
	err := RollbackApp("PO", "app", "someone")
	require.Error(t, err)
	assert.Contains(t, DepartmentSafeError("rollback", err), "되돌아갈 이전 버전이 없습니다")
}

// Rolling back one step follows a symlink that only ever holds one version.
// Every version the app has run is still in the deploy repository, so being
// able to reach only the most recent one is a limit of the mechanism rather
// than of what is possible — and a version that failed on an unapproved
// package is exactly the one worth reaching once the package is approved.
func TestDeployVersionRefusesWhatItCannotDo(t *testing.T) {
	withTempAppData(t)
	require.NoError(t, MutateAppState("PO", "app", func(st *AppState) bool {
		st.Actual = AppStateActivating // mid-swap
		return true
	}))

	// The same gate as a redeploy: this replaces what is serving.
	err := DeployVersion(t.Context(), "PO", "app", "abc123", "admin", true)
	require.Error(t, err)
	assert.NotEmpty(t, DepartmentSafeError("deploy-version", err))

	// The "commit not in the repository" refusal is not exercised here: it
	// needs the central deploy repo configured, which is integration setup
	// rather than a unit test. What matters is that it is checked before a
	// build starts, so the failure is "that version is no longer there" rather
	// than a message about git from inside a build.
}

// A deploy that is queued or building has no build-log entry yet — the header
// is written when it finishes — so without a row of its own the history page
// implies nothing is happening while a deploy is underway.
func TestHistoryShowsADeployStillInFlight(t *testing.T) {
	finished := []deployAttempt{{At: 100, SHA: "bbbbbbbbbbbb"}, {At: 50, SHA: "aaaaaaaaaaaa"}}

	rows := withLiveState(slices.Clone(finished),
		&AppState{Actual: AppStateBuilding, SHA: "cccccccccccccccc", UpdatedAt: 200}, "aaaaaaaaaaaadddd")

	require.Len(t, rows, 3)
	assert.True(t, rows[0].InProgress)
	assert.Equal(t, "cccccccccccc", rows[0].SHA, "shortened to match how the log records it")
	assert.Contains(t, rows[0].Summary, "설치")

	// "Running" and "latest" are different rows whenever a deploy failed or an
	// older version was put back, and that difference is what someone came to
	// find.
	assert.True(t, rows[1].Latest, "the newest finished attempt")
	assert.False(t, rows[1].Current)
	assert.True(t, rows[2].Current, "an older release is what is actually serving")

	// Nothing in flight: no synthetic row, and the newest attempt is still
	// marked.
	rows = withLiveState(slices.Clone(finished), &AppState{Actual: AppStateRunning}, "bbbbbbbbbbbbdddd")
	require.Len(t, rows, 2)
	assert.True(t, rows[0].Latest)
	assert.True(t, rows[0].Current, "here they are the same row")
}
