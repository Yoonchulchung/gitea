// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An empty file must still produce a safe app: no network, no downloads,
// no packages. Every loosening should be an explicit line in git.
func TestBuiltinDefaultsAreSafe(t *testing.T) {
	cfg, err := ParseAppsConfig([]byte(""))
	require.NoError(t, err)

	s := cfg.EffectiveSettings("PO", "app")
	assert.Equal(t, NetworkNone, s.Network.Mode)
	assert.Equal(t, "block", s.Download.Policy)
	assert.Empty(t, s.AllowedPackages(), "nothing is installable until an admin approves it")
	assert.True(t, s.IsEnabled(), "an app nobody has written a line for still deploys")
	assert.Positive(t, s.Limits.MemoryMB)
	assert.Positive(t, s.Limits.TmpMB, "an unbounded tmpfs would eat host RAM")
}

func TestEffectiveSettingsMerge(t *testing.T) {
	cfg, err := ParseAppsConfig([]byte(`
version: 1
defaults:
  access: login
  limits: {memoryMB: 512}
  dependencies:
    allow: [fastapi, uvicorn]
apps:
  HR/payroll:
    access: org
    limits: {memoryMB: 1024}
    network: {mode: broker, allow: [{host: erp.internal, methods: [GET]}]}
    dependencies:
      allowExtra: [openpyxl]
  PO/readme: {}
  LEGACY/tool:
    enabled: false
`))
	require.NoError(t, err)

	hr := cfg.EffectiveSettings("HR", "payroll")
	assert.Equal(t, AccessOrg, hr.Access, "per-app wins over defaults")
	assert.Equal(t, 1024, hr.Limits.MemoryMB)
	assert.Equal(t, 64, hr.Limits.Processes, "unset fields keep the builtin, not zero")
	assert.Equal(t, NetworkBroker, hr.Network.Mode)
	require.Len(t, hr.Network.Allow, 1)
	assert.Equal(t, "erp.internal", hr.Network.Allow[0].Host)
	// allowExtra accumulates onto the shared base rather than replacing it —
	// otherwise every per-app entry has to restate the common packages, and
	// the day someone forgets, the app loses packages it already had.
	assert.ElementsMatch(t, []string{"fastapi", "uvicorn", "openpyxl"}, hr.AllowedPackages())

	po := cfg.EffectiveSettings("PO", "readme")
	assert.Equal(t, AccessLogin, po.Access, "an empty entry inherits defaults")
	assert.ElementsMatch(t, []string{"fastapi", "uvicorn"}, po.AllowedPackages())

	assert.False(t, cfg.EffectiveSettings("LEGACY", "tool").IsEnabled())
	assert.Equal(t, AccessLogin, cfg.EffectiveSettings("NEW", "repo").Access,
		"an app with no entry at all still gets the defaults")
}

// A typo in an enum must fail loudly. Falling through to a default would
// quietly grant something nobody chose — the worst possible outcome for a
// value like `access`.
func TestParseAppsConfigRejectsBadValues(t *testing.T) {
	for name, yml := range map[string]string{
		"bad access":          "apps:\n  PO/app:\n    access: publik\n",
		"bad network mode":    "apps:\n  PO/app:\n    network: {mode: sometimes}\n",
		"bad download":        "apps:\n  PO/app:\n    download: {policy: maybe}\n",
		"bad defaults":        "defaults:\n  access: everyone\n",
		"key not owner/repo":  "apps:\n  justrepo: {}\n",
		"unsupported version": "version: 99\n",
		"not yaml":            "apps:\n  - [unclosed\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseAppsConfig([]byte(yml))
			assert.Error(t, err)
		})
	}
}

// If an admin commits a broken apps.yml, apps already running must keep the
// policy they had. Falling back to builtin defaults would silently change
// access and network policy across every app at the moment nobody is
// looking at the file.
func TestBrokenConfigKeepsLastGood(t *testing.T) {
	good, err := ParseAppsConfig([]byte("defaults:\n  access: org\n"))
	require.NoError(t, err)
	SetAppsConfig(good)
	t.Cleanup(func() { SetAppsConfig(&AppsConfig{}) })

	assert.Equal(t, AccessOrg, SettingsFor("PO", "app").Access)

	SetAppsConfigError(errors.New("boom"))

	assert.Equal(t, AccessOrg, SettingsFor("PO", "app").Access, "policy must not change")
	_, _, loadErr := AppsConfigSnapshot()
	assert.Error(t, loadErr, "but the failure is visible to admins")
}

// The default is a shared host's budget, not one app's comfort: dozens of
// departments deploy here and there is no memory quota underneath. A
// generous default multiplied by thirty apps oversubscribes the machine, and
// then the kernel chooses which one dies rather than the platform.
func TestDefaultMemoryFitsManyAppsOnOneHost(t *testing.T) {
	cfg, err := ParseAppsConfig([]byte(""))
	require.NoError(t, err)
	limit := cfg.EffectiveSettings("PO", "app").Limits.MemoryMB

	// Comfortably above what a plain FastAPI app resides at (~90 MB), and
	// low enough that thirty of them are not a promise the host cannot keep.
	assert.GreaterOrEqual(t, limit, 128, "too tight for CPython + pydantic's compiled core")
	assert.LessOrEqual(t, limit*30, 8*1024, "thirty apps must fit in a modest host")
}

// The limit is a heap limit but the watchdog can only see resident memory,
// which also counts the interpreter and its shared libraries. Comparing them
// directly would make the watchdog fire before the limit it is enforcing.
func TestWatchdogAllowsForResidentOverhead(t *testing.T) {
	assert.Greater(t, memoryWatchdogHeadroom, 1.0,
		"RSS legitimately exceeds the heap by the size of the mapped runtime")
}
