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
	assert.Len(t, appKey("PO", "app"), 64) // full sha256 hex — no truncation to collide on
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

		{"mid-deploy blocks restart", AppStateBuilding, "restart", true, false},
		{"mid-deploy still allows an admin suspend", AppStateBuilding, "suspend", true, true},

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
