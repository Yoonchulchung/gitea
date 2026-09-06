// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"strings"

	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// appKey used to be the full sha256 hex. It was shortened because the app's
// unix socket lives under a directory named by it and the full digest pushed
// that path past sun_path (see appstate.go), but shortening it renamed every
// directory the platform owns — so an instance that upgraded found its apps
// with no releases, no metrics and no secrets, reporting that they had never
// been deployed.
//
// The short key is a prefix of the long one, so the mapping is unambiguous
// and this can be done by renaming. It runs once at startup and is a no-op
// on any instance that never wrote a long-form name.

// oldAppKeyLength is the full sha256 hex length these names used to have.
const oldAppKeyLength = 64

// appKeyedDirs are every place a name is derived from appKey. Missing ones
// are skipped: an instance that never used a feature has no directory for it.
func appKeyedDirs() []string {
	return []string{
		filepath.Join(setting.AppDataPath, "company-app-state"),
		filepath.Join(setting.AppDataPath, "company-apps"),
		filepath.Join(setting.AppDataPath, "company-metrics"),
		filepath.Join(setting.AppDataPath, "company-env"),
		filepath.Join(setting.AppDataPath, "company-permissions"),
	}
}

// migrateAppKeyLength renames long-form entries to their short form.
func migrateAppKeyLength() {
	for _, dir := range appKeyedDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // never used on this instance
		}
		for _, entry := range entries {
			name, suffix := entry.Name(), ""
			if ext := filepath.Ext(name); ext != "" {
				name, suffix = strings.TrimSuffix(name, ext), ext
			}
			if len(name) != oldAppKeyLength || !isHex(name) {
				continue
			}

			from := filepath.Join(dir, entry.Name())
			to := filepath.Join(dir, name[:appKeyLength]+suffix)
			if _, err := os.Stat(to); err == nil {
				// Both names exist, which means the instance ran after the
				// upgrade and before this migration. During that window every
				// app looked as though it had never been deployed, so the
				// short-form entry is a record of that breakage rather than of
				// anything real — the long-form one is the actual history.
				//
				// Moved aside instead of deleted: this is someone's data, and
				// being wrong about which copy matters should cost them a file
				// to look at rather than the file itself.
				aside := to + ".post-upgrade"
				if err := os.Rename(to, aside); err != nil {
					log.Error("company: moving %s aside: %v", to, err)
					continue
				}
				log.Warn("company: %s was written before the app-key migration ran; kept as %s", to, aside)
			}
			if err := os.Rename(from, to); err != nil {
				log.Error("company: renaming %s to %s: %v", from, to, err)
				continue
			}
			if entry.IsDir() {
				repointSymlinks(to, from, to)
				discardStaleVenvs(to)
			}
			log.Info("company: migrated %s to the shortened app key", entry.Name())
		}
	}
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return s != ""
}

// repointSymlinks rewrites absolute symlinks under root that still refer to
// oldPrefix.
//
// An app's tree is held together by them — `current` and `previous` point at
// a release, and each release's `.venv` points at a shared environment — and
// they are absolute, so renaming the directory above leaves every one of
// them dangling. The app then has a release directory it cannot reach, which
// looks exactly like having no release at all.
func repointSymlinks(root, oldPrefix, newPrefix string) {
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.Type()&os.ModeSymlink == 0 {
			return nil //nolint:nilerr // a vanished entry is not a failure worth aborting the walk for
		}
		target, err := os.Readlink(path)
		if err != nil || !strings.HasPrefix(target, oldPrefix) {
			return nil //nolint:nilerr // relative or unrelated links are already correct
		}
		updated := newPrefix + strings.TrimPrefix(target, oldPrefix)
		tmp := path + ".tmp"
		_ = os.Remove(tmp)
		if err := os.Symlink(updated, tmp); err != nil {
			log.Error("company: repointing %s: %v", path, err)
			return nil
		}
		if err := os.Rename(tmp, path); err != nil {
			log.Error("company: repointing %s: %v", path, err)
		}
		return nil
	})
	if err != nil {
		log.Error("company: walking %s to repoint symlinks: %v", root, err)
	}
}

// discardStaleVenvs removes environments built under the old directory name.
//
// A virtualenv is not relocatable. Every console script in it starts with a
// shebang naming the interpreter by absolute path, so after the rename the
// script is still there and still executable while the interpreter it points
// at is not — which surfaces as "fork/exec .../bin/uvicorn: no such file or
// directory", an error that names a file which plainly exists.
//
// Deleting them is the honest repair: the next deploy rebuilds one, and a
// rebuilt environment is correct in a way a patched-up one would only appear
// to be.
func discardStaleVenvs(appHome string) {
	venvs := filepath.Join(appHome, "venvs")
	if _, err := os.Stat(venvs); err != nil {
		return
	}
	if err := os.RemoveAll(venvs); err != nil {
		log.Error("company: removing stale virtualenvs in %s: %v", venvs, err)
		return
	}
	log.Warn("company: removed virtualenvs under %s — they were built under the old path and cannot be moved; redeploy to rebuild", appHome)
}
