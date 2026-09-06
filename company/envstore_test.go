// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"strings"
	"testing"

	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSecretKey gives the encryption a key to work with; tests that only
// touch AppDataPath don't need it, but anything storing a value does.
func withSecretKey(t *testing.T) {
	t.Helper()
	prev := setting.SecretKey
	setting.SecretKey = "test-secret-key"
	t.Cleanup(func() { setting.SecretKey = prev })
}

func TestSaveAndLoadAppEnv(t *testing.T) {
	withTempAppData(t)
	withSecretKey(t)

	require.NoError(t, SaveAppEnv("PO", "app", "alice", map[string]string{
		"DATABASE_URL": "postgres://secret@db/x",
		"API_KEY":      "sk-abc",
	}, nil))

	env, version, err := LoadAppEnv("PO", "app")
	require.NoError(t, err)
	assert.Equal(t, "postgres://secret@db/x", env["DATABASE_URL"])
	assert.Equal(t, int64(1), version)

	t.Run("nothing is stored in plaintext", func(t *testing.T) {
		// The whole point of this store: an admin reading the data directory,
		// or a leaked backup, must not yield credentials.
		body, err := os.ReadFile(appEnvFilePath("PO", "app"))
		require.NoError(t, err)
		assert.NotContains(t, string(body), "postgres://secret@db/x")
		assert.NotContains(t, string(body), "sk-abc")
		assert.Contains(t, string(body), "DATABASE_URL", "names are not secret, and the UI needs them")
	})

	t.Run("names are listed, values never are", func(t *testing.T) {
		names, version, err := AppEnvNames("PO", "app")
		require.NoError(t, err)
		assert.Equal(t, []string{"API_KEY", "DATABASE_URL"}, names)
		assert.Equal(t, int64(1), version)
	})

	t.Run("a later save bumps the version so the UI can say restart required", func(t *testing.T) {
		require.NoError(t, SaveAppEnv("PO", "app", "bob", map[string]string{"API_KEY": "sk-new"}, nil))
		env, version, err := LoadAppEnv("PO", "app")
		require.NoError(t, err)
		assert.Equal(t, "sk-new", env["API_KEY"])
		assert.Equal(t, "postgres://secret@db/x", env["DATABASE_URL"], "an unrelated variable is untouched")
		assert.Equal(t, int64(2), version)
	})

	t.Run("unset removes, and wins over a simultaneous set", func(t *testing.T) {
		require.NoError(t, SaveAppEnv("PO", "app", "bob",
			map[string]string{"API_KEY": "ignored"}, []string{"API_KEY"}))
		env, _, err := LoadAppEnv("PO", "app")
		require.NoError(t, err)
		assert.NotContains(t, env, "API_KEY")
	})

	t.Run("history records who and which key, never the value", func(t *testing.T) {
		history := AppEnvHistory("PO", "app")
		require.NotEmpty(t, history)
		assert.Equal(t, "bob", history[0].Actor, "newest first")
		assert.Equal(t, []string{"API_KEY"}, history[0].Unset)
		assert.Equal(t, "alice", history[len(history)-1].Actor)
	})
}

// LD_PRELOAD is the one that turns a text field into code execution inside
// the sandbox; the rest would break the app in ways that look like a
// platform bug rather than a bad input.
func TestValidateEnvNameRejects(t *testing.T) {
	for _, name := range []string{
		"LD_PRELOAD", "ld_preload", "LD_LIBRARY_PATH",
		"PATH", "HOME", "PYTHONPATH", "SOCKET", "ROOT_PATH", "IFS",
		"", "1ABC", "A-B", "A B", "A=B", "A;rm -rf /",
	} {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, ValidateEnvName(name))
		})
	}
	for _, name := range []string{"API_KEY", "_internal", "db2Url"} {
		assert.NoError(t, ValidateEnvName(name), name)
	}
}

// One bad entry must change nothing — otherwise a form submission applies
// half of itself and the department has no way to know which half.
func TestSaveAppEnvIsAllOrNothing(t *testing.T) {
	withTempAppData(t)
	withSecretKey(t)

	require.NoError(t, SaveAppEnv("PO", "app", "alice", map[string]string{"GOOD": "1"}, nil))
	err := SaveAppEnv("PO", "app", "alice", map[string]string{
		"ALSO_GOOD":  "2",
		"LD_PRELOAD": "/tmp/evil.so",
	}, nil)
	require.Error(t, err)

	env, version, loadErr := LoadAppEnv("PO", "app")
	require.NoError(t, loadErr)
	assert.Equal(t, map[string]string{"GOOD": "1"}, env)
	assert.Equal(t, int64(1), version, "the rejected save did not bump the version")
}

func TestSaveAppEnvRejectsBadValues(t *testing.T) {
	withTempAppData(t)
	withSecretKey(t)

	// A NUL would silently truncate the credential in the environment block.
	assert.Error(t, SaveAppEnv("PO", "app", "a", map[string]string{"K": "a\x00b"}, nil))
	assert.Error(t, SaveAppEnv("PO", "app", "a",
		map[string]string{"K": strings.Repeat("x", maxEnvValueSize+1)}, nil))
}

// An app with no variables is the normal case, not an error — most apps
// never set one.
func TestLoadAppEnvMissingFile(t *testing.T) {
	withTempAppData(t)
	withSecretKey(t)

	env, version, err := LoadAppEnv("PO", "never-configured")
	require.NoError(t, err)
	assert.Empty(t, env)
	assert.Zero(t, version)
}

// A wrong SECRET_KEY must fail the start rather than hand the app an empty
// credential, which would fail much later and much less legibly.
func TestLoadAppEnvFailsClosedOnBadKey(t *testing.T) {
	withTempAppData(t)
	withSecretKey(t)
	require.NoError(t, SaveAppEnv("PO", "app", "alice", map[string]string{"API_KEY": "sk-abc"}, nil))

	setting.SecretKey = "a-different-key"
	_, _, err := LoadAppEnv("PO", "app")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "API_KEY")
	assert.NotContains(t, err.Error(), "sk-abc")
}
