// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"sort"

	"gitea.dev/modules/log"
)

// Old releases and their virtualenvs are deleted after a successful deploy.
//
// Without this the tree only ever grows: a new directory per deployed commit,
// and a new venv per distinct dependency set — which for anything with numpy
// or pandas in it is hundreds of megabytes each. There is no disk quota to
// catch it, because quotas need root and this platform has none
// (docs/company/app-platform.md), so the only thing standing between a
// frequently-deployed app and a full volume is this function.

// releasesKept is how many past releases stay on disk. It bounds the rollback
// menu as much as the disk: keeping more than the state file's history
// (appHistoryLimit) would leave releases nothing can ever roll back to.
const releasesKept = 5

// gcReleases removes releases and venvs nothing needs any more.
//
// Callers must hold the app's deployMu. Everything here is best-effort and
// logged rather than returned: reclaiming disk is housekeeping, and a deploy
// that otherwise succeeded must not be reported as failed because a stale
// directory could not be removed.
func gcReleases(owner, repo string) {
	p := appPathsFor(owner, repo)

	// The live release and the rollback target are never candidates. Reading
	// them first, before listing anything, is what makes this safe: if either
	// link cannot be read the set of protected paths is empty and the keep
	// count below still spares the newest entries.
	protected := map[string]bool{}
	for _, link := range []string{p.current, p.previous} {
		if target, err := os.Readlink(link); err == nil {
			protected[target] = true
		}
	}

	kept := gcReleaseDirs(p, protected)
	gcVenvs(p, kept)
}

// gcReleaseDirs deletes all but the newest releasesKept directories, and
// returns the ones that remain.
func gcReleaseDirs(p appPaths, protected map[string]bool) []string {
	entries, err := os.ReadDir(p.releases)
	if err != nil {
		return nil // nothing deployed yet
	}

	type release struct {
		path    string
		modTime int64
	}
	candidates := make([]release, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(p.releases, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, release{path: path, modTime: info.ModTime().Unix()})
	}
	// Newest first, so the tail is what gets removed.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].modTime > candidates[j].modTime })

	var kept []string
	for i, c := range candidates {
		// Protected releases are kept regardless of age *and* do not consume
		// a slot — an app that has not deployed in months would otherwise see
		// its own running release age out from under it.
		if protected[c.path] {
			kept = append(kept, c.path)
			continue
		}
		if i < releasesKept {
			kept = append(kept, c.path)
			continue
		}
		if err := os.RemoveAll(c.path); err != nil {
			log.Warn("company: could not remove old release %s: %v", c.path, err)
			kept = append(kept, c.path) // still on disk, so still referencing its venv
		}
	}
	return kept
}

// gcVenvs deletes virtualenvs no surviving release points at.
//
// Driven by what the releases actually reference rather than by age: a venv
// is shared by every release with the same dependency set, so the newest one
// can easily be the one an old release still needs, and deleting by age would
// break exactly the release someone is about to roll back to.
func gcVenvs(p appPaths, keptReleases []string) {
	venvRoot := filepath.Join(p.home, "venvs")
	entries, err := os.ReadDir(venvRoot)
	if err != nil {
		return
	}

	inUse := map[string]bool{}
	for _, release := range keptReleases {
		if target, err := os.Readlink(filepath.Join(release, ".venv")); err == nil {
			inUse[target] = true
		}
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(venvRoot, entry.Name())
		if inUse[path] {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			log.Warn("company: could not remove unused virtualenv %s: %v", path, err)
		}
	}
}
