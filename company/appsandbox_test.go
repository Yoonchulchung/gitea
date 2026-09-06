// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildStartArgs(t *testing.T) {
	args := buildStartArgs(
		"uvicorn main:app --uds ${SOCKET} --root-path ${ROOT_PATH}",
		"/run/app.sock", "/apps/PO/x")
	assert.Equal(t,
		[]string{"uvicorn", "main:app", "--uds", "/run/app.sock", "--root-path", "/apps/PO/x"},
		args)

	// Split on whitespace, never through a shell — there is no shell in the
	// sandbox, and a shell would make the start string an injection surface.
	assert.Equal(t, []string{"uvicorn", "a;rm", "-rf", "/"}, buildStartArgs("uvicorn a;rm -rf /", "", ""))
	assert.Empty(t, buildStartArgs("   ", "", ""))
}

// The app binds where ${SOCKET} says and Gitea connects where appPaths says.
// Inside the sandbox those are two names for the same file, so they are
// derived from one function; if they ever disagreed the app would bind
// somewhere the proxy never looks and every request would 502.
func TestSocketPathsAgree(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")

	env := buildEnv(p, "/apps/PO/app", nil)
	assert.Contains(t, env, "SOCKET="+appSocketForProcess(p))

	if available, _ := SandboxStatus(); available {
		assert.Equal(t, sandboxSocketPath, appSocketForProcess(p),
			"sandboxed: the app's writable dir is mounted at /run")
	} else {
		assert.Equal(t, p.socket, appSocketForProcess(p), "unsandboxed: the real host path")
	}
}

// exec.Cmd with a nil Env hands the child Gitea's whole environment, which
// would include SECRET_KEY and database credentials.
func TestBuildEnvDoesNotInheritParent(t *testing.T) {
	withTempAppData(t)
	t.Setenv("GITEA__DATABASE__PASSWD", "super-secret")

	env := buildEnv(appPathsFor("PO", "app"), "/apps/PO/app", map[string]string{
		"API_KEY":    "sk-abc",
		"LD_PRELOAD": "/tmp/evil.so", // second guard behind envstore validation
	})

	for _, entry := range env {
		assert.NotContains(t, entry, "super-secret")
		assert.NotContains(t, entry, "LD_PRELOAD")
	}
	assert.Contains(t, env, "API_KEY=sk-abc")
}

// The department's rows state permission, never enforcement.
//
// Two mistakes are avoided at once. "차단됨" would promise a technical block
// this host may not be applying; saying the block is *not* applied would hand
// every person with repository access a working description of a hole only an
// administrator can close. What the app is allowed to do is true either way.
func TestDepartmentRowsStatePermissionNotEnforcement(t *testing.T) {
	rows := permissionRows(AppSettings{Network: AppNetwork{Mode: NetworkNone}}, &AppState{})

	var outbound *PermissionRow
	for i := range rows {
		if rows[i].Label == "외부 통신" {
			outbound = &rows[i]
		}
	}
	require.NotNil(t, outbound)
	assert.Equal(t, "허용되지 않음", outbound.Value)
	assert.Contains(t, outbound.Reason, "권한이 없습니다")

	// Whether the platform can currently impose it belongs on the admin
	// screen, and must not leak into anything a department reads.
	for _, row := range rows {
		assert.NotContains(t, row.Reason, "막을 수 없는")
		assert.NotContains(t, row.Value, "차단되지 않음")
	}
}

// The assistant must not be told calls "fail" on a host where they succeed:
// it would write code around a barrier that is not there, or trust one.
func TestAIContextDoesNotPromiseAnUnenforcedBlock(t *testing.T) {
	blocked := PlatformEnvironment{NetworkMode: NetworkNone, NetworkEnforced: true}.AIContext()
	assert.Contains(t, blocked, "NO network access")

	unenforced := PlatformEnvironment{NetworkMode: NetworkNone}.AIContext()
	assert.NotContains(t, unenforced, "NO network access")
	assert.Contains(t, unenforced, "cannot enforce")
	assert.Contains(t, unenforced, "not allowed, only unblocked")
}
