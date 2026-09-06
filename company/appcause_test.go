// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
	assert.LessOrEqual(t, len(strings.Split(got, "\n")), installErrorLines+2)
	assert.Contains(t, got, "requirements.txt")
}

func TestSummarizeInstallFailureWithoutErrorLines(t *testing.T) {
	// A build can fail without pip printing an ERROR: line at all — a killed
	// process, a network drop. The department still needs a next step rather
	// than an empty box.
	got := summarizeInstallFailure("Killed\n")
	assert.Contains(t, got, "requirements.txt")
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
	assert.NotContains(t, cause.Detail, "Downloading")
	assert.Less(t, len(cause.Detail), len(pipOutput)/2)
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
			assert.NotContains(t, cause.Detail, secret, reason)
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
	assert.Contains(t, cause.Summary, "배포")
}
