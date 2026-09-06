// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The real output that broke the sidebar: pip prints a Collecting/Downloading
// pair per package before it reaches the problem, and appends every available
// version to the error. None of that helps a department decide what to change.
const pipOutput = `Collecting fastapi==0.104.1
  Downloading fastapi-0.104.1-py3-none-any.whl.metadata (24 kB)
Collecting uvicorn==0.24.0
  Downloading uvicorn-0.24.0-py3-none-any.whl.metadata (6.4 kB)
Collecting pydantic==2.5.0
  Downloading pydantic-2.5.0-py3-none-any.whl.metadata (174 kB)
INFO: pip is looking at multiple versions of pydantic to determine which version is compatible with other requirements. This could take a while.
ERROR: Ignored the following yanked versions: 2.41.3, 2.43.0
ERROR: Could not find a version that satisfies the requirement pydantic-core==2.14.1 (from pydantic) (from versions: 0.0.1, 2.35.0, 2.35.1, 2.35.2, 2.36.0, 2.37.0, 2.46.5, 2.47.0, 2.48.0)
ERROR: No matching distribution found for pydantic-core==2.14.1`

func TestSummarizeInstallFailure(t *testing.T) {
	got := summarizeInstallFailure(pipOutput)

	assert.NotContains(t, got, "Downloading", "progress lines are noise")
	assert.NotContains(t, got, "Collecting")
	assert.Contains(t, got, "No matching distribution found for pydantic-core==2.14.1")
	// The version list is hundreds of characters and helps nobody here.
	assert.NotContains(t, got, "(from versions:")
	assert.NotContains(t, got, "2.48.0")
	// Two error lines plus the one-line instruction — a sidebar panel, not a
	// log viewer.
	assert.LessOrEqual(t, len(strings.Split(got, "\n")), installErrorLines+1)
	assert.NotContains(t, got, "Ignored the following", "pip labels this ERROR but it is not the failure")
	assert.NotContains(t, got, "\n\n", "a blank line inside a sidebar panel is wasted space")
	// The next step is appended as a key: this runs in the build worker, which
	// has no reader, so it renders in the instance's own language rather than
	// the reader's (company/usererror.go on platformLocale).
	assert.Contains(t, got, "company.cause.fix_and_redeploy")
}

func TestSummarizeInstallFailureWithoutErrorLines(t *testing.T) {
	// A build can fail without pip printing an ERROR: line at all — a killed
	// process, a network drop. The department still needs a next step rather
	// than an empty box.
	got := summarizeInstallFailure("Killed\n")
	assert.Equal(t, "company.cause.check_requirements", got)
	assert.NotContains(t, got, "Killed", "raw build output is admin-only")
}

// The department must never be handed the raw build log, whatever it says.
func TestDepartmentCauseKeepsBuildOutputAdminOnly(t *testing.T) {
	cause := DepartmentCause(&AppState{
		Actual:  AppStateFailed,
		Reason:  ReasonInstallFailed,
		Message: pipOutput,
	})
	assert.NotNil(t, cause)
	assert.NotContains(t, cause.DetailText, "Downloading")
	assert.Less(t, len(cause.DetailText), len(pipOutput)/2)
}

// Nothing a department sees may carry an absolute path from inside Gitea's
// data directory, a build log, or any other admin-only detail. Every reason
// code needs a sentence written for a non-developer; the default branch is a
// backstop, not a place to fall through to.
func TestDepartmentCauseNeverLeaksAdminDetail(t *testing.T) {
	secret := "/Users/someone/gitea/data/company-apps/9f2c/current"
	for _, reason := range []string{
		ReasonInstallFailed, ReasonNoRelease, ReasonHealthTimeout,
		ReasonCrashLoop, ReasonSandboxUnavailable, ReasonSecretError,
		ReasonDeployQueueFull, ReasonRolledBack, "some_future_code",
	} {
		cause := DepartmentCause(&AppState{Actual: AppStateFailed, Reason: reason, Message: secret})
		if assert.NotNil(t, cause, reason) {
			assert.NotContains(t, cause.Detail+cause.DetailText, secret, reason)
			assert.NotEmpty(t, cause.Summary, reason)
		}
	}
}

// "Start pressed before anything was ever deployed" is not a broken app, and
// the fix is a deploy rather than a code change — so it gets its own message
// and its own button.
func TestNoReleaseCausePointsAtDeploying(t *testing.T) {
	cause := DepartmentCause(&AppState{Actual: AppStateFailed, Reason: ReasonNoRelease})
	assert.Equal(t, "deploy", cause.Action)
	assert.Equal(t, "company.app.cause.no_release", cause.Summary)
}

// Pressing start on an app whose build failed must not replace the reason it
// failed. "There is nothing to run" is a consequence of that failure, and it
// is the one message that tells nobody what to fix.
func TestFailedStartKeepsTheDeployFailure(t *testing.T) {
	withTempAppData(t)
	require.NoError(t, MutateAppState("PO", "app", func(st *AppState) bool {
		st.Actual = AppStateFailed
		st.Reason = ReasonInstallFailed
		st.Message = "ERROR: No matching distribution found for pydantic-core==2.14.1"
		return true
	}))

	// No release exists, so this fails with errNoRelease.
	require.Error(t, supervisorFor("PO", "app").Start())

	st := LoadAppState("PO", "app")
	assert.Equal(t, ReasonInstallFailed, st.Reason, "the actionable cause survives")
	assert.Contains(t, DepartmentCause(st).DetailText, "pydantic-core")
}

// An app nobody has ever deployed is a different situation, and gets a
// message that says what to do rather than stating a fact.
func TestNeverDeployedSaysWhatToDo(t *testing.T) {
	cause := DepartmentCause(&AppState{Actual: AppStateFailed, Reason: ReasonNoRelease})
	assert.Equal(t, "deploy", cause.Action)
	assert.Equal(t, "company.app.cause.no_release.detail", cause.Detail)
	// The key is asserted above; the sentence itself lives in the locale files now.
}

// A build breaks for reasons unrelated to the code, and before this there
// was no way back except asking the department to submit the whole request
// again — re-running an approval nobody's mind had changed about.
func TestRedeployRequeuesTheRecordedCommit(t *testing.T) {
	withTempAppData(t)
	require.NoError(t, MutateAppState("PO", "app", func(st *AppState) bool {
		st.Actual = AppStateFailed
		st.Reason = ReasonInstallFailed
		st.SHA = "a92027a4479650425a3efa41d853c1cbed0fedb8"
		return true
	}))

	saved := deployQueue
	deployQueue = make(chan deployJob, 1)
	t.Cleanup(func() { deployQueue = saved })

	require.NoError(t, RedeployApp("PO", "app", "admin", true))

	job := <-deployQueue
	assert.Equal(t, "a92027a4479650425a3efa41d853c1cbed0fedb8", job.SHA, "the same commit, not a new one")

	st := LoadAppState("PO", "app")
	assert.Equal(t, AppStateQueued, st.Actual)
	assert.Empty(t, st.Reason, "the previous failure is cleared — this is a fresh attempt")
}

// Nothing to retry is not an error worth a stack trace, and the two
// audiences need different words for it.
func TestRedeployWithoutAnyDeploy(t *testing.T) {
	withTempAppData(t)
	err := RedeployApp("PO", "never", "admin", true)
	require.Error(t, err)
	assert.Equal(t, "company.err.never_deployed.admin", AdminError(err), "keys now; the sentences live in the locale files")
	assert.Equal(t, "company.err.never_deployed", DepartmentSafeError("ctx", err))
}

// The version currently serving users belongs to the department whatever
// else is going on. A new deploy request — queued, building, or failed —
// must not take away control of what is already running.
func TestControlSurvivesAFailedRedeploy(t *testing.T) {
	withTempAppData(t)

	// A previous deploy succeeded and the app is running.
	require.NoError(t, MutateAppState("PO", "app", func(st *AppState) bool {
		st.Desired, st.Actual = AppStateRunning, AppStateRunning
		st.HasRelease = true
		return true
	}))

	// A new deploy is submitted and its build fails. Nothing was swapped, so
	// the old version is still serving.
	failBuild("PO", "app", ReasonInstallFailed, "pip said no", AppStateRunning, "설치 실패")

	st := LoadAppState("PO", "app")
	assert.Equal(t, ReasonInstallFailed, st.Reason, "the failure is recorded")
	assert.Equal(t, AppStateRunning, st.Actual,
		"...but the running version is not reported as dead")

	ok, why := st.CanTransition("stop", false)
	assert.True(t, ok, "the department can still stop what is running: %s", why)

	// And they can still see why the deploy failed, even though the app runs.
	require.NotNil(t, DepartmentCause(st))
	assert.Equal(t, "company.app.cause.install_failed", DepartmentCause(st).Summary)
}

// Queued and building touch nothing — the previous version is serving
// normally. Only the swap itself is delicate enough to refuse a start.
func TestOnlyTheSwapBlocksControl(t *testing.T) {
	for _, state := range []string{AppStateQueued, AppStateBuilding} {
		st := &AppState{Actual: state, HasRelease: true}
		for _, action := range []string{"start", "stop", "restart"} {
			ok, why := st.CanTransition(action, false)
			assert.True(t, ok, "%s during %s: %s", action, state, why)
		}
	}

	swapping := &AppState{Actual: AppStateActivating, HasRelease: true}
	ok, _ := swapping.CanTransition("start", false)
	assert.False(t, ok, "starting mid-swap would race the deploy's own start")
	ok, _ = swapping.CanTransition("stop", false)
	assert.True(t, ok, "but stopping is always allowed — it is the safety valve")
}

// A rollback leaves the app running, and the reason it happened is worth
// reading. Keying the cause on Actual hid it.
func TestRollbackIsVisibleWhileRunning(t *testing.T) {
	cause := DepartmentCause(&AppState{Actual: AppStateRunning, Reason: ReasonRolledBack})
	require.NotNil(t, cause)
	assert.Equal(t, "company.app.cause.rolled_back", cause.Summary)
}

// Only two states survive a build that never reached the swap: Running,
// because those users are still being served, and Suspended, because an
// admin stopped the app on purpose. Everything else means nothing is
// serving, which is a failure.
func TestOnlyServingStatesSurviveABuildFailure(t *testing.T) {
	withTempAppData(t)
	for prior, want := range map[string]string{
		AppStateRunning:   AppStateRunning,
		AppStateSuspended: AppStateSuspended,
		AppStateStopped:   AppStateFailed,
		AppStateBuilding:  AppStateFailed,
		"":                AppStateFailed,
	} {
		require.NoError(t, MutateAppState("PO", "app", func(st *AppState) bool {
			st.Actual = prior
			return true
		}))
		failBuild("PO", "app", ReasonInstallFailed, "pip said no", prior)
		assert.Equal(t, want, LoadAppState("PO", "app").Actual, "prior %q", prior)
	}
}

// A department may rebuild the commit already deployed. The interesting case
// is the one where the commit was never the problem: a package the build was
// refused for has since been approved, and the same commit now builds.
func TestDepartmentMayRedeploy(t *testing.T) {
	withTempAppData(t)
	require.NoError(t, MutateAppState("PO", "app", func(st *AppState) bool {
		st.Actual, st.Reason = AppStateFailed, ReasonPackageDenied
		st.SHA = "a42245e516e4"
		return true
	}))

	saved := deployQueue
	deployQueue = make(chan deployJob, 1)
	t.Cleanup(func() { deployQueue = saved })

	require.NoError(t, RedeployApp("PO", "app", "staff", false))
	assert.Equal(t, "a42245e516e4", (<-deployQueue).SHA)
}

// Pressing it twice is duplication, not urgency — the deploy in flight is
// already rebuilding this commit.
func TestRedeployRefusedWhileOneIsInFlight(t *testing.T) {
	for _, busy := range []string{AppStateQueued, AppStateBuilding, AppStateActivating} {
		ok, why := (&AppState{Actual: busy, SHA: "x"}).CanTransition("redeploy", false)
		assert.False(t, ok, busy)
		assert.NotEmpty(t, why)
	}
}
