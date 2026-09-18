// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readyWriter stands in for the app log and says when the script has got
// past its setup — a signal sent before a trap is in place just ends the shell.
type readyWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *readyWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("ready")) {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

// startTestApp runs script the way the platform runs an app, supervised by s.
func startTestApp(t *testing.T, s *appSupervisor, script string, watch bool) *exec.Cmd {
	t.Helper()
	w := &readyWriter{ready: make(chan struct{})}
	cmd := exec.Command("sh", "-c", script)
	setAppProcessAttrs(cmd, w)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	s.cmd, s.startedAt = cmd, time.Now()
	if watch {
		go s.watchExit(cmd, io.NopCloser(strings.NewReader("")))
	}
	select {
	case <-w.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("the test app never got going")
	}
	return cmd
}

func shortenStops(t *testing.T) {
	prevGrace, prevWait := stopGracePeriod, appWaitDelay
	stopGracePeriod, appWaitDelay = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { stopGracePeriod, appWaitDelay = prevGrace, prevWait })
}

func cmdOf(s *appSupervisor) *exec.Cmd {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd
}

// The kernel's OOM killer takes one process. When that is the app and
// something it started lives on holding its output, the exit used to go
// unnoticed — no restart, and a state saying running — and the leftover kept
// the app's socket, so every later start refused as "already running".
func TestAnAppThatDiesAloneIsNoticed(t *testing.T) {
	withTempAppData(t)
	shortenStops(t)
	s := &appSupervisor{owner: "PO", repo: "alone", paths: appPathsFor("PO", "alone")}
	cmd := startTestApp(t, s, "sleep 30 & echo ready; exec sleep 60", true)
	s.mu.Lock()
	s.crashes, s.startedAt = crashRestartLimit-1, time.Now().Add(-time.Hour)
	s.mu.Unlock()

	require.NoError(t, cmd.Process.Kill()) // the app alone
	assert.Eventually(t, func() bool { return cmdOf(s) == nil }, 3*time.Second, 50*time.Millisecond,
		"the exit went unnoticed while its child held the output")
	assert.Eventually(t, func() bool { return syscall.Kill(-cmd.Process.Pid, 0) != nil }, 3*time.Second, 50*time.Millisecond,
		"what the app started outlived it")

	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, 1, s.crashes, "an app that ran for an hour starts a new crash streak rather than ending an old one")
}

// A stuck app often ignores SIGTERM too, so it is killed — and a start must
// wait for that kill to land. Starting straight after it took the dying
// process for a live one, started nothing, and left the app down while its
// state said running.
func TestStopWaitsForAKilledProcess(t *testing.T) {
	shortenStops(t)
	s := &appSupervisor{owner: "PO", repo: "stuck"}
	startTestApp(t, s, "trap '' TERM; echo ready; while :; do sleep 1; done", true)

	s.mu.Lock()
	err := s.stopLocked()
	reaped := s.cmd == nil
	s.mu.Unlock()
	require.NoError(t, err)
	assert.True(t, reaped, "stop returned while the killed process still counted as running")
}

// The lock is released while a stop waits, and a start that got in then —
// the department pressing start, the crash watcher, a deploy — has to find
// the stop's decision rather than bring the app back.
func TestStartStandsDownForAStopInProgress(t *testing.T) {
	withTempAppData(t)
	shortenStops(t)
	s := &appSupervisor{owner: "PO", repo: "racing", paths: appPathsFor("PO", "racing")}
	startTestApp(t, s, "trap '' TERM; echo ready; while :; do sleep 1; done", true)

	stopped := make(chan error, 1)
	go func() { stopped <- s.Stop("dept", AppStateStopped, "") }()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopping
	}, 3*time.Second, 10*time.Millisecond)

	s.mu.Lock()
	err := s.startLocked(AppSettings{}, nil, 0, "")
	gone := s.cmd == nil
	s.mu.Unlock()
	require.NoError(t, <-stopped)
	assert.ErrorIs(t, err, errStartSuperseded, "the stop was the later decision")
	assert.True(t, gone, "the start waited for the stopping process instead of taking it for a live one")
}

// A process that outlives SIGKILL is still there, and a restart that carried
// on would start a second copy beside it and record the first one's pid.
func TestAStopThatFailsSaysSo(t *testing.T) {
	withTempAppData(t)
	shortenStops(t)
	s := &appSupervisor{owner: "PO", repo: "unkillable", paths: appPathsFor("PO", "unkillable")}
	// Not watched, so never reaped — which is how a process stuck in the
	// kernel looks from here.
	cmd := startTestApp(t, s, "echo ready; exec sleep 30", false)
	t.Cleanup(func() { _ = cmd.Wait() })

	s.mu.Lock()
	err := s.stopLocked()
	s.mu.Unlock()
	require.Error(t, err, "a stop that did not stop reported success")
	require.Error(t, s.Restart())
	assert.Same(t, cmd, cmdOf(s), "nothing was started beside it")
}

func TestInterruptedDeploy(t *testing.T) {
	st := &AppState{Actual: AppStateBuilding, SHA: "old", History: []AppHistoryEntry{
		{Status: AppStateQueued, SHA: "new"},
		{Status: AppStateRunning, SHA: "old"},
	}}
	assert.Equal(t, "new", interruptedDeploy(st), "the commit that was on its way, not the one serving")

	st.Actual = AppStateRunning
	assert.Empty(t, interruptedDeploy(st))
}

// A deploy that failed its health check and was rolled back left the app
// running, and its request read as deployed.
func TestLastOutcomeOf(t *testing.T) {
	st := &AppState{History: []AppHistoryEntry{
		{Status: AppStateRunning, SHA: "old", Reason: ReasonRolledBack},
		{Status: AppStateFailed, SHA: "new", Reason: ReasonHealthTimeout},
		{Status: AppStateQueued, SHA: "new"},
	}}
	assert.Equal(t, AppStateFailed, st.LastOutcomeOf("new"))
	assert.Equal(t, AppStateRunning, st.LastOutcomeOf("old"))
	assert.Empty(t, st.LastOutcomeOf("gone"))
}

func TestOrphanCommandLine(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")
	assert.True(t, orphanCommandLine(p, "python "+p.releases+"/abc/.venv/bin/uvicorn main:app --uds "+p.socket))
	assert.False(t, orphanCommandLine(p, "tail -f "+p.logs+"/app.log"), "an administrator reading the log is not the app")
}
