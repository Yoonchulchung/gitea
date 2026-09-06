// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
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

// PreviousRelease is where "이전 버전으로" would take the app, and reports
// nothing when there is nowhere to go.
//
// A `previous` pointing at the release already running counts as nowhere.
// That state is reachable — an automatic rollback used to leave it behind,
// and a deploy sets it to whatever was current a moment before — and offering
// it produces a button that stops the app, starts the same version again, and
// reports success while nothing has changed. Whoever pressed it is left
// believing they went back a version.
func PreviousRelease(owner, repo string) ReleaseInfo {
	p := appPathsFor(owner, repo)
	target, err := os.Readlink(p.previous)
	if err != nil {
		return ReleaseInfo{}
	}
	if current, err := os.Readlink(p.current); err == nil && current == target {
		return ReleaseInfo{}
	}
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

// backfillReleaseSHAs identifies releases built before they recorded their own
// commit id.
//
// The directory name is sha256(sha)[:16], which is one-way, so the id cannot
// be read back out of it — but it can be confirmed. Hashing each commit in the
// central deploy repository and looking for a directory with that name
// recovers the mapping exactly, with no guessing: a match is proof, because
// nothing else hashes to that name.
//
// Needed because the alternative is silence. Without it, an app whose release
// predates this leaves every screen unable to say which version is serving,
// including the deploy history where the whole question is which of these
// attempts is the one still running.
//
// Runs once at startup, is skipped entirely for apps whose releases already
// carry the file, and is best-effort throughout — this is a repair, and
// failing it should cost the label rather than the boot.
func backfillReleaseSHAs(ctx context.Context) {
	states := ListAppStates()
	unidentified := map[string][]string{} // appKey -> release dirs missing the record
	for _, st := range states {
		p := appPathsFor(st.Owner, st.Repo)
		entries, err := os.ReadDir(p.releases)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			dir := filepath.Join(p.releases, entry.Name())
			if entry.IsDir() && readReleaseSHA(dir) == "" {
				unidentified[st.Owner+"/"+st.Repo] = append(unidentified[st.Owner+"/"+st.Repo], dir)
			}
		}
	}
	if len(unidentified) == 0 {
		return
	}

	commits, err := centralDeployCommitIDs(ctx)
	if err != nil {
		log.Warn("company: could not read the central deploy history to identify old releases: %v", err)
		return
	}
	// Indexed by directory name, so this is one pass over the commits rather
	// than one pass per release.
	byName := make(map[string]string, len(commits))
	for _, sha := range commits {
		sum := sha256.Sum256([]byte(sha))
		byName[hex.EncodeToString(sum[:16])] = sha
	}

	for app, dirs := range unidentified {
		for _, dir := range dirs {
			sha, ok := byName[filepath.Base(dir)]
			if !ok {
				continue // its commit is no longer in the history; nothing to recover from
			}
			if err := writeReleaseSHA(dir, sha); err != nil {
				log.Error("company: recording %s for %s: %v", sha, dir, err)
				continue
			}
			log.Info("company: identified an existing release of %s as %s", app, sha)
		}
	}
}

// centralDeployCommitIDs lists every commit the deploy repository still has.
func centralDeployCommitIDs(ctx context.Context) ([]string, error) {
	owner, name, err := centralDeployOwnerName()
	if err != nil {
		return nil, err
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	gitRepo, err := git.OpenRepository(ctx, central)
	if err != nil {
		return nil, err
	}
	defer gitRepo.Close()

	commit, err := gitRepo.GetBranchCommit(ctx, central.DefaultBranch)
	if err != nil {
		return nil, err
	}
	// Bounded: a release older than this many deploys has been cleaned up long
	// since, and the point is to identify what is on disk now.
	commits, err := commit.CommitsByRange(ctx, gitRepo, 1, backfillCommitScan, "", "", "")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(commits))
	for _, c := range commits {
		out = append(out, c.ID.String())
	}
	return out, nil
}

// backfillCommitScan bounds how far back the identification looks.
const backfillCommitScan = 500

// releaseManifestFile lists what the release's environment actually contains,
// beside the commit id and for the same reason: the answer belongs with the
// thing it describes rather than somewhere that can drift from it.
const releaseManifestFile = "packages"

func writeReleaseManifest(release string, packages []string) error {
	return os.WriteFile(filepath.Join(release, releaseManifestFile),
		[]byte(strings.Join(packages, "\n")+"\n"), 0o600)
}

// InstalledPackages is what the app's current release really has installed.
//
// Distinct from the allowlist, which is what it *may* install. Policy changes
// without the app being rebuilt, so approving a package does not put it in
// the running environment — and until now every screen showed the policy in a
// panel titled as though it were the contents.
//
// Empty for a release built before this was recorded, which reads as "not
// known" rather than "nothing installed".
func InstalledPackages(owner, repo string) []string {
	target, err := os.Readlink(appPathsFor(owner, repo).current)
	if err != nil {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(target, releaseManifestFile))
	if err != nil {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
