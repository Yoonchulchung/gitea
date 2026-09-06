// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"fmt"
	"os"

	"gitea.dev/modules/log"
)

// Departments run their own apps; admins run the platform.
//
// "Please take our app down for an hour" should not be a request to an
// administrator — it is the department's app and their decision. What the
// admin keeps is the ability to protect the platform from an app, which is a
// different thing and needs a different state: an app the department stopped
// is `stopped` and they can start it again, while one an admin stopped is
// `suspended` and they cannot. Without that distinction an admin who stops a
// runaway app just gets it restarted by the department a minute later, and
// the protection means nothing. See docs/company/app-platform.md.

// startAppAs starts an app on behalf of someone, honouring the
// stopped/suspended distinction. isAdmin decides whether a suspension can be
// overridden.
func startAppAs(owner, repo, actor string, isAdmin bool) error {
	st := LoadAppState(owner, repo)
	if ok, why := st.CanTransition("start", isAdmin); !ok {
		return errors.New(why)
	}
	if err := supervisorFor(owner, repo).Start(); err != nil {
		return err
	}
	return MutateAppState(owner, repo, func(s *AppState) bool {
		s.AppendHistory(AppHistoryEntry{Status: AppStateRunning, Actor: actor})
		return true
	})
}

// RollbackApp switches back to the previously deployed release.
//
// Available to the department, not just admins: "the version we shipped this
// morning is broken, put back yesterday's" is exactly the decision the people
// who shipped it should be able to make immediately, without waiting for
// someone else to be available.
func RollbackApp(owner, repo, actor string) error {
	p := appPathsFor(owner, repo)
	previous, err := os.Readlink(p.previous)
	if err != nil {
		return errors.New("there is no previous version to go back to")
	}
	if _, err := os.Stat(previous); err != nil {
		// A release directory that has been cleaned up would otherwise fail
		// after the app is already stopped, leaving it down.
		return errors.New("the previous version's files are no longer on disk")
	}

	s := supervisorFor(owner, repo)
	// Held for the whole switch, so a deploy landing at the same moment
	// cannot interleave with it.
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	current, _ := os.Readlink(p.current)

	s.mu.Lock()
	s.stopLocked()
	s.mu.Unlock()

	if err := swapSymlink(p.current, previous); err != nil {
		return err
	}
	if current != "" {
		// The version being rolled back out becomes the rollback target, so
		// rolling back twice returns to where you started rather than
		// stranding the app.
		_ = swapSymlink(p.previous, current)
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("the previous version could not be started: %w", err)
	}
	RecordRestart(owner, repo)
	return MutateAppState(owner, repo, func(st *AppState) bool {
		st.Reason, st.Message = "", ""
		st.AppendHistory(AppHistoryEntry{Status: AppStateRunning, Actor: actor, Reason: ReasonRolledBack})
		return true
	})
}

// RestartAppAs restarts and records who did it.
func RestartAppAs(owner, repo, actor string, isAdmin bool) error {
	if err := RestartApp(owner, repo, isAdmin); err != nil {
		return err
	}
	RecordRestart(owner, repo)
	return MutateAppState(owner, repo, func(st *AppState) bool {
		st.AppendHistory(AppHistoryEntry{Status: AppStateRunning, Actor: actor, Reason: "restarted"})
		return true
	})
}

// RemoveApp takes an app off the platform entirely: process stopped, routes
// withdrawn, files and secrets deleted.
//
// Admin-only, and irreversible by design — this is the answer to a renamed
// department repo leaving an app running forever at a URL nobody maintains.
// The code itself is untouched: it still lives in the central deploy repo's
// git history, so "remove" costs the department nothing they cannot redeploy.
func RemoveApp(owner, repo, actor string) error {
	s := supervisorFor(owner, repo)
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	s.mu.Lock()
	s.stopLocked()
	s.mu.Unlock()

	// Unregister before deleting, so no request can arrive for an app whose
	// socket is being removed underneath it.
	UnregisterApp(owner, repo)
	proxyCache.Delete(appKey(owner, repo))

	if err := DeleteAppEnv(owner, repo); err != nil {
		log.Error("company: removing env for %s/%s: %v", owner, repo, err)
	}
	p := appPathsFor(owner, repo)
	if err := os.RemoveAll(p.home); err != nil {
		return fmt.Errorf("the app's files could not be removed: %w", err)
	}
	// The state file is kept, marked removed: it carries the history of what
	// happened, which is exactly what someone asking "where did that app go?"
	// needs.
	return MutateAppState(owner, repo, func(st *AppState) bool {
		st.Desired = AppStateStopped
		st.Actual = AppStateStopped
		st.PID = 0
		st.Reason = "removed"
		st.Message = "이 앱은 관리자가 플랫폼에서 제거했습니다"
		st.Health = AppHealth{State: "unknown"}
		st.AppendHistory(AppHistoryEntry{Status: AppStateStopped, Actor: actor, Reason: "removed"})
		return true
	})
}
