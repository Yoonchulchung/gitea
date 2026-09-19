// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
		return userKeyError(why) // CanTransition's text is written for staff
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
		return userKeyError("company.err.no_rollback_target")
	}
	// The button is hidden in this case, but a form can still be submitted:
	// restarting the running version and calling it a rollback would tell
	// someone they went back when they did not.
	if current, err := os.Readlink(p.current); err == nil && current == previous {
		return userKeyError("company.err.no_rollback_target")
	}
	if _, err := os.Stat(previous); err != nil {
		// A release directory that has been cleaned up would otherwise fail
		// after the app is already stopped, leaving it down.
		return audienceKeyError("company.err.rollback_gone", "company.err.rollback_gone.admin")
	}

	s := supervisorFor(owner, repo)
	// Held for the whole switch, so a deploy landing at the same moment
	// cannot interleave with it.
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	current, _ := os.Readlink(p.current)

	s.mu.Lock()
	err = s.stopLocked()
	s.mu.Unlock()
	if err != nil {
		return err // still running from the release it would be switched away from
	}

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
	// The code went back; the database did not. Migrations the newer version
	// applied are still in it, and an older version that does not expect
	// them can fail in ways that look like anything but this.
	ahead := migrationsAhead(current, previous)
	return MutateAppState(owner, repo, func(st *AppState) bool {
		st.HasRelease = true
		st.Reason, st.Message, st.UserMessage = "", "", ""
		if ahead > 0 {
			st.Reason = ReasonSchemaAhead
			st.Message = fmt.Sprintf("the database carries %d migration(s) from the version rolled back from; restore the snapshot taken before them if the app misbehaves", ahead)
			st.UserMessageKey, st.UserMessageArg = "company.app.schema_ahead_detail", ahead
		}
		adoptCurrentReleaseSHA(st, owner, repo)
		st.AppendHistory(AppHistoryEntry{Status: AppStateRunning, SHA: st.SHA, Actor: actor, Reason: ReasonRolledBack})
		return true
	})
}

// migrationsAhead counts the migrations a release carries that an older one
// does not: what a rollback leaves applied in the database.
func migrationsAhead(newer, older string) int {
	if newer == "" || older == "" {
		return 0
	}
	newerFiles, err := loadMigrations(filepath.Join(newer, "app"))
	if err != nil {
		return 0
	}
	olderFiles, _ := loadMigrations(filepath.Join(older, "app"))
	highest := 0
	for _, m := range olderFiles {
		highest = max(highest, m.Version)
	}
	ahead := 0
	for _, m := range newerFiles {
		if m.Version > highest {
			ahead++
		}
	}
	return ahead
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

// RemoveApp takes an app off the platform: process stopped, routes
// withdrawn, releases and secrets deleted.
//
// This is a soft delete. Everything it removes is rebuildable from git, so
// "remove" costs the department nothing they cannot redeploy — but that
// argument has never applied to their data, which is kept and put on a
// retention clock instead. Redeploying inside that window brings it back
// untouched (company/appdata.go, docs/company/app-data.md).
func RemoveApp(ctx context.Context, owner, repo, actor string) error {
	s := supervisorFor(owner, repo)
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	s.mu.Lock()
	err := s.stopLocked()
	s.mu.Unlock()
	if err != nil {
		return err // deleting the files of a process that is still running
	}

	// Unregister before deleting, so no request can arrive for an app whose
	// socket is being removed underneath it.
	UnregisterApp(owner, repo)
	proxyCache.Delete(appKey(owner, repo))

	if err := DeleteAppEnv(owner, repo); err != nil {
		log.Error("company: removing env for %s/%s: %v", owner, repo, err)
	}
	// Before the files: this needs the repository to still resolve by name,
	// and nothing here guarantees it will a moment later.
	MarkAppDataRemoved(ctx, owner, repo)

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
		st.Reason = ReasonRemoved
		st.Message = "removed from the platform by " + actor
		st.UserMessage = ""
		st.UserMessageKey = "company.app.removed"
		st.Health = AppHealth{State: "unknown"}
		st.AppendHistory(AppHistoryEntry{Status: AppStateStopped, Actor: actor, Reason: ReasonRemoved})
		return true
	})
}

// CancelStuckDeploy takes an app out of a busy state nothing will finish.
//
// Only a state that has stood for a while: a deploy still building is not
// stuck, and pulling its record out from under it would leave the worker
// writing over whatever comes next. The app is recorded as what it actually
// is — running if its process is, failed if not.
func CancelStuckDeploy(owner, repo, actor string) error {
	st := LoadAppState(owner, repo)
	if !st.IsBusy() {
		return userKeyError("company.err.not_deploying")
	}
	if time.Since(time.Unix(st.UpdatedAt, 0)) < staleDeployAfter {
		return userKeyError("company.err.deploy_not_stuck", int(staleDeployAfter.Minutes()))
	}
	serving := IsAppRunning(owner, repo)
	return MutateAppState(owner, repo, func(st *AppState) bool {
		st.Actual = AppStateFailed
		if serving {
			st.Actual = AppStateRunning
		}
		st.FailedAt = time.Now().Unix()
		st.Reason = ReasonDeployCancelled
		st.Message = "a deploy that had been " + st.Actual + " for over " + staleDeployAfter.String() + " was cancelled by " + actor
		st.UserMessage = ""
		st.AppendHistory(AppHistoryEntry{Status: AppStateFailed, Actor: actor, Reason: ReasonDeployCancelled})
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
		return userKeyError(why)
	}
	if st.SHA == "" {
		return audienceKeyError("company.err.never_deployed", "company.err.never_deployed.admin")
	}
	prior := markQueued(owner, repo, st.SHA, 0, true, AppHistoryEntry{Actor: actor, Reason: "redeploy"})
	enqueueDeploy(owner, repo, st.SHA, st.PRID, prior)
	return nil
}
