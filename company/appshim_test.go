// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlatformShimRedirectsSQLite(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to exercise the shim")
	}
	root := t.TempDir()
	sitePackages := filepath.Join(root, "venv", "lib", "python3", "site-packages")
	codeDir := filepath.Join(root, "app")
	dataDir := filepath.Join(root, "data")
	scratch := filepath.Join(root, "tmp")
	for _, dir := range []string{sitePackages, codeDir, dataDir, scratch} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	require.NoError(t, installPlatformShim(filepath.Join(root, "venv")))
	require.NoError(t, installPlatformShim(filepath.Join(root, "venv")), "installing twice is a no-op")
	require.NoError(t, os.Chmod(codeDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(codeDir, 0o700) })

	script := `
import site, sys
site.addsitedir(sys.argv[1])
import sqlite3, sqlite3.dbapi2
sqlite3.connect("app.db").execute("CREATE TABLE relative (x)")
sqlite3.dbapi2.connect(sys.argv[2] + "/data/x.db").execute("CREATE TABLE in_code_dir (x)")
sqlite3.connect("file:app.db?mode=rwc", uri=True).execute("CREATE TABLE uri (x)")
sqlite3.connect(":memory:").execute("CREATE TABLE memory (x)")
sqlite3.connect(sys.argv[3] + "/scratch.db").execute("CREATE TABLE scratch (x)")
`
	cmd := exec.Command(python, "-c", script, sitePackages, codeDir, scratch)
	cmd.Dir = codeDir
	cmd.Env = []string{"DB_PATH=" + filepath.Join(dataDir, appDataDBName), platformShimEnv + "=" + codeDir}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.Contains(t, string(out), "[platform] SQLite database 'app.db' is stored at")

	tables := func(db string) string {
		out, err := exec.Command(python, "-c",
			`import sqlite3, sys; print(sorted(r[0] for r in sqlite3.connect(sys.argv[1]).execute("SELECT name FROM sqlite_master")))`,
			db).Output()
		require.NoError(t, err)
		return string(out)
	}
	assert.Equal(t, "['in_code_dir', 'relative', 'uri']\n", tables(filepath.Join(dataDir, appDataDBName)))
	assert.Equal(t, "['scratch']\n", tables(filepath.Join(scratch, "scratch.db")), "a writable path outside the code is the app's own choice")
}
