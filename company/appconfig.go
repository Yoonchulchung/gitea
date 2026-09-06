// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/log"

	"go.yaml.in/yaml/v4"
)

// The platform's policy lives in `.company/apps.yml` at the root of the
// central deploy repo, owned by admins.
//
// Staff cannot write it, and that is structural rather than a convention:
// every path a snapshot produces starts with "<owner>/" (see
// snapshotFilesUnderPrefix), and owner names must begin with an
// alphanumeric (modules/validation/helpers.go), so no organization can ever
// be named ".company". A root-level dotfile is unreachable by any deploy.
//
// Admins never hand-edit this file either — the approval buttons commit it
// for them (docs/company/app-platform.md), so YAML syntax is not something
// a non-developer operator has to learn.

// AppNetworkMode controls egress. Default is none: most internal apps need
// no outbound access at all, and the ones that do should be a visible,
// approved exception rather than the default posture.
const (
	NetworkNone   = "none"   // no network namespace access whatsoever
	NetworkBroker = "broker" // only via the platform broker's allowlist
	NetworkOpen   = "open"   // unrestricted — an explicit admin exception
)

// App access modes. Deployed apps are independent of Gitea auth by default;
// a department opts in to requiring it.
const (
	AccessPublic = "public" // anyone who can reach the server
	AccessLogin  = "login"  // any signed-in Gitea user
	AccessOrg    = "org"    // members of the owning organization (admins too)
)

// AppLimits are enforced with rlimits, not cgroups — cgroups need root and
// this platform has none. See docs/company/app-platform-impl.md §6.
type AppLimits struct {
	// MemoryMB caps the heap (RLIMIT_DATA). Sized for what a plain FastAPI
	// app actually uses — interpreter, uvicorn, pydantic and starlette come
	// to roughly 90 MB resident — rather than for the heaviest app anyone
	// might ever write.
	//
	// The default matters more than it looks: this is a shared host with no
	// disk or memory quota of its own, and dozens of departments. A generous
	// default multiplied by thirty apps oversubscribes the machine, and then
	// the kernel picks which one dies rather than the platform. Raising it is
	// a per-app decision an admin makes from the recorded peak, which is why
	// the dashboard shows peak next to limit.
	MemoryMB  int `yaml:"memoryMB"`
	Processes int `yaml:"processes"`
	OpenFiles int `yaml:"openFiles"`
	TmpMB     int `yaml:"tmpMB"` // tmpfs size; unbounded /tmp would eat RAM
}

// AppNetwork is the egress policy. AllowHosts only applies to broker mode.
type AppNetwork struct {
	Mode  string           `yaml:"mode"`
	Allow []AppNetworkRule `yaml:"allow"`
}

type AppNetworkRule struct {
	Host    string   `yaml:"host"`
	Methods []string `yaml:"methods"`
	Paths   []string `yaml:"paths"`
}

// AppDownload controls whether an app may hand files to a browser. Blocked
// by default; see docs/company/app-platform.md on what this does and does
// not stop.
type AppDownload struct {
	Policy           string `yaml:"policy"` // "block" | "allow"
	MaxResponseBytes int64  `yaml:"maxResponseBytes"`
}

// AppDependencies is the package allowlist. Allow is the shared base;
// AllowExtra is per-app so approving pandas for one department does not
// silently open it everywhere (least privilege).
type AppDependencies struct {
	Allow      []string `yaml:"allow"`
	AllowExtra []string `yaml:"allowExtra"`
}

// AppSettings is one app's effective policy, after defaults are merged in.
type AppSettings struct {
	Enabled *bool `yaml:"enabled"`
	// BasePackages are installed into every app's environment whether or not
	// its requirements.txt asks for them.
	//
	// A department writing a FastAPI app should not have to know that
	// pydantic exists, let alone which version of it has wheels for the
	// host's interpreter — that is how "pydantic==2.5.0" ends up in a
	// requirements.txt and fails on a Python nobody told them about. The
	// platform provides the stack it advertises; the department's file is
	// for what they need *on top* of it.
	BasePackages []string        `yaml:"basePackages"`
	Access       string          `yaml:"access"`
	Install      []string        `yaml:"install"`
	Start        string          `yaml:"start"`
	HealthPath   string          `yaml:"healthPath"`
	Limits       AppLimits       `yaml:"limits"`
	Network      AppNetwork      `yaml:"network"`
	Download     AppDownload     `yaml:"download"`
	Dependencies AppDependencies `yaml:"dependencies"`
}

// AppsConfig is the whole file.
type AppsConfig struct {
	Version  int                     `yaml:"version"`
	Defaults AppSettings             `yaml:"defaults"`
	Apps     map[string]*AppSettings `yaml:"apps"`
}

// builtinDefaults is what applies before the file says anything — chosen so
// that a completely empty apps.yml still yields a safe app: no network, no
// downloads, no packages, modest limits. Every loosening is then an
// explicit, reviewable line in git.
func builtinDefaults() AppSettings {
	return AppSettings{
		Access: AccessPublic, // deployed apps are independent of Gitea auth by default
		// Deliberately unpinned. These have to install against whatever
		// interpreter the host has, and a pin whose wheels predate it breaks
		// every app at once. An admin who wants reproducibility can pin them
		// on the admin page, knowingly.
		BasePackages: []string{"fastapi", "uvicorn", "pydantic"},
		Install:      []string{"pip install --only-binary=:all: -r requirements.txt"},
		Start:        "uvicorn main:app --uds ${SOCKET} --root-path ${ROOT_PATH}",
		HealthPath:   "/health",
		Limits:       AppLimits{MemoryMB: 192, Processes: 64, OpenFiles: 4096, TmpMB: 64},
		Network:      AppNetwork{Mode: NetworkNone},
		Download:     AppDownload{Policy: "block", MaxResponseBytes: 5 << 20},
	}
}

// EffectiveSettings merges builtin defaults, the file's `defaults`, and the
// per-app entry. Only fields the narrower scope actually set win; a zero
// value means "not specified", never "set it to zero".
func (c *AppsConfig) EffectiveSettings(owner, repo string) AppSettings {
	out := builtinDefaults()
	apply := func(s *AppSettings) {
		if s == nil {
			return
		}
		if s.Enabled != nil {
			out.Enabled = s.Enabled
		}
		if s.Access != "" {
			out.Access = s.Access
		}
		if len(s.Install) > 0 {
			out.Install = s.Install
		}
		// Replaced rather than accumulated: this is the platform's own stack,
		// and an admin narrowing it for one app means exactly that.
		if len(s.BasePackages) > 0 {
			out.BasePackages = s.BasePackages
		}
		if s.Start != "" {
			out.Start = s.Start
		}
		if s.HealthPath != "" {
			out.HealthPath = s.HealthPath
		}
		if s.Limits.MemoryMB > 0 {
			out.Limits.MemoryMB = s.Limits.MemoryMB
		}
		if s.Limits.Processes > 0 {
			out.Limits.Processes = s.Limits.Processes
		}
		if s.Limits.OpenFiles > 0 {
			out.Limits.OpenFiles = s.Limits.OpenFiles
		}
		if s.Limits.TmpMB > 0 {
			out.Limits.TmpMB = s.Limits.TmpMB
		}
		if s.Network.Mode != "" {
			out.Network = s.Network
		}
		if s.Download.Policy != "" {
			out.Download.Policy = s.Download.Policy
		}
		if s.Download.MaxResponseBytes > 0 {
			out.Download.MaxResponseBytes = s.Download.MaxResponseBytes
		}
		// Package allowlists accumulate rather than replace: `allow` is the
		// shared base and `allowExtra` is what this department additionally
		// needed. Replacing would mean every per-app entry has to restate
		// the common set, and the day someone forgets, their app silently
		// loses packages it was already approved for.
		out.Dependencies.Allow = append(out.Dependencies.Allow, s.Dependencies.Allow...)
		out.Dependencies.AllowExtra = append(out.Dependencies.AllowExtra, s.Dependencies.AllowExtra...)
	}
	apply(&c.Defaults)
	apply(c.Apps[owner+"/"+repo])
	return out
}

// AllowedPackages is the flattened allowlist for one app.
//
// Base packages are always on it: the platform chose them, so requiring an
// admin to approve them again would be approving their own decision.
func (s AppSettings) AllowedPackages() []string {
	all := append([]string{}, s.Dependencies.Allow...)
	all = append(all, s.Dependencies.AllowExtra...)
	all = append(all, BasePackageNames(s.BasePackages)...)

	// Deduplicated because the lists genuinely overlap: a package approved
	// for a department before it joined the base set is in both, and the
	// sidebar was showing "fastapi, uvicorn, pydantic, fastapi, uvicorn,
	// pydantic" to the people this panel exists for.
	seen := make(map[string]bool, len(all))
	out := make([]string, 0, len(all))
	for _, name := range all {
		key := normalizePackageName(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// IsEnabled reports whether the app should be deployed at all. Unset means
// enabled — an admin has to opt an app *out*, so a new department isn't
// silently ignored because nobody added a line for it.
func (s AppSettings) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// ParseAppsConfig parses and validates apps.yml. Validation is strict about
// enum values because a typo like `access: publik` would otherwise fall
// through to a default and quietly grant something nobody chose.
func ParseAppsConfig(data []byte) (*AppsConfig, error) {
	var cfg AppsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("apps.yml is not valid YAML: %w", err)
	}
	if cfg.Version != 0 && cfg.Version != 1 {
		return nil, fmt.Errorf("apps.yml version %d is not supported", cfg.Version)
	}

	check := func(what string, s *AppSettings) error {
		if s == nil {
			return nil
		}
		switch s.Access {
		case "", AccessPublic, AccessLogin, AccessOrg:
		default:
			return fmt.Errorf("%s: access %q must be one of public, login, org", what, s.Access)
		}
		switch s.Network.Mode {
		case "", NetworkNone, NetworkBroker, NetworkOpen:
		default:
			return fmt.Errorf("%s: network.mode %q must be one of none, broker, open", what, s.Network.Mode)
		}
		switch s.Download.Policy {
		case "", "block", "allow":
		default:
			return fmt.Errorf("%s: download.policy %q must be block or allow", what, s.Download.Policy)
		}
		return nil
	}
	if err := check("defaults", &cfg.Defaults); err != nil {
		return nil, err
	}
	for key, s := range cfg.Apps {
		if _, _, ok := strings.Cut(key, "/"); !ok {
			return nil, fmt.Errorf("apps: %q must be written as \"owner/repo\"", key)
		}
		if err := check("apps."+key, s); err != nil {
			return nil, err
		}
	}
	return &cfg, nil
}

// appsConfigCache holds the last configuration that parsed cleanly.
//
// The hot path (the reverse proxy, on every request to every app) reads
// policy from here, so it must never touch the filesystem — see
// docs/company/app-platform-impl.md §3.
//
// Keeping the last *good* value is the other half of its job: if an admin
// commits a broken apps.yml, apps that are already running must keep
// running under the policy they had. Falling back to built-in defaults
// instead would silently change access and network policy across every app
// at the exact moment nobody is looking at the file.
var appsConfigCache = struct {
	mu       sync.RWMutex
	cfg      *AppsConfig
	loadedAt time.Time
	err      error // last load failure, surfaced to admins
}{cfg: &AppsConfig{}}

// SetAppsConfig installs a freshly parsed configuration.
func SetAppsConfig(cfg *AppsConfig) {
	appsConfigCache.mu.Lock()
	defer appsConfigCache.mu.Unlock()
	appsConfigCache.cfg = cfg
	appsConfigCache.loadedAt = time.Now()
	appsConfigCache.err = nil
}

// SetAppsConfigError records a load failure without disturbing the config
// currently in force.
func SetAppsConfigError(err error) {
	appsConfigCache.mu.Lock()
	defer appsConfigCache.mu.Unlock()
	appsConfigCache.err = err
	log.Error("company: apps.yml not applied, keeping the previous configuration: %v", err)
}

// AppsConfigSnapshot returns the configuration in force plus the last load
// error, if any. The error is shown on the admin page so a broken commit is
// visible rather than merely inert.
func AppsConfigSnapshot() (*AppsConfig, time.Time, error) {
	appsConfigCache.mu.RLock()
	defer appsConfigCache.mu.RUnlock()
	return appsConfigCache.cfg, appsConfigCache.loadedAt, appsConfigCache.err
}

// SettingsFor is the hot-path lookup: effective policy for one app, no I/O.
func SettingsFor(owner, repo string) AppSettings {
	appsConfigCache.mu.RLock()
	cfg := appsConfigCache.cfg
	appsConfigCache.mu.RUnlock()
	return cfg.EffectiveSettings(owner, repo)
}
