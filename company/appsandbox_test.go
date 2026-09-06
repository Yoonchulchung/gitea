// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
