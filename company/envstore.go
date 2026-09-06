// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gitea.dev/modules/json"
	secret_module "gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
)

// Department apps need configuration — database URLs, API keys — and it must
// not live in the repository, where every deploy request would put it in
// front of an admin and in git history forever.
//
// So it is entered in Gitea, encrypted at rest with the instance SECRET_KEY
// (the same primitive Actions secrets use, see company/secretstore.go), and
// injected straight into the process environment at start. No .env file is
// ever written to the deploy server: the plaintext exists only in the
// child's memory.
//
// Values are never shown again after saving, and never appear in logs, in
// argv (see the --setenv note in appsandbox.go), or in any admin screen.
// What this does NOT protect against is the person who typed the value in —
// see docs/company/app-platform.md.

const (
	maxEnvVars         = 100
	maxEnvValueSize    = 4 << 10
	appEnvHistoryLimit = 20
)

// envNamePattern is the POSIX shell variable-name shape. Anything else
// either cannot be exported at all or, worse, can be smuggled through the
// environment block as something the shell will re-interpret.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// AppEnvChange records that a variable changed, without recording what it
// changed to. The values are the secret; the fact that a secret rotated at
// 14:03 and who did it is exactly what an incident investigation needs.
type AppEnvChange struct {
	At    int64    `json:"at"`
	Actor string   `json:"actor"`
	Set   []string `json:"set,omitempty"`
	Unset []string `json:"unset,omitempty"`
}

// appEnvFile is the on-disk shape. Values are ciphertext.
type appEnvFile struct {
	// Version increments on every change. The supervisor records which
	// version a running process was started with, so the UI can tell a
	// department "you saved changes, restart to apply them" instead of
	// letting them wonder why the new value isn't taking effect — a
	// process's environment cannot be changed while it runs.
	Version int64             `json:"version"`
	Vars    map[string]string `json:"vars"`
	History []AppEnvChange    `json:"history,omitempty"`
}

func appEnvFilePath(owner, repo string) string {
	return filepath.Join(setting.AppDataPath, "company-env", appKey(owner, repo)+".json")
}

// readAppEnvFile returns the stored file, or an empty one if there is none.
//
// Unlike app state, a corrupt file is an error rather than "treat as
// absent": silently continuing with no variables would start the app with a
// missing database password and produce a failure nobody can trace back to
// here. Secrets are fail-closed (docs/company/app-platform-impl.md §4).
func readAppEnvFile(path string) (*appEnvFile, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &appEnvFile{Vars: map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var f appEnvFile
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("the stored environment variables are unreadable: %w", err)
	}
	if f.Vars == nil {
		f.Vars = map[string]string{}
	}
	return &f, nil
}

func writeAppEnvFileLocked(path string, f *appEnvFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadAppEnv decrypts every variable for one app and returns them with the
// current version.
//
// Fail-closed on a decryption failure: an app that starts with an empty
// value where a credential should be will fail somewhere far from the
// cause. Unlike company/secretstore.go there is no plaintext-migration
// fallback here, because nothing ever wrote plaintext to this file.
func LoadAppEnv(owner, repo string) (map[string]string, int64, error) {
	path := appEnvFilePath(owner, repo)
	lock := appStateLockFor(path)
	lock.Lock()
	defer lock.Unlock()

	f, err := readAppEnvFile(path)
	if err != nil {
		return nil, 0, err
	}
	out := make(map[string]string, len(f.Vars))
	for name, encrypted := range f.Vars {
		value, err := secret_module.DecryptSecret(setting.SecretKey, encrypted)
		if err != nil {
			// The name is safe to report; it is what the department needs in
			// order to re-enter the value. The ciphertext is not.
			return nil, 0, userKeyError("company.err.env_decrypt", name)
		}
		out[name] = value
	}
	return out, f.Version, nil
}

// AppEnvNames lists the variable names an app has, with the stored version.
// Names only — the values are never returned to any screen.
func AppEnvNames(owner, repo string) ([]string, int64, error) {
	path := appEnvFilePath(owner, repo)
	lock := appStateLockFor(path)
	lock.Lock()
	defer lock.Unlock()

	f, err := readAppEnvFile(path)
	if err != nil {
		return nil, 0, err
	}
	names := make([]string, 0, len(f.Vars))
	for name := range f.Vars {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, f.Version, nil
}

// AppEnvHistory returns who changed which variables and when, newest first.
func AppEnvHistory(owner, repo string) []AppEnvChange {
	path := appEnvFilePath(owner, repo)
	lock := appStateLockFor(path)
	lock.Lock()
	defer lock.Unlock()

	f, err := readAppEnvFile(path)
	if err != nil {
		return nil
	}
	return f.History
}

// ValidateEnvName rejects names that would break the app or hand it a way
// out of the sandbox.
func ValidateEnvName(name string) error {
	if name == "" {
		return userKeyError("company.err.env_name_empty")
	}
	if len(name) > 128 {
		return userKeyError("company.err.env_name_long", name[:32])
	}
	if !envNamePattern.MatchString(name) {
		return userKeyError("company.err.env_name_invalid", name)
	}
	if isReservedEnvName(name) {
		// LD_PRELOAD is the one that matters: it injects an arbitrary shared
		// library into the process, which is code execution inside the
		// sandbox from a text field. PATH/HOME/PYTHON* are rejected because
		// overriding them breaks the app in ways that look like a platform
		// bug.
		return userKeyError("company.err.env_name_reserved", strings.ToUpper(name))
	}
	return nil
}

// SaveAppEnv applies a set of changes as one atomic edit.
//
// set overwrites or adds; unset removes. A name present in both is removed —
// callers build these from a form where a cleared field means "delete", and
// resolving it here keeps that from depending on map iteration order.
//
// Every name is validated before anything is written, so a form with one bad
// entry changes nothing rather than applying half of itself.
func SaveAppEnv(owner, repo, actor string, set map[string]string, unset []string) error {
	for name, value := range set {
		if err := ValidateEnvName(name); err != nil {
			return err
		}
		if len(value) > maxEnvValueSize {
			return userKeyError("company.err.env_value_large", name, maxEnvValueSize)
		}
		if strings.ContainsRune(value, 0) {
			// A NUL terminates the string in the environment block, so the
			// process would silently receive a truncated credential.
			return userKeyError("company.err.env_value_nul", name)
		}
	}

	path := appEnvFilePath(owner, repo)
	lock := appStateLockFor(path)
	lock.Lock()
	defer lock.Unlock()

	f, err := readAppEnvFile(path)
	if err != nil {
		return err
	}
	for _, name := range unset {
		delete(f.Vars, name)
	}
	for name, value := range set {
		if slices.Contains(unset, name) {
			continue
		}
		encrypted, err := secret_module.EncryptSecret(setting.SecretKey, value)
		if err != nil {
			return err
		}
		f.Vars[name] = encrypted
	}
	if len(f.Vars) > maxEnvVars {
		return userKeyError("company.err.env_too_many", maxEnvVars)
	}

	f.Version++
	change := AppEnvChange{At: time.Now().Unix(), Actor: actor, Unset: unset}
	for name := range set {
		if !slices.Contains(unset, name) {
			change.Set = append(change.Set, name)
		}
	}
	slices.Sort(change.Set)
	f.History = append([]AppEnvChange{change}, f.History...)
	if len(f.History) > appEnvHistoryLimit {
		f.History = f.History[:appEnvHistoryLimit]
	}
	return writeAppEnvFileLocked(path, f)
}

// DeleteAppEnv removes an app's variables entirely, for when an app is
// removed from the platform.
func DeleteAppEnv(owner, repo string) error {
	path := appEnvFilePath(owner, repo)
	lock := appStateLockFor(path)
	lock.Lock()
	defer lock.Unlock()

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
