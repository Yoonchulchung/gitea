// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"testing"

	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const longKey = "96462367fd540a84fe20d548acfd0bf99f8c460bf21d440b74e85b7c92b4387a"

// Shortening appKey renamed every directory the platform owns, so an
// instance that upgraded found its apps with no releases, no metrics and no
// secrets — reporting that they had never been deployed.
func TestMigrateAppKeyLength(t *testing.T) {
	withTempAppData(t)
	state := filepath.Join(setting.AppDataPath, "company-app-state")
	apps := filepath.Join(setting.AppDataPath, "company-apps")
	require.NoError(t, os.MkdirAll(state, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(apps, longKey, "releases"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(state, longKey+".json"), []byte(`{"owner":"PO"}`), 0o600))

	migrateAppKeyLength()

	short := longKey[:appKeyLength]
	assert.FileExists(t, filepath.Join(state, short+".json"))
	assert.DirExists(t, filepath.Join(apps, short, "releases"))
	assert.NoFileExists(t, filepath.Join(state, longKey+".json"))
}

// Both names exist when the instance ran after the upgrade and before this
// migration. Everything written in that window is a record of the breakage,
// not of anything real — but it is still someone's data, so it is moved
// aside rather than deleted.
func TestMigrateKeepsBothWhenTheyCollide(t *testing.T) {
	withTempAppData(t)
	state := filepath.Join(setting.AppDataPath, "company-app-state")
	require.NoError(t, os.MkdirAll(state, 0o700))
	short := longKey[:appKeyLength]
	require.NoError(t, os.WriteFile(filepath.Join(state, longKey+".json"), []byte(`{"real":true}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(state, short+".json"), []byte(`{"broken":true}`), 0o600))

	migrateAppKeyLength()

	body, err := os.ReadFile(filepath.Join(state, short+".json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"real":true}`, string(body), "the pre-upgrade history wins")
	assert.FileExists(t, filepath.Join(state, short+".json.post-upgrade"), "nothing is thrown away")
}

// Anything not shaped like the old key is left alone, and an instance that
// never wrote one migrates nothing.
func TestMigrateLeavesEverythingElseAlone(t *testing.T) {
	withTempAppData(t)
	state := filepath.Join(setting.AppDataPath, "company-app-state")
	require.NoError(t, os.MkdirAll(state, 0o700))
	for _, name := range []string{"96462367fd540a84.json", "notahexname.json", "README"} {
		require.NoError(t, os.WriteFile(filepath.Join(state, name), []byte("x"), 0o600))
	}

	assert.NotPanics(t, migrateAppKeyLength)

	for _, name := range []string{"96462367fd540a84.json", "notahexname.json", "README"} {
		assert.FileExists(t, filepath.Join(state, name))
	}
}

// An app's tree is held together by absolute symlinks — current and previous
// point at a release, each release's .venv at a shared environment — so
// renaming the directory above them leaves every one dangling. The app then
// has a release it cannot reach, which looks exactly like having none.
func TestMigrateRepointsSymlinks(t *testing.T) {
	withTempAppData(t)
	apps := filepath.Join(setting.AppDataPath, "company-apps")
	oldHome := filepath.Join(apps, longKey)
	release := filepath.Join(oldHome, "releases", "ff317fb6")
	venv := filepath.Join(oldHome, "venvs", "831e275f")
	require.NoError(t, os.MkdirAll(release, 0o700))
	require.NoError(t, os.MkdirAll(venv, 0o700))
	require.NoError(t, os.Symlink(release, filepath.Join(oldHome, "current")))
	require.NoError(t, os.Symlink(venv, filepath.Join(release, ".venv")))

	migrateAppKeyLength()

	newHome := filepath.Join(apps, longKey[:appKeyLength])
	current, err := os.Readlink(filepath.Join(newHome, "current"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(newHome, "releases", "ff317fb6"), current)
	assert.DirExists(t, current, "and it resolves")

	// Repointed into the new home rather than left dangling at the old path.
	// The environment it names is deliberately gone — a venv cannot be moved,
	// so it is rebuilt by the next deploy rather than patched (see
	// TestMigrateDiscardsStaleVenvs).
	venvLink, err := os.Readlink(filepath.Join(newHome, "releases", "ff317fb6", ".venv"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(newHome, "venvs", "831e275f"), venvLink)
}

// A virtualenv is not relocatable: every console script in it names the
// interpreter by absolute path, so after a rename the script is still there
// and still executable while the interpreter it points at is not.
func TestMigrateDiscardsStaleVenvs(t *testing.T) {
	withTempAppData(t)
	apps := filepath.Join(setting.AppDataPath, "company-apps")
	oldHome := filepath.Join(apps, longKey)
	require.NoError(t, os.MkdirAll(filepath.Join(oldHome, "venvs", "831e275f", "bin"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(oldHome, "venvs", "831e275f", "bin", "uvicorn"),
		[]byte("#!"+oldHome+"/venvs/831e275f/bin/python3\n"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(oldHome, "releases", "ff317fb6"), 0o700))

	migrateAppKeyLength()

	newHome := filepath.Join(apps, longKey[:appKeyLength])
	assert.NoDirExists(t, filepath.Join(newHome, "venvs"),
		"a shebang cannot be repointed, so the environment is rebuilt rather than patched")
	assert.DirExists(t, filepath.Join(newHome, "releases", "ff317fb6"),
		"the code itself is kept — only the environment is rebuilt")
}
