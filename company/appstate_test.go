// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withTempAppData points AppDataPath at a throwaway dir so state tests
// touch real files (the atomic-write and locking behaviour is the point)
// without depending on, or disturbing, the instance's own data.
func withTempAppData(t *testing.T) {
	t.Helper()
	prev := setting.AppDataPath
	setting.AppDataPath = t.TempDir()
	t.Cleanup(func() { setting.AppDataPath = prev })
}

func TestAppKeyAvoidsCollisions(t *testing.T) {
	// "-" and "." are legal in both owner and repo names, so any flat
	// "<owner>-<repo>" join genuinely collides. These two must not share a
	// file, or one department's state would silently overwrite another's.
	assert.NotEqual(t, appKey("a-b", "c"), appKey("a", "b-c"))
	assert.NotEqual(t, appKey("x", "y.z"), appKey("x.y", "z"))
	// Truncated to appKeyLength so the app's socket path fits in sun_path —
	// see TestSocketPathFitsInSunPath. 64 bits is still far more than the
	// number of department apps that will ever exist.
	assert.Len(t, appKey("PO", "app"), appKeyLength)
}

func TestAppStateRoundTrip(t *testing.T) {
	withTempAppData(t)

	// Never deployed: a default, not an error — "no state" is normal.
	st := LoadAppState("PO", "app")
	assert.Equal(t, AppStateStopped, st.Actual)
	assert.Equal(t, AppStateStopped, st.Desired)

	require.NoError(t, MutateAppState("PO", "app", func(s *AppState) bool {
		s.Desired = AppStateRunning
		s.Actual = AppStateRunning
		s.SHA = "abc123"
		s.AppendHistory(AppHistoryEntry{SHA: "abc123", Status: AppStateRunning, Actor: "admin"})
		return true
	}))

	got := LoadAppState("PO", "app")
	assert.Equal(t, AppStateRunning, got.Actual)
	assert.Equal(t, "abc123", got.SHA)
	assert.NotZero(t, got.UpdatedAt)
	require.Len(t, got.History, 1)
	assert.Equal(t, "admin", got.History[0].Actor)
}

func TestMutateAppStateAbortWritesNothing(t *testing.T) {
	withTempAppData(t)
	require.NoError(t, MutateAppState("PO", "app", func(s *AppState) bool {
		s.SHA = "should-not-persist"
		return false // reject
	}))
	assert.Empty(t, LoadAppState("PO", "app").SHA)
}

func TestCorruptStateFileFailsOpen(t *testing.T) {
	withTempAppData(t)
	file := appStateFile("PO", "app")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
	require.NoError(t, os.WriteFile(file, []byte("{not json"), 0o600))

	// A corrupt record must not take the app down or blow up the dashboard —
	// it reads as "no state yet" and gets rebuilt on the next write.
	st := LoadAppState("PO", "app")
	assert.Equal(t, "PO", st.Owner)
	assert.Equal(t, AppStateStopped, st.Actual)

	require.NoError(t, MutateAppState("PO", "app", func(s *AppState) bool {
		s.Actual = AppStateRunning
		return true
	}))
	assert.Equal(t, AppStateRunning, LoadAppState("PO", "app").Actual)
}

func TestHistoryIsCappedNewestFirst(t *testing.T) {
	withTempAppData(t)
	require.NoError(t, MutateAppState("PO", "app", func(s *AppState) bool {
		for i := range appHistoryLimit + 5 {
			s.AppendHistory(AppHistoryEntry{SHA: fmt.Sprintf("sha%d", i), Status: AppStateRunning})
		}
		return true
	}))
	h := LoadAppState("PO", "app").History
	require.Len(t, h, appHistoryLimit)
	assert.Equal(t, fmt.Sprintf("sha%d", appHistoryLimit+4), h[0].SHA, "newest first")
}

func TestRollbackTargetSkipsCurrent(t *testing.T) {
	st := &AppState{SHA: "new", History: []AppHistoryEntry{
		{SHA: "new", Status: AppStateRunning},
		{SHA: "bad", Status: AppStateFailed}, // never roll back to a failure
		{SHA: "old", Status: AppStateRunning},
	}}
	assert.Equal(t, "old", st.RollbackTarget())

	assert.Empty(t, (&AppState{SHA: "only"}).RollbackTarget())
}

// The suspended/stopped split is what makes an admin's protective stop
// mean anything: if a department could simply restart an app the admin had
// just stopped for eating the machine, the admin's only remaining lever
// would be taking their controls away entirely.
func TestCanTransition(t *testing.T) {
	cases := []struct {
		name    string
		actual  string
		action  string
		isAdmin bool
		allow   bool
	}{
		{"department starts a stopped app", AppStateStopped, "start", false, true},
		{"department stops a running app", AppStateRunning, "stop", false, true},
		{"department rolls back", AppStateRunning, "rollback", false, true},

		{"department CANNOT start a suspended app", AppStateSuspended, "start", false, false},
		{"department CANNOT restart a suspended app", AppStateSuspended, "restart", false, false},
		{"admin must resume before starting", AppStateSuspended, "start", true, false},

		{"department cannot suspend", AppStateRunning, "suspend", false, false},
		{"department cannot remove", AppStateStopped, "remove", false, false},
		{"admin can suspend", AppStateRunning, "suspend", true, true},
		{"admin can resume a suspended app", AppStateSuspended, "resume", true, true},
		{"resume is meaningless when not suspended", AppStateRunning, "resume", true, false},

		// A queued or building deploy touches nothing — the previously
		// deployed version is serving normally, and the department has not
		// given up control of it by asking for a new one.
		{"a building deploy does not block restart", AppStateBuilding, "restart", true, true},
		{"nor stop", AppStateBuilding, "stop", false, true},
		{"nor an admin suspend", AppStateBuilding, "suspend", true, true},
		// The swap itself is the few seconds where a concurrent start would
		// race the deploy's own.
		{"the swap blocks start", AppStateActivating, "start", true, false},
		{"but never blocks stop — it is the safety valve", AppStateActivating, "stop", false, true},

		{"unknown action is refused", AppStateRunning, "explode", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, msg := (&AppState{Actual: c.actual}).CanTransition(c.action, c.isAdmin)
			assert.Equal(t, c.allow, ok)
			if !ok {
				assert.NotEmpty(t, msg, "a refusal must say why — it is shown to whoever clicked")
			}
		})
	}
}

func TestListAppStatesSortedAndSkipsCorrupt(t *testing.T) {
	withTempAppData(t)
	for _, a := range [][2]string{{"PO", "b"}, {"HR", "a"}, {"PO", "a"}} {
		require.NoError(t, MutateAppState(a[0], a[1], func(s *AppState) bool { return true }))
	}
	require.NoError(t, os.WriteFile(filepath.Join(appStateDir(), "garbage.json"), []byte("nope"), 0o600))

	got := ListAppStates()
	require.Len(t, got, 3, "one corrupt file must not blank the dashboard")
	assert.Equal(t, [][2]string{{"HR", "a"}, {"PO", "a"}, {"PO", "b"}},
		[][2]string{{got[0].Owner, got[0].Repo}, {got[1].Owner, got[1].Repo}, {got[2].Owner, got[2].Repo}})
}

// Concurrent updates must not lose one another: without the per-file lock
// held across read-modify-write, two goroutines both read the old history
// and one overwrites the other's append. Run with -race.
func TestMutateAppStateIsSerialized(t *testing.T) {
	withTempAppData(t)
	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_ = MutateAppState("PO", "app", func(s *AppState) bool {
				s.AppendHistory(AppHistoryEntry{SHA: fmt.Sprintf("s%d", i), Status: AppStateRunning})
				return true
			})
		})
	}
	wg.Wait()
	// History is capped, so the observable invariant is "the cap is full" —
	// if writes were lost we would see fewer.
	assert.Len(t, LoadAppState("PO", "app").History, appHistoryLimit)
}

// The socket lives under a directory named by this key, and sun_path is
// limited to 104 bytes on macOS and 108 on Linux. A full 64-character digest
// pushed it past that and every app failed to bind with "AF_UNIX path too
// long" — from inside asyncio, where it named neither the path nor the limit.
func TestSocketPathFitsInSunPath(t *testing.T) {
	prev := setting.AppDataPath
	// A deep but entirely ordinary deployment location.
	setting.AppDataPath = "/home/gitea-service-account/production/gitea/data"
	t.Cleanup(func() { setting.AppDataPath = prev })

	socket := appPathsFor("PROCUREMENT", "quarterly-report-generator").socket
	assert.Less(t, len(socket), maxUnixSocketPath, "socket path: %s", socket)
}

// A rollback moves the `current` symlink but nothing recovers the commit id
// from it: releases are named sha256(sha)[:16]. Without a record inside the
// release, the state file keeps naming the version that just failed, so the
// screen lies about what is serving and "다시 배포" rebuilds the broken commit.
func TestReleaseRecordsItsOwnSHA(t *testing.T) {
	release := t.TempDir()
	assert.Empty(t, readReleaseSHA(release), "a release with no record must not invent one")

	require.NoError(t, writeReleaseSHA(release, "abc123"))
	assert.Equal(t, "abc123", readReleaseSHA(release))

	// Adoption is what repairs a state left over from before a rollback, and
	// must not blank a SHA when the release predates the record.
	st := &AppState{SHA: "stale"}
	adoptCurrentReleaseSHA(st, "PO", "never-deployed")
	assert.Equal(t, "stale", st.SHA)
}

// Narrowing is the department's to make and must be reversible by them up to
// the ceiling policy sets — measuring against their own current choice instead
// would make the first narrowing permanent.
func TestAccessOptionsMeasureAgainstPolicyNotChoice(t *testing.T) {
	got := accessOptionsFor(AccessOrg, AccessPublic)
	for _, o := range got {
		assert.False(t, o.NeedsRequest, "%s is within policy, so it is a switch not a request", o.Value)
		assert.Equal(t, o.Value == AccessOrg, o.Selected)
	}
	// When policy itself is org-only, nothing wider may be offered as a switch.
	for _, o := range accessOptionsFor(AccessOrg, AccessOrg) {
		assert.Equal(t, o.Value != AccessOrg, o.NeedsRequest, o.Value)
	}
}

// Which release is live is not "the newest attempt": a failed deploy leaves
// the previous version running, and a rollback puts an older one back. The
// build log records a shortened SHA, so the match is by prefix.
func TestMarkCurrentFlagsTheLiveRelease(t *testing.T) {
	attempts := []deployAttempt{
		{SHA: "aaaaaaaaaaaa", Failed: true}, // newest, and it failed
		{SHA: "bbbbbbbbbbbb"},               // what is actually serving
		{SHA: "bbbbbbbbbbbb"},               // an earlier deploy of the same commit
	}
	markCurrent(attempts, "bbbbbbbbbbbbccccccccccccdddddddddddd")

	assert.False(t, attempts[0].Current)
	assert.True(t, attempts[1].Current)
	assert.False(t, attempts[2].Current, "only the newest attempt for that commit is marked")

	// Nothing deployed yet: no row claims to be running.
	none := []deployAttempt{{SHA: "aaaaaaaaaaaa"}}
	markCurrent(none, "")
	assert.False(t, none[0].Current)
}
