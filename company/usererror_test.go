// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"

	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The error that started this: os errors are *fs.PathError and every one of
// them names an absolute path inside Gitea's data directory.
func TestDepartmentSafeErrorHidesFilesystemErrors(t *testing.T) {
	_, err := os.Readlink("/Users/someone/gitea/data/company-apps/9646/current")
	require.Error(t, err)
	require.ErrorAs(t, err, new(*fs.PathError), "the premise of this test")

	got := DepartmentSafeError("starting PO/report", err)
	assert.NotContains(t, got, "/Users")
	assert.NotContains(t, got, "company-apps")
	assert.NotEmpty(t, got, "a department still needs to be told something happened")
}

// Opt-in disclosure: a message reaches a department only if it was written
// for one. Wrapping still works, so callers can add context.
func TestUserErrorPassesThrough(t *testing.T) {
	err := userErrorf("저장소 최상위에 %s 가 없습니다", "main.py")
	assert.Equal(t, "저장소 최상위에 main.py 가 없습니다", DepartmentSafeError("ctx", err))

	wrapped := errors.Join(errors.New("internal detail"), err)
	assert.Equal(t, "저장소 최상위에 main.py 가 없습니다", DepartmentSafeError("ctx", wrapped))

	assert.Empty(t, DepartmentSafeError("ctx", nil))
}

func TestRedactServerPaths(t *testing.T) {
	prev := setting.AppDataPath
	setting.AppDataPath = "/srv/gitea/data"
	t.Cleanup(func() { setting.AppDataPath = prev })

	assert.NotContains(t,
		RedactServerPaths("OSError: Permission denied: '/srv/gitea/data/company-apps/x'"),
		"/srv/gitea/data")
	assert.NotContains(t,
		RedactServerPaths("Permission denied: '/home/git/gitea/custom/conf/app.ini'"),
		"/home/git")

	// A package name or a system path tells nobody anything they could not
	// guess, and blanking them would mangle the one useful line.
	kept := RedactServerPaths("No matching distribution found for pydantic-core==2.14.1")
	assert.Contains(t, kept, "pydantic-core==2.14.1")
	assert.Contains(t, RedactServerPaths("using /usr/lib/python3.11"), "/usr/lib/python3.11")
}

// The invariant this whole file exists for: whatever a failure recorded in
// Message, a department must never be shown it.
func TestDepartmentCauseIgnoresAdminMessageEntirely(t *testing.T) {
	leak := "readlink /Users/someone/gitea/data/company-apps/9646/current: no such file"
	reasons := []string{
		ReasonInstallFailed, ReasonPackageDenied, ReasonOOM, ReasonHealthTimeout,
		ReasonCrashLoop, ReasonSuspended, ReasonSandboxUnavailable, ReasonSecretError,
		ReasonDeployQueueFull, ReasonContractViolation, ReasonNoRelease,
		ReasonRolledBack, "a_reason_nobody_has_written_a_sentence_for",
	}
	for _, reason := range reasons {
		st := &AppState{Actual: AppStateFailed, Reason: reason, Message: leak}
		cause := DepartmentCause(st)
		require.NotNil(t, cause, reason)
		assert.NotContains(t, cause.Detail, "/Users", reason)
		assert.NotContains(t, cause.Detail, "company-apps", reason)
		assert.NotEmpty(t, cause.Summary, reason)
	}
}

// UserMessage is the only channel to a department, and it reaches them.
func TestUserMessageIsWhatSurfaces(t *testing.T) {
	cause := DepartmentCause(&AppState{
		Actual:      AppStateSuspended,
		Reason:      ReasonSuspended,
		Message:     "internal: killed by watchdog at /srv/gitea/data",
		UserMessage: "메모리를 너무 많이 써서 정지시켰습니다",
	})
	assert.Equal(t, "메모리를 너무 많이 써서 정지시켰습니다", cause.Detail)
}

// pip's own errors can name a path, so even the filtered subset is scrubbed.
func TestInstallFailureSummaryRedactsPaths(t *testing.T) {
	prev := setting.AppDataPath
	setting.AppDataPath = "/srv/gitea/data"
	t.Cleanup(func() { setting.AppDataPath = prev })

	got := summarizeInstallFailure(strings.Join([]string{
		"Collecting fastapi==0.104.1",
		"ERROR: Could not install packages due to an OSError: [Errno 13] Permission denied: '/srv/gitea/data/company-apps/x/.venv'",
		"ERROR: No matching distribution found for pydantic-core==2.14.1",
	}, "\n"))

	assert.NotContains(t, got, "/srv/gitea/data")
	assert.Contains(t, got, "pydantic-core==2.14.1", "the actionable half survives")
}

// Safe to show is not the same as addressed to the reader. An administrator
// told to press "Deploy Request" has been handed the department's job.
func TestAudienceErrorSeparatesTheTwoVoices(t *testing.T) {
	err := audienceError("배포 요청을 해주세요", "부서의 배포 요청을 승인해 주세요")

	assert.Equal(t, "배포 요청을 해주세요", DepartmentSafeError("ctx", err))
	assert.Equal(t, "부서의 배포 요청을 승인해 주세요", AdminError(err))
}

func TestAdminErrorFallsBack(t *testing.T) {
	// Advice that suits both audiences is written once.
	shared := userErrorf("변수 이름이 비어 있습니다")
	assert.Equal(t, "변수 이름이 비어 있습니다", AdminError(shared))
	assert.Equal(t, "변수 이름이 비어 있습니다", DepartmentSafeError("ctx", shared))

	// Nothing is withheld from an admin: an unwritten error arrives whole,
	// paths and all, which is exactly what they need.
	raw := errors.New("readlink /srv/gitea/data/company-apps/x/current: no such file")
	assert.Equal(t, raw.Error(), AdminError(raw))
	assert.NotContains(t, DepartmentSafeError("ctx", raw), "/srv")
}

// The message that prompted the split: an admin pressing start on an app
// with nothing built must not be told to file a deploy request.
func TestNoReleaseSpeaksToBothAudiences(t *testing.T) {
	assert.Contains(t, DepartmentSafeError("ctx", errNoRelease), "Deploy Request")
	assert.NotContains(t, AdminError(errNoRelease), "Deploy Request")
	assert.Contains(t, AdminError(errNoRelease), "승인")
}

// An app's own output names the server's paths — a Python traceback prints
// the absolute location of the venv and of the release tree, both inside the
// directory the sandbox exists to hide. A department reading its own logs
// must not be handed that.
func TestLogRedactionKeepsTracebacksReadable(t *testing.T) {
	prev := setting.AppDataPath
	setting.AppDataPath = "/Users/someone/gitea/data"
	t.Cleanup(func() { setting.AppDataPath = prev })

	line := `  File "/Users/someone/gitea/data/company-apps/96462367fd540a84/venvs/831e275f/lib/python3.14/site-packages/uvicorn/server.py", line 81, in serve`
	got := RedactServerPaths(line)

	assert.NotContains(t, got, "/Users/someone")
	assert.NotContains(t, got, "gitea/data")
	// Still a traceback: the file, the line and the function are what makes
	// one useful, and none of them are the server's business to hide.
	assert.Contains(t, got, "uvicorn/server.py")
	assert.Contains(t, got, "line 81, in serve")
}
