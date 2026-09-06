// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"strings"

	"gitea.dev/modules/log"
)

// Which version is an app actually running?
//
// The `current` symlink is the truth — startLocked follows it, so that is what
// the process runs after any restart. But the release directory is named
// sha256(sha)[:16] so that a commit id can never be used as a path component
// (releaseDir in appproc.go), and that hash is one-way: from the symlink alone
// the platform cannot say which commit is serving.
//
// So the state file's SHA was the only record, and it is written by whoever
// *intended* a version to run rather than by whatever ended up running. A
// rollback moves the symlink without moving that field, and from then on the
// screen names one commit while the process runs another — and "다시 배포"
// rebuilds the commit that just failed.
//
// The fix is to stop keeping the answer somewhere else. Each release records
// its own commit id inside itself, so the pair (symlink, file) is written by
// the same act that puts the code on disk and cannot drift from it. Nothing
// here is cached: every read goes to the file.

// releaseSHAFile sits beside `app/` rather than inside it, so it is never part
// of what the app serves.
const releaseSHAFile = "sha"

// writeReleaseSHA records which commit a release directory holds.
func writeReleaseSHA(release, sha string) error {
	return os.WriteFile(filepath.Join(release, releaseSHAFile), []byte(sha+"\n"), 0o600)
}

// readReleaseSHA reads it back, empty if the release predates this record or
// has been cleaned up.
func readReleaseSHA(release string) string {
	if release == "" {
		return ""
	}
	body, err := os.ReadFile(filepath.Join(release, releaseSHAFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

// CurrentReleaseSHA is the commit the app will run the next time it starts —
// read from disk, so it is correct after a rollback and after a restart.
func CurrentReleaseSHA(owner, repo string) string {
	p := appPathsFor(owner, repo)
	target, err := os.Readlink(p.current)
	if err != nil {
		return ""
	}
	return readReleaseSHA(target)
}

// ReleaseInfo is what a release is and when it was built, for the screens
// where someone is deciding whether to move off it.
type ReleaseInfo struct {
	SHA     string
	BuiltAt int64 // 0 when the release is gone
	Exists  bool
}

// describeRelease reads one release directory.
//
// BuiltAt comes from the sha file's timestamp rather than a recorded field:
// it is written once, at the start of the build, and never touched again, so
// its mtime is the build time without a second thing to keep in sync. Releases
// built before that file existed still get a time from the directory, which is
// what makes "이전 버전으로" say something useful about them rather than
// nothing.
func describeRelease(dir string) ReleaseInfo {
	if dir == "" {
		return ReleaseInfo{}
	}
	info := ReleaseInfo{SHA: readReleaseSHA(dir)}
	if st, err := os.Stat(filepath.Join(dir, releaseSHAFile)); err == nil {
		info.BuiltAt, info.Exists = st.ModTime().Unix(), true
		return info
	}
	if st, err := os.Stat(dir); err == nil {
		info.BuiltAt, info.Exists = st.ModTime().Unix(), true
	}
	return info
}

// CurrentRelease is what the app is serving; PreviousRelease is where "이전
// 버전으로" would take it. Both read the symlinks, so both are right after a
// rollback.
func CurrentRelease(owner, repo string) ReleaseInfo {
	target, _ := os.Readlink(appPathsFor(owner, repo).current)
	return describeRelease(target)
}

func PreviousRelease(owner, repo string) ReleaseInfo {
	target, _ := os.Readlink(appPathsFor(owner, repo).previous)
	return describeRelease(target)
}

// adoptCurrentReleaseSHA points the state at whatever is actually on disk.
//
// Called wherever the symlink moves for a reason other than a deploy — a
// rollback, or startup reconciliation after one. Silent when the release has
// no record: an app deployed before this existed should keep the SHA it has
// rather than have it blanked.
func adoptCurrentReleaseSHA(st *AppState, owner, repo string) {
	sha := CurrentReleaseSHA(owner, repo)
	if sha == "" || sha == st.SHA {
		return
	}
	log.Info("company: %s/%s is serving %s; state said %s", owner, repo, sha, st.SHA)
	st.SHA = sha
}
