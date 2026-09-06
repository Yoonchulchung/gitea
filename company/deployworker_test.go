// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tree entry reaches this from data staff control, so a name that walks out
// of the release directory must not be joined blindly. Rooting the name
// before joining is what confines it — "../x" becomes "/x" becomes
// "<base>/x" — and the containment check afterwards is the assertion that
// this held.
func TestSafeJoin(t *testing.T) {
	base := t.TempDir()
	for name, want := range map[string]string{
		"main.py":          "main.py",
		"pkg/mod.py":       "pkg/mod.py",
		"./a.py":           "a.py",
		"../escape.py":     "escape.py",
		"../../etc/passwd": "etc/passwd",
		"/etc/passwd":      "etc/passwd",
		"a/../../b.py":     "b.py",
	} {
		got, err := safeJoin(base, name)
		require.NoError(t, err, name)
		assert.Equal(t, filepath.Join(base, want), got, name)
	}
}

// The venv cache key must depend on the dependency set and nothing else —
// otherwise every commit rebuilds it (minutes per deploy) or, worse, two
// different sets share one.
func TestRequirementsKey(t *testing.T) {
	a, _ := ParseRequirements("fastapi==0.115.0\nuvicorn==0.30.6")
	b, _ := ParseRequirements("uvicorn==0.30.6\nfastapi==0.115.0")
	c, _ := ParseRequirements("fastapi==0.115.1\nuvicorn==0.30.6")

	assert.Equal(t, requirementsKey(a), requirementsKey(b), "file order must not change the key")
	assert.NotEqual(t, requirementsKey(a), requirementsKey(c), "a version bump must rebuild")
	assert.NotEqual(t, requirementsKey(a), requirementsKey(nil))
}

// current must never be absent, even for an instant: the proxy reads it and
// the supervisor starts from it.
func TestSwapSymlinkIsAtomic(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "current")
	first := filepath.Join(dir, "r1")
	second := filepath.Join(dir, "r2")
	require.NoError(t, os.Mkdir(first, 0o700))
	require.NoError(t, os.Mkdir(second, 0o700))

	require.NoError(t, swapSymlink(link, first))
	target, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, first, target)

	// Repointing an existing link must succeed, not fail with EEXIST.
	require.NoError(t, swapSymlink(link, second))
	target, err = os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, second, target)
}

func TestLastLines(t *testing.T) {
	assert.Equal(t, "c\nd", lastLines("a\nb\nc\nd\n", 2))
	assert.Equal(t, "a\nb", lastLines("a\nb", 5))
}

// A full queue must reject with a clear reason rather than block the merge
// notification it is called from.
func TestEnqueueDeployRejectsWhenFull(t *testing.T) {
	withTempAppData(t)

	saved := deployQueue
	deployQueue = make(chan deployJob, 1)
	t.Cleanup(func() { deployQueue = saved })

	enqueueDeploy("PO", "first", "sha1", 1)
	enqueueDeploy("PO", "second", "sha2", 2)

	assert.Len(t, deployQueue, 1)
	st := LoadAppState("PO", "second")
	assert.Equal(t, AppStateFailed, st.Actual)
	assert.Equal(t, ReasonDeployQueueFull, st.Reason)

	// The accepted job carries no failure: enqueueDeploy only records the
	// rejection, leaving the queued state its caller already wrote.
	assert.Empty(t, LoadAppState("PO", "first").Reason)
}
