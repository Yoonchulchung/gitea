// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// Per-department-app deployment state, one flat JSON file per app.
//
// Deliberately not a database table: modelmigration/migrations.go is a
// sequentially numbered append-only list, so a fork adding its own
// migration collides with upstream's next one both textually (adjacent-line
// conflict on nearly every upstream merge) and semantically (two different
// schema changes claiming one version number, on databases that already ran
// one of them). The data here is one small record per department, never
// queried by anything but "get one" and "list all" — a file is enough. Same
// reasoning and same on-disk conventions as company/deploy_snapshot.go.
//
// See docs/company/app-platform-impl.md §1-2.

// App lifecycle states. `desired` only ever holds Running or Stopped — it
// is what a human asked for; the rest describe what is actually true.
const (
	// desired + actual
	AppStateRunning = "running"
	AppStateStopped = "stopped"

	// actual only
	AppStateQueued     = "queued"     // merged, waiting for a deploy worker
	AppStateBuilding   = "building"   // venv / pip install
	AppStateActivating = "activating" // swapping current, starting, health-checking
	AppStateFailed     = "failed"     // not running and not intentionally stopped
	AppStateSuspended  = "suspended"  // an admin stopped it; the department cannot restart it
)

// Failure reason codes. Every one maps to a plain-language sentence for the
// department (see docs/company/app-platform-impl.md §4) — staff never see a
// raw log, so an unclassified failure is a dead end for them.
const (
	ReasonInstallFailed      = "install_failed"
	ReasonPackageDenied      = "package_denied"
	ReasonOOM                = "oom"
	ReasonHealthTimeout      = "health_timeout"
	ReasonCrashLoop          = "crash_loop"
	ReasonSuspended          = "suspended"
	ReasonSandboxUnavailable = "sandbox_unavailable"
	ReasonSecretError        = "secret_error"
	ReasonDeployQueueFull    = "deploy_queue_full"
	ReasonContractViolation  = "contract_violation" // no main.py
	ReasonRolledBack         = "rolled_back"
	ReasonNoRelease          = "no_release" // start pressed before any deploy succeeded
)

// appHistoryLimit bounds the per-app history. It doubles as the rollback
// menu, so it has to be long enough to reach past a bad run of deploys but
// short enough that the file stays small and cheap to rewrite in full.
const appHistoryLimit = 10

// AppHealth is the last health-check result. `CheckedAt` matters as much as
// `State`: without it a stale "up" from an hour ago is indistinguishable
// from a fresh one, and the admin page would quietly lie.
type AppHealth struct {
	State     string `json:"state"` // "up" | "down" | "unknown"
	CheckedAt int64  `json:"checkedAt"`
	Detail    string `json:"detail,omitempty"` // admin-only, e.g. "HTTP 200"
}

// AppHistoryEntry is one past deploy or control action.
type AppHistoryEntry struct {
	SHA    string `json:"sha,omitempty"`
	Status string `json:"status"`
	At     int64  `json:"at"`
	Actor  string `json:"actor,omitempty"`  // who did it, for "why is this off?"
	Reason string `json:"reason,omitempty"` // reason code or admin's free text
}

// AppState is the whole record. Owner/Repo live inside the file rather than
// in its name so "list every app" is just "read every file in the dir" —
// the filename is a hash and can't be reversed.
type AppState struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`

	// Desired is the human's intent; Actual is reality. Keeping them apart
	// is what stops a Gitea restart from resurrecting an app a department
	// deliberately switched off.
	Desired string `json:"desired"`
	Actual  string `json:"actual"`

	SHA       string `json:"sha,omitempty"` // central-deploy commit currently live
	PRID      int64  `json:"prID,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Sandboxed bool   `json:"sandboxed"`

	StartedAt int64 `json:"startedAt,omitempty"`
	UpdatedAt int64 `json:"updatedAt"`

	Health AppHealth `json:"health"`

	// Reason is a code from the list above.
	//
	// Message is admin-only. It carries whatever actually went wrong — a
	// filesystem error with an absolute path, a build log — and must never
	// reach a department or the staff-facing badge endpoint.
	//
	// UserMessage is the half that may. It is only ever set from text
	// deliberately written for a non-developer (see company/usererror.go), so
	// that showing it is safe by construction rather than by remembering.
	// Empty is fine: DepartmentCause has a sentence for every reason code.
	Reason      string `json:"reason,omitempty"`
	Message     string `json:"message,omitempty"`
	UserMessage string `json:"userMessage,omitempty"`

	// EnvVersion is bumped whenever environment variables are saved.
	// EnvVersionRunning is what the live process was started with. When they
	// differ the UI shows "restart required" — a process's environment
	// cannot be changed in place, and without this the person who just
	// saved a value would be debugging why it had no effect.
	EnvVersion        int64 `json:"envVersion,omitempty"`
	EnvVersionRunning int64 `json:"envVersionRunning,omitempty"`

	History []AppHistoryEntry `json:"history,omitempty"`
}

// appKey is the on-disk identity of one app. Hashed rather than
// "<owner>-<repo>" because "-" and "." are both legal in owner and repo
// names (modules/validation/helpers.go), so any flat join genuinely
// collides — "a-b/c" and "a/b-c" would land on the same file. Same reason
// company/workspace_tmp.go hashes its own keys.
func appKey(owner, repo string) string {
	sum := sha256.Sum256([]byte(owner + "/" + repo))
	return hex.EncodeToString(sum[:])
}

func appStateDir() string {
	return filepath.Join(setting.AppDataPath, "company-app-state")
}

func appStateFile(owner, repo string) string {
	return filepath.Join(appStateDir(), appKey(owner, repo)+".json")
}

// Per-file locks, same shape as company/workspace_tmp.go's: only the map
// access is globally locked, and callers hold the per-file mutex across
// their whole read-modify-write so two concurrent updates can't lose one
// another's change. Entries are never evicted — bounded by the number of
// department apps this instance has ever seen.
var (
	appStateLocksMu sync.Mutex
	appStateLocks   = map[string]*sync.Mutex{}
)

func appStateLockFor(file string) *sync.Mutex {
	appStateLocksMu.Lock()
	defer appStateLocksMu.Unlock()
	l, ok := appStateLocks[file]
	if !ok {
		l = &sync.Mutex{}
		appStateLocks[file] = l
	}
	return l
}

// readAppStateFile parses one state file. A corrupt or unreadable file is
// reported as "not found" rather than an error: state is observability, and
// losing it must never take an app down (fail-open, see
// docs/company/app-platform-impl.md §4). The corruption is logged so it
// doesn't pass silently.
func readAppStateFile(file string) (*AppState, bool) {
	body, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	var st AppState
	if err := json.Unmarshal(body, &st); err != nil {
		log.Error("company: app state %s is corrupt, treating as absent: %v", file, err)
		return nil, false
	}
	return &st, true
}

// writeAppStateFileLocked writes st atomically. Callers must already hold
// the per-file lock.
func writeAppStateFileLocked(file string, st *AppState) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	// Atomic on the same filesystem — a reader never sees a half-written
	// file, which matters because the admin dashboard reads these on every
	// page load while deploys are writing them.
	return os.Rename(tmp, file)
}

// writeFileAtomic writes body with the same temp-file-and-rename dance the
// state files use, so a reader never sees a half-written file.
func writeFileAtomic(file string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// readFileIfExists returns nil, nil when the file is simply absent — the
// normal case for a request nobody has made yet.
func readFileIfExists(file string) ([]byte, error) {
	body, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return body, err
}

// LoadAppState returns the app's state, or a zero-value default if it has
// none yet (never deployed, or the file was lost/corrupt). Never returns an
// error for a missing file: "no state" is a normal, expected answer.
func LoadAppState(owner, repo string) *AppState {
	file := appStateFile(owner, repo)
	lock := appStateLockFor(file)
	lock.Lock()
	defer lock.Unlock()

	if st, ok := readAppStateFile(file); ok {
		return st
	}
	return &AppState{Owner: owner, Repo: repo, Desired: AppStateStopped, Actual: AppStateStopped}
}

// MutateAppState runs mutate against the app's current state under its lock
// and persists the result. This is the only supported way to change state:
// read-modify-write done by callers separately would drop concurrent
// updates (a deploy finishing while an admin clicks stop, say).
//
// mutate may return false to abort without writing — used to reject a
// transition (see CanTransition) without leaving a partial change behind.
func MutateAppState(owner, repo string, mutate func(*AppState) bool) error {
	file := appStateFile(owner, repo)
	lock := appStateLockFor(file)
	lock.Lock()
	defer lock.Unlock()

	st, ok := readAppStateFile(file)
	if !ok {
		st = &AppState{Owner: owner, Repo: repo, Desired: AppStateStopped, Actual: AppStateStopped}
	}
	st.Owner, st.Repo = owner, repo // heal a file whose identity fields were lost
	if !mutate(st) {
		return nil
	}
	st.UpdatedAt = time.Now().Unix()
	if err := writeAppStateFileLocked(file, st); err != nil {
		return err
	}
	// An app becomes routable at the same moment it becomes real, so the
	// proxy never has to consult the disk to find out (appregistry.go).
	RegisterApp(owner, repo)
	return nil
}

// AppendHistory records one event, newest first, capped at appHistoryLimit.
// Call from inside a MutateAppState callback.
func (st *AppState) AppendHistory(entry AppHistoryEntry) {
	if entry.At == 0 {
		entry.At = time.Now().Unix()
	}
	st.History = append([]AppHistoryEntry{entry}, st.History...)
	if len(st.History) > appHistoryLimit {
		st.History = st.History[:appHistoryLimit]
	}
}

// RollbackTarget returns the newest previously-deployed SHA that isn't the
// one live now, or "" if there's nothing to roll back to.
func (st *AppState) RollbackTarget() string {
	for _, h := range st.History {
		if h.Status == AppStateRunning && h.SHA != "" && h.SHA != st.SHA {
			return h.SHA
		}
	}
	return ""
}

// IsBusy reports whether a deploy is mid-flight. Control actions check this
// so a restart can't land between "current swapped" and "health checked".
func (st *AppState) IsBusy() bool {
	switch st.Actual {
	case AppStateQueued, AppStateBuilding, AppStateActivating:
		return true
	}
	return false
}

// ListAppStates returns every app that has state on disk, sorted by
// owner/repo so the admin list doesn't reshuffle between page loads.
// Unreadable files are skipped, not fatal — one corrupt record must not
// blank the whole dashboard.
func ListAppStates() []*AppState {
	entries, err := os.ReadDir(appStateDir())
	if err != nil {
		return nil // no apps deployed yet
	}
	out := make([]*AppState, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		file := filepath.Join(appStateDir(), e.Name())
		lock := appStateLockFor(file)
		lock.Lock()
		st, ok := readAppStateFile(file)
		lock.Unlock()
		if ok {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}

// CanTransition reports whether `action` is allowed from the app's current
// state, and if not, why — the message is shown to whoever clicked.
//
// The rule that matters: `suspended` is an admin's protective stop, so the
// department cannot undo it. Without that distinction a department could
// simply restart an app the admin just stopped for eating the machine, and
// the admin's only remaining option would be to take away their controls
// entirely. See docs/company/app-platform.md.
func (st *AppState) CanTransition(action string, isAdmin bool) (bool, string) {
	if st.IsBusy() && action != "suspend" {
		return false, "a deploy is in progress — try again once it finishes"
	}
	switch action {
	case "start", "restart", "rollback", "redeploy":
		if st.Actual == AppStateSuspended {
			if !isAdmin {
				return false, "an administrator stopped this app; ask them to resume it"
			}
			return false, "resume the app before starting it"
		}
		return true, ""
	case "stop":
		if st.Actual == AppStateSuspended {
			return false, "the app is already stopped by an administrator"
		}
		return true, ""
	case "suspend", "resume", "remove":
		if !isAdmin {
			return false, "only an administrator can do this"
		}
		if action == "resume" && st.Actual != AppStateSuspended {
			return false, "the app is not suspended"
		}
		return true, ""
	}
	return false, fmt.Sprintf("unknown action %q", action)
}
