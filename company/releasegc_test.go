// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeRelease creates a release directory pointing at a venv, with a
// controllable age so the newest-first ordering can be tested.
func makeRelease(t *testing.T, p appPaths, name, venvKey string, age time.Duration) string {
	t.Helper()
	release := filepath.Join(p.releases, name)
	require.NoError(t, os.MkdirAll(filepath.Join(release, "app"), 0o700))

	venv := filepath.Join(p.home, "venvs", venvKey)
	require.NoError(t, os.MkdirAll(filepath.Join(venv, "bin"), 0o700))
	require.NoError(t, os.Symlink(venv, filepath.Join(release, ".venv")))

	when := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(release, when, when))
	return release
}

func TestGCReleasesKeepsTheNewest(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")

	var releases []string
	for i := range 8 {
		releases = append(releases, makeRelease(t, p,
			"r"+string(rune('a'+i)), "venv"+string(rune('a'+i)), time.Duration(i)*time.Hour))
	}
	// releases[0] is newest, releases[7] is oldest.
	require.NoError(t, swapSymlink(p.current, releases[0]))
	require.NoError(t, swapSymlink(p.previous, releases[1]))

	gcReleases("PO", "app")

	for i := range releasesKept {
		assert.DirExists(t, releases[i], "the newest %d releases stay", releasesKept)
	}
	for i := releasesKept; i < len(releases); i++ {
		assert.NoDirExists(t, releases[i], "release %d should have been collected", i)
	}
}

// The running release and the rollback target survive no matter how old they
// are. An app nobody has deployed in months would otherwise have the release
// it is currently serving deleted out from under it.
func TestGCReleasesNeverDeletesCurrentOrPrevious(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")

	ancient := makeRelease(t, p, "ancient", "venv-ancient", 5000*time.Hour)
	rollback := makeRelease(t, p, "rollback", "venv-rollback", 4000*time.Hour)
	require.NoError(t, swapSymlink(p.current, ancient))
	require.NoError(t, swapSymlink(p.previous, rollback))

	// Enough newer releases to fill every slot several times over.
	for i := range 10 {
		makeRelease(t, p, "new"+string(rune('a'+i)), "venv-new", time.Duration(i)*time.Minute)
	}

	gcReleases("PO", "app")

	assert.DirExists(t, ancient, "the running release must survive")
	assert.DirExists(t, rollback, "the rollback target must survive")
	assert.DirExists(t, filepath.Join(p.home, "venvs", "venv-ancient"))
	assert.DirExists(t, filepath.Join(p.home, "venvs", "venv-rollback"))
}

// A venv is shared by every release with the same dependency set, so it has
// to be collected by reference and never by age — the newest venv can easily
// be the one an older, still-kept release needs.
func TestGCVenvsFollowReferences(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")

	// Two kept releases share one venv; a third, collectable release has its
	// own. A fourth venv belongs to nothing at all.
	keptA := makeRelease(t, p, "a", "shared", time.Minute)
	keptB := makeRelease(t, p, "b", "shared", 2*time.Minute)
	require.NoError(t, swapSymlink(p.current, keptA))

	orphanVenv := filepath.Join(p.home, "venvs", "orphan")
	require.NoError(t, os.MkdirAll(orphanVenv, 0o700))

	gcReleases("PO", "app")

	assert.DirExists(t, keptA)
	assert.DirExists(t, keptB)
	assert.DirExists(t, filepath.Join(p.home, "venvs", "shared"),
		"a venv two kept releases point at must survive")
	assert.NoDirExists(t, orphanVenv, "a venv nothing references is what actually fills the disk")
}

// An app that has never deployed has no directories at all; collecting must
// be a no-op rather than an error.
func TestGCReleasesOnEmptyApp(t *testing.T) {
	withTempAppData(t)
	assert.NotPanics(t, func() { gcReleases("PO", "never-deployed") })
}
