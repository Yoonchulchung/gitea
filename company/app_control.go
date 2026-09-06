// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
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
		return userErrorf("%s", why) // CanTransition's text is written for staff
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
		return userErrorf("되돌아갈 이전 버전이 없습니다")
	}
	if _, err := os.Stat(previous); err != nil {
		// A release directory that has been cleaned up would otherwise fail
		// after the app is already stopped, leaving it down.
		return audienceError(
			"이전 버전의 파일이 서버에 더 이상 없습니다",
			"이전 릴리스 디렉터리가 없습니다 — 릴리스 GC 로 정리되었을 수 있습니다. 재배포가 필요합니다")
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
		st.HasRelease = true
		st.Reason, st.Message, st.UserMessage = "", "", ""
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
		st.HasRelease = false // the files are gone; a redeploy is the only way back
		st.PID = 0
		st.Reason = "removed"
		st.Message = "removed from the platform by " + actor
		st.UserMessage = "관리자가 이 앱을 플랫폼에서 제거했습니다"
		st.Health = AppHealth{State: "unknown"}
		st.AppendHistory(AppHistoryEntry{Status: AppStateStopped, Actor: actor, Reason: "removed"})
		return true
	})
}

// RedeployApp builds and activates the recorded commit again.
//
// The gap this fills: an approved deploy that failed had no way back. A
// build breaks for reasons that have nothing to do with the code — the queue
// was full, PyPI was briefly unreachable, the host ran out of disk — and
// without this the only route was to ask the department to submit the whole
// request again, which re-runs an approval nobody's mind has changed about.
//
// Open to the department, not only to admins. It re-runs the same commit, so
// it fixes nothing a code change would fix — but the interesting cases are
// the ones where the commit was never the problem: an admin has just
// approved the package the build was refused for, or changed the base
// package list, and that same commit now builds. Making them submit an
// identical deploy request to pick that up would re-run an approval nobody's
// mind has changed about.
//
// Where the fix really is in their own files, the failure already says so,
// which is a better answer than withholding the button.
func RedeployApp(owner, repo, actor string, isAdmin bool) error {
	st := LoadAppState(owner, repo)
	if ok, why := st.CanTransition("redeploy", isAdmin); !ok {
		return userErrorf("%s", why)
	}
	if st.SHA == "" {
		return audienceError(
			"아직 배포된 적이 없어 다시 배포할 것이 없습니다",
			"기록된 커밋이 없습니다 — 이 앱은 배포 요청이 승인된 적이 없습니다")
	}
	if err := MutateAppState(owner, repo, func(s *AppState) bool {
		s.Desired = AppStateRunning
		s.Actual = AppStateQueued
		s.Reason, s.Message, s.UserMessage = "", "", ""
		s.AppendHistory(AppHistoryEntry{Status: AppStateQueued, SHA: st.SHA, Actor: actor, Reason: "redeploy"})
		return true
	}); err != nil {
		return err
	}
	enqueueDeploy(owner, repo, st.SHA, st.PRID)
	return nil
}
