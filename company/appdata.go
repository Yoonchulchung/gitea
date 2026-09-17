// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// An app's data is the one thing this platform cannot rebuild. Releases,
// virtualenvs and logs all derive from git and can be thrown away; a
// department's database cannot.
//
// So it does not live under company-apps/<hash>/. That tree is keyed by
// sha256(owner+"/"+repo) and deleted wholesale when an app is removed, and
// both of those lose data — a rename orphans it, a removal destroys it. Here
// the key is the repository ID, which survives both, and removal happens in
// two steps instead of one. See docs/company/app-data.md.

const (
	appDataDirName  = "company-app-data"
	appDataMetaFile = ".meta.json"

	appDataRetentionDaysDefault = 90
	appDataGCInterval           = 6 * time.Hour
	// A sweep deletes data permanently. Doing that in the first seconds of a
	// boot leaves nobody able to read the log and intervene, and against a
	// 90-day clock the delay costs nothing.
	appDataGCInitialDelay = time.Hour
)

func appDataRoot() string { return filepath.Join(setting.AppDataPath, appDataDirName) }

// appDataDirFor is where one app's data physically lives.
//
// Named by repository ID: an int64 from the database, never user text. That
// is what makes the location survive a rename or a transfer, and it also
// means no part of this path can be steered by anything a person typed.
func appDataDirFor(repoID int64) string {
	return filepath.Join(appDataRoot(), strconv.FormatInt(repoID, 10))
}

// appDataMeta records whose directory this is.
//
// The names are for people reading an admin screen; nothing finds the
// directory by them. They are stored rather than looked up because they have
// to outlive the repository — a directory whose repo was deleted still has
// to be identifiable by a human deciding whether to keep it.
type appDataMeta struct {
	RepoID    int64  `json:"repoID"`
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	CreatedAt int64  `json:"createdAt"`
	RemovedAt int64  `json:"removedAt,omitempty"` // soft delete; 0 means live
}

func loadAppDataMeta(dir string) (appDataMeta, error) {
	var meta appDataMeta
	body, err := readFileIfExists(filepath.Join(dir, appDataMetaFile))
	if err != nil || body == nil {
		return meta, err
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return appDataMeta{}, err
	}
	return meta, nil
}

func saveAppDataMeta(dir string, meta appDataMeta) error {
	body, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, appDataMetaFile), body)
}

// resolveAppDataRepoID maps an app to the ID its data is filed under.
//
// Deliberately not cached. A cache keyed by owner/repo goes stale the moment
// a repository is renamed, and if a new repository later takes the freed
// name the stale entry would hand it another app's database. This is one
// indexed query on deploy and app start, not on every request.
func resolveAppDataRepoID(ctx context.Context, owner, repo string) (int64, error) {
	r, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	return r.ID, nil
}

// AppDataDir returns the app's data directory, creating it on first use.
//
// A failure is returned, never worked around. The tempting fallback — carry
// on with a fresh directory — is indistinguishable from data loss to the
// person whose records are suddenly empty.
//
// Reaching for the directory also clears a pending removal: every caller
// here is the app being live again, and that is exactly what undoes a soft
// delete. Making it a separate call would mean one forgotten line silently
// leaves a live app's data on a deletion clock.
func AppDataDir(ctx context.Context, owner, repo string) (string, error) {
	id, err := resolveAppDataRepoID(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("locating the data directory for %s/%s: %w", owner, repo, err)
	}
	return ensureAppDataDir(id, owner, repo)
}

// ensureAppDataDir is AppDataDir once the ID is known — split out so the
// directory and metadata behaviour can be tested without a database.
func ensureAppDataDir(id int64, owner, repo string) (string, error) {
	dir := appDataDirFor(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	meta, err := loadAppDataMeta(dir)
	if err != nil {
		return "", err
	}
	was := meta
	if meta.CreatedAt == 0 {
		meta.CreatedAt = time.Now().Unix()
	}
	meta.RepoID, meta.Owner, meta.Repo, meta.RemovedAt = id, owner, repo, 0
	if meta != was {
		if err := saveAppDataMeta(dir, meta); err != nil {
			return "", err
		}
		if was.RemovedAt != 0 {
			log.Info("company: %s/%s: data restored — it was scheduled for deletion", owner, repo)
		}
	}
	return dir, nil
}

// MarkAppDataRemoved starts the retention clock without touching the data.
//
// Never fails a removal. If the repository is already gone the directory
// cannot be found by name, and the sweep below starts the same clock when it
// notices the ID no longer resolves — so the one thing that must not happen,
// data outliving its clock forever, cannot.
func MarkAppDataRemoved(ctx context.Context, owner, repo string) {
	id, err := resolveAppDataRepoID(ctx, owner, repo)
	if err != nil {
		log.Warn("company: %s/%s: could not mark data removed (%v); the sweep will pick it up", owner, repo, err)
		return
	}
	dir := appDataDirFor(id)
	if _, err := os.Stat(dir); err != nil {
		return // never had any data
	}
	meta, err := loadAppDataMeta(dir)
	if err != nil {
		log.Warn("company: %s/%s: unreadable data metadata: %v", owner, repo, err)
		return
	}
	if meta.RemovedAt != 0 {
		return
	}
	meta.RepoID, meta.Owner, meta.Repo, meta.RemovedAt = id, owner, repo, time.Now().Unix()
	if meta.CreatedAt == 0 {
		meta.CreatedAt = meta.RemovedAt
	}
	if err := saveAppDataMeta(dir, meta); err != nil {
		log.Error("company: %s/%s: marking data removed: %v", owner, repo, err)
	}
}

// PurgeAppData deletes one app's data permanently. There is no way back from
// here, which is why nothing calls it except the sweep and an administrator
// who confirmed it.
func PurgeAppData(repoID int64) error {
	return os.RemoveAll(appDataDirFor(repoID))
}

// AppDataArchive is one soft-deleted directory, for the admin screen.
type AppDataArchive struct {
	appDataMeta
	Bytes int64
	Until time.Time
}

// ListAppDataArchives returns what is being kept and for how much longer.
func ListAppDataArchives() []AppDataArchive {
	retention := appDataRetention()
	var out []AppDataArchive
	for _, dir := range appDataDirs() {
		meta, err := loadAppDataMeta(dir)
		if err != nil || meta.RemovedAt == 0 {
			continue
		}
		removed := time.Unix(meta.RemovedAt, 0)
		out = append(out, AppDataArchive{
			appDataMeta: meta,
			Bytes:       appDataBytes(dir),
			Until:       removed.Add(retention),
		})
	}
	return out
}

// appDataDirs lists the per-app directories, skipping anything that is not
// one — an ID is all digits, so a stray file or a leftover temp directory is
// recognisable without guessing.
func appDataDirs() []string {
	entries, err := os.ReadDir(appDataRoot())
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.ParseInt(entry.Name(), 10, 64); err != nil {
			continue
		}
		dirs = append(dirs, filepath.Join(appDataRoot(), entry.Name()))
	}
	return dirs
}

// appDataBytes sizes a directory without following symlinks.
//
// The app runs as this same user and can put a symlink in its own data
// directory. Following one would have the platform charge another
// directory's contents — or the whole volume — against this app's quota.
func appDataBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal
		}
		info, err := d.Info() // Lstat: never resolves a symlink
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// appDataRetention is how long soft-deleted data is kept.
//
// A value of zero or less is ignored rather than honoured: read literally it
// means "delete immediately", which would turn one typo in app.ini into
// irreversible deletion across every archived app.
func appDataRetention() time.Duration {
	days := companySettingPositiveInt("APP_DATA_RETENTION_DAYS", appDataRetentionDaysDefault)
	return time.Duration(days) * 24 * time.Hour
}

// sweepAppData is the hard-delete pass, and the orphan detector.
//
// exists reports whether a repository ID is still a repository; it is a
// parameter so the decision logic can be tested without a database. A lookup
// error is propagated as an error rather than as "gone" — starting deletion
// clocks because the database blinked is exactly the bug this must not have.
func sweepAppData(now time.Time, retention time.Duration, exists func(int64) (bool, error)) {
	for _, dir := range appDataDirs() {
		meta, err := loadAppDataMeta(dir)
		if err != nil {
			log.Warn("company: unreadable data metadata in %s; leaving it alone: %v", filepath.Base(dir), err)
			continue
		}
		if meta.RepoID == 0 {
			continue // never written by us; not ours to delete
		}

		if meta.RemovedAt == 0 {
			live, err := exists(meta.RepoID)
			if err != nil || live {
				continue
			}
			meta.RemovedAt = now.Unix()
			if err := saveAppDataMeta(dir, meta); err != nil {
				log.Error("company: starting the retention clock for %s/%s: %v", meta.Owner, meta.Repo, err)
				continue
			}
			log.Info("company: %s/%s: repository is gone; keeping its data until %s",
				meta.Owner, meta.Repo, now.Add(retention).Format(time.DateOnly))
			continue
		}

		if now.Sub(time.Unix(meta.RemovedAt, 0)) < retention {
			continue
		}
		// An orphan whose repository was deleted can still have a process
		// running: nothing stopped it. Deleting the database underneath a
		// live writer turns a scheduled cleanup into a corruption report.
		if IsAppRunning(meta.Owner, meta.Repo) {
			log.Warn("company: %s/%s: data is past its retention but the app is still running; not deleting",
				meta.Owner, meta.Repo)
			continue
		}
		if err := PurgeAppData(meta.RepoID); err != nil {
			log.Error("company: deleting expired data for %s/%s: %v", meta.Owner, meta.Repo, err)
			continue
		}
		log.Info("company: %s/%s: data deleted after %s in retention", meta.Owner, meta.Repo, retention)
	}
}

// StartAppDataGC runs the retention sweep for as long as this process lives.
func StartAppDataGC() {
	ctx := graceful.GetManager().ShutdownContext()
	exists := func(id int64) (bool, error) {
		if _, err := repo_model.GetRepositoryByID(ctx, id); err != nil {
			if repo_model.IsErrRepoNotExist(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}

	go func() {
		timer := time.NewTimer(appDataGCInitialDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		ticker := time.NewTicker(appDataGCInterval)
		defer ticker.Stop()
		for {
			sweepAppData(time.Now(), appDataRetention(), exists)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// AppDataEnabled is the master switch. Off by default: an instance that has
// never needed app data should not grow a directory tree, a sweep and a
// mount point for it.
func AppDataEnabled() bool { return companySetting("APP_DATA_ENABLED") == "true" }

// sandboxDataPath is where the data directory is mounted inside bubblewrap.
// Fixed, so an app's code never contains a path that depends on where its
// repository happens to live.
const sandboxDataPath = "/data"

const appDataDBName = "app.db"

// appDataDirForProcess is the data directory as the app itself sees it —
// the same split as appSocketForProcess, for the same reason: under
// bubblewrap the host path and the in-sandbox path are different names for
// one directory, and the two must never be allowed to disagree.
func appDataDirForProcess(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	if mode, _ := sandboxMode(); mode == SandboxBubblewrap {
		return sandboxDataPath
	}
	// Landlock has no mount namespace, so the app sees the real path.
	return dataDir
}

// appDataForStart resolves where this app's data lives, or "" when the
// feature is off.
//
// An error is not returned: a failure here must not take a running app off
// the air, and starting *without* the data directory would have the app
// create its database somewhere else entirely. So it fails the start, which
// is the only outcome that cannot silently lose data.
func appDataForStart(owner, repo string) (string, error) {
	if !AppDataEnabled() {
		return "", nil
	}
	ctx := graceful.GetManager().ShutdownContext()
	dir, err := AppDataDir(ctx, owner, repo)
	if err != nil {
		return "", audienceKeyError("company.err.data_dir", "company.err.data_dir.admin", err.Error())
	}
	return dir, nil
}

// dataInfo is what the filesystem under AppDataPath turned out to support.
type dataInfo struct {
	JournalMode   string // what SQLite actually gave us, not what we asked for
	SQLiteVersion string
}

// dataProbe finds out what this host can really do, rather than assuming
// Linux on a local disk.
//
// The whole trick is that `PRAGMA journal_mode=WAL` answers with the mode it
// *applied*. On a filesystem that cannot do WAL — NFS is the usual one —
// SQLite quietly stays where it was and says so. That turns "are we on a
// network mount?", which is guesswork, into reading a string back, which is
// not.
//
// Probed through Python rather than a Go driver: every app already needs an
// interpreter, and the migration runner will use the same one, so there is
// exactly one SQLite in play instead of two that can disagree.
var dataProbe = sync.OnceValues(func() (dataInfo, error) {
	info, err := pythonProbe()
	if err != nil {
		return dataInfo{}, fmt.Errorf("no interpreter to test the data directory with: %w", err)
	}
	if err := os.MkdirAll(appDataRoot(), 0o700); err != nil {
		return dataInfo{}, err
	}
	// Under the real root, because the answer is a property of *this*
	// filesystem and a temp dir elsewhere could be a different one.
	dir, err := os.MkdirTemp(appDataRoot(), ".probe-")
	if err != nil {
		return dataInfo{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	cmd := exec.Command(info.Path, "-", filepath.Join(dir, "probe.db")) //nolint:gosec // interpreter path is admin-owned config
	cmd.Stdin = strings.NewReader(dataProbeScript)
	out, err := cmd.Output()
	if err != nil {
		return dataInfo{}, fmt.Errorf("the data directory could not be tested: %w", err)
	}
	var result struct {
		OK      bool   `json:"ok"`
		Mode    string `json:"journalMode"`
		SQLite  string `json:"sqlite"`
		Message string `json:"error"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return dataInfo{}, fmt.Errorf("unreadable answer from the data directory test: %w", err)
	}
	if !result.OK {
		return dataInfo{}, errors.New(result.Message)
	}
	return dataInfo{JournalMode: result.Mode, SQLiteVersion: result.SQLite}, nil
})

const dataProbeScript = `
import json, sqlite3, sys
out = {"ok": False, "sqlite": sqlite3.sqlite_version, "journalMode": ""}
try:
    c = sqlite3.connect(sys.argv[1])
    out["journalMode"] = c.execute("PRAGMA journal_mode=WAL").fetchone()[0]
    c.execute("CREATE TABLE probe(x)")
    c.execute("INSERT INTO probe VALUES(1)")
    c.commit()
    c.close()
    out["ok"] = True
except Exception as e:
    out["error"] = str(e)
print(json.dumps(out))
`

// DataJournalMode is the journal mode apps' databases will be opened in.
//
// The probe decides, because it measured; the setting overrides, because a
// measurement can be wrong and an operator must be able to say so.
func DataJournalMode() string {
	switch override := strings.ToLower(companySetting("APP_DATA_JOURNAL_MODE")); override {
	case "wal", "truncate":
		return override
	case "", "auto":
	default:
		log.Warn("company: APP_DATA_JOURNAL_MODE=%q is not auto, wal or truncate; measuring instead", override)
	}
	info, err := dataProbe()
	if err != nil || !strings.EqualFold(info.JournalMode, "wal") {
		// Not a failure to report loudly at every call: the reason was
		// logged once at startup, and TRUNCATE is a working answer.
		return "truncate"
	}
	return "wal"
}

// DataStatus is the one-line answer for the admin screen and the boot log.
func DataStatus() (available bool, detail string) {
	if !AppDataEnabled() {
		return false, "app data is switched off ([company] APP_DATA_ENABLED)"
	}
	info, err := dataProbe()
	if err != nil {
		return false, err.Error()
	}
	mode := DataJournalMode()
	if !strings.EqualFold(info.JournalMode, "wal") {
		return true, fmt.Sprintf("SQLite %s, journal mode %s — this filesystem cannot do WAL", info.SQLiteVersion, mode)
	}
	return true, fmt.Sprintf("SQLite %s, journal mode %s", info.SQLiteVersion, mode)
}

const (
	appDataQuotaMBDefault     = 512
	appDataWarnPctDefault     = 80
	appDataHostFloorMBDefault = 1024
	// The walk is cheaper than it looks — the directory holds one database —
	// but it is still work, and a quota does not move in five seconds.
	dataSampleInterval = time.Minute
)

// companySettingPositiveInt reads a number that only makes sense above zero.
// A missing, unparseable or non-positive value keeps the default rather than
// being honoured: every setting here is a limit, and "0" read literally
// means either "no space at all" or "delete immediately".
func companySettingPositiveInt(key string, def int) int {
	raw := companySetting(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Warn("company: [company] %s=%q is not a positive number; using %d", key, raw, def)
		return def
	}
	return n
}

// AppDataUsage is how much of its allowance an app has spent.
type AppDataUsage struct {
	Bytes      int64
	QuotaBytes int64
	Percent    int
	MeasuredAt time.Time
}

// Warning is when the department should be told, while there is still room
// to act.
func (u AppDataUsage) Warning() bool {
	return u.QuotaBytes > 0 && u.Percent >= companySettingPositiveInt("APP_DATA_WARN_PCT", appDataWarnPctDefault)
}

// Full stops new deploys. Not the app: a database at its limit still serves
// every read, and stopping it would turn a full disk into an outage.
func (u AppDataUsage) Full() bool { return u.QuotaBytes > 0 && u.Bytes >= u.QuotaBytes }

func appDataQuotaBytes(settings AppSettings) int64 {
	mb := settings.Limits.DataMB
	if mb <= 0 {
		mb = companySettingPositiveInt("APP_DATA_QUOTA_MB", appDataQuotaMBDefault)
	}
	return int64(mb) << 20
}

func measureAppDataUsage(dir string, settings AppSettings) AppDataUsage {
	usage := AppDataUsage{
		Bytes:      appDataBytes(dir),
		QuotaBytes: appDataQuotaBytes(settings),
		MeasuredAt: time.Now(),
	}
	if usage.QuotaBytes > 0 {
		usage.Percent = int(usage.Bytes * 100 / usage.QuotaBytes)
	}
	return usage
}

var (
	dataUsageMu    sync.Mutex
	dataUsageCache = map[string]AppDataUsage{}
)

// AppDataUsageFor answers the screens. Measured on demand when no sample has
// landed yet, because an app that is stopped is never sampled and its usage
// is exactly what someone looking at a full disk needs to see.
func AppDataUsageFor(ctx context.Context, owner, repo string) (AppDataUsage, bool) {
	if !AppDataEnabled() {
		return AppDataUsage{}, false
	}
	key := appKey(owner, repo)
	dataUsageMu.Lock()
	usage, ok := dataUsageCache[key]
	dataUsageMu.Unlock()
	if ok && time.Since(usage.MeasuredAt) < dataSampleInterval {
		return usage, true
	}

	id, err := resolveAppDataRepoID(ctx, owner, repo)
	if err != nil {
		return usage, ok
	}
	return AppDataUsageForRepoID(owner, repo, id)
}

// AppDataUsageForRepoID is the same answer for a caller that already holds
// the repository — the app list holds one per row, and looking each of them
// up again by name would be a query per line of the page.
func AppDataUsageForRepoID(owner, repo string, repoID int64) (AppDataUsage, bool) {
	if !AppDataEnabled() {
		return AppDataUsage{}, false
	}
	key := appKey(owner, repo)
	dataUsageMu.Lock()
	usage, ok := dataUsageCache[key]
	dataUsageMu.Unlock()
	if ok && time.Since(usage.MeasuredAt) < dataSampleInterval {
		return usage, true
	}

	settings := SettingsFor(owner, repo)
	dir := appDataDirFor(repoID)
	if _, err := os.Stat(dir); err != nil {
		return AppDataUsage{QuotaBytes: appDataQuotaBytes(settings)}, true
	}
	usage = measureAppDataUsage(dir, settings)
	dataUsageMu.Lock()
	dataUsageCache[key] = usage
	dataUsageMu.Unlock()
	return usage, true
}

// checkDataLimit is the watchdog half, called from the resource sampler.
//
// The ladder is deliberately gentler than the memory one. Memory is stopped
// because a runaway app threatens the machine; a full app database threatens
// only its own writes, and the app still answers reads. What genuinely
// threatens the machine is the *volume* running out — gitea.db lives on it —
// and that is the only condition here that takes an app off the air.
func checkDataLimit(owner, repo string, settings AppSettings) {
	if !AppDataEnabled() {
		return
	}
	key := appKey(owner, repo)
	dataUsageMu.Lock()
	last, seen := dataUsageCache[key]
	dataUsageMu.Unlock()
	if seen && time.Since(last.MeasuredAt) < dataSampleInterval {
		return
	}

	ctx := graceful.GetManager().ShutdownContext()
	id, err := resolveAppDataRepoID(ctx, owner, repo)
	if err != nil {
		return
	}
	dir := appDataDirFor(id)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	usage := measureAppDataUsage(dir, settings)
	dataUsageMu.Lock()
	dataUsageCache[key] = usage
	dataUsageMu.Unlock()

	if !usage.Full() {
		return
	}
	free, known := freeBytesOn(appDataRoot())
	floor := int64(companySettingPositiveInt("APP_DATA_HOST_FLOOR_MB", appDataHostFloorMBDefault)) << 20
	if !known || free >= floor {
		log.Warn("company: %s/%s is at its data limit (%d MB); new deploys are refused",
			owner, repo, usage.QuotaBytes>>20)
		return
	}

	// Over its own limit *and* the volume is nearly gone. Stopping the app
	// that overran is the only move that does not punish a department for
	// somebody else's data.
	log.Error("company: %s/%s is over its data limit and the volume has %d MB left; stopping it",
		owner, repo, free>>20)
	if err := supervisorFor(owner, repo).Stop("platform", AppStateFailed, ReasonDataFull); err != nil {
		log.Error("company: stopping %s/%s after the disk filled: %v", owner, repo, err)
	}
	_ = MutateAppState(owner, repo, func(st *AppState) bool {
		st.FailedAt = time.Now().Unix()
		st.Reason = ReasonDataFull
		st.Message = "the app was stopped: it is over its " + strconv.FormatInt(usage.QuotaBytes>>20, 10) +
			" MB data limit and the server is running out of disk"
		st.UserMessageKey = "company.app.data_full"
		return true
	})
}

// RefuseDeployIfDataFull blocks a deploy that has nowhere to put a snapshot.
//
// Checked before the build rather than after: a migration takes a copy of
// the database first, so starting a deploy with no room for one would fail
// somewhere deep in the runner instead of here, where the reason is plain.
func RefuseDeployIfDataFull(ctx context.Context, owner, repo string) error {
	usage, ok := AppDataUsageFor(ctx, owner, repo)
	if !ok || !usage.Full() {
		return nil
	}
	return audienceKeyError("company.err.data_full", "company.err.data_full.admin",
		usage.Bytes>>20, usage.QuotaBytes>>20)
}

const (
	appDataSnapshotDir         = ".snapshots"
	appDataSnapshotKeepDefault = 10
	appDataRollbackDaysDefault = 30
)

// appRunForProcess is the run directory as the app sees it — the same
// two-names-for-one-directory split as the data directory above.
func appRunForProcess(p appPaths) string {
	if mode, _ := sandboxMode(); mode == SandboxBubblewrap {
		return "/run"
	}
	return p.run
}
