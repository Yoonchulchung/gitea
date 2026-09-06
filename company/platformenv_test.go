// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every wrong guess the assistant makes about the environment costs a
// department a failed deploy. The Python version is stated with its
// consequence attached, because a model handed a bare fact still guesses at
// what to do with it — and this particular guess is what broke the first
// real deploy on this platform.
func TestAIContextExplainsWhatThePythonVersionMeans(t *testing.T) {
	env := PlatformEnvironment{
		PythonVersion: "3.12.3",
		BasePackages:  []string{"fastapi", "uvicorn", "pydantic"},
		AllowedExtra:  []string{"openpyxl"},
		MemoryLimitMB: 192,
		StartCommand:  "uvicorn main:app --uds ${SOCKET}",
		NetworkMode:   NetworkNone,
		// The enforced case: the unenforced one is its own test, because the
		// two must not say the same thing.
		NetworkEnforced: true,
	}
	got := env.AIContext()

	assert.Contains(t, got, "3.12.3")
	assert.Contains(t, got, "No matching distribution found",
		"the version alone does not tell a model what to avoid")
	assert.Contains(t, got, "do NOT add these to requirements.txt")
	assert.Contains(t, got, "openpyxl")
	assert.Contains(t, got, "192")
	assert.Contains(t, got, "NO network access")
	assert.Contains(t, got, "main.py")
}

// A host with no interpreter is worth saying out loud: nothing deploys until
// an administrator fixes it, and the employee is otherwise left waiting.
func TestAIContextSaysWhenPythonIsMissing(t *testing.T) {
	got := PlatformEnvironment{PythonError: "python3 was not found"}.AIContext()
	assert.Contains(t, got, "no usable Python")
	assert.Contains(t, got, "administrator")
	assert.NotContains(t, got, "Any dependency you add", "there is nothing to add it to")
}

// The snapshot is assembled from policy and probes — nothing in it is
// executed on demand, and nothing comes from the assistant.
func TestDescribePlatformEnvironmentReadsPolicy(t *testing.T) {
	cfg, err := ParseAppsConfig([]byte(`
defaults:
  basePackages: [fastapi]
  limits: {memoryMB: 256}
apps:
  PO/app:
    network: {mode: none}
    dependencies:
      allowExtra: [openpyxl]
`))
	require.NoError(t, err)
	SetAppsConfig(cfg)
	t.Cleanup(func() { SetAppsConfig(&AppsConfig{}) })

	env := DescribePlatformEnvironment("PO", "app")
	assert.Equal(t, []string{"fastapi"}, env.BasePackages)
	assert.Equal(t, []string{"openpyxl"}, env.AllowedExtra)
	assert.Equal(t, 256, env.MemoryLimitMB)
	assert.Equal(t, NetworkNone, env.NetworkMode)
	assert.NotEmpty(t, env.SandboxMode)

	// A different repository gets the shared defaults and none of PO's
	// approvals — an approval for one department is not one for another.
	assert.Empty(t, DescribePlatformEnvironment("HR", "other").AllowedExtra)
}

func TestAIContextIsNotEmpty(t *testing.T) {
	assert.NotEmpty(t, strings.TrimSpace(PlatformEnvironment{}.AIContext()))
}
