// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// The person deploying an app should not have to know that its code
// directory is read-only or that DB_PATH exists, so the platform redirects
// SQLite itself. A .pth file rather than sitecustomize.py, which Debian's
// own copy in the stdlib directory would shadow.
//
//go:embed appshim/_company_platform.py
var platformShim []byte

const platformShimModule = "_company_platform"

// platformShimEnv names the app's code directory for the shim. Only the app
// gets it: the migration and data runners share the venv, and outside
// bubblewrap their cwd is Gitea's, which may contain the data directory.
const platformShimEnv = "COMPANY_APP_DIR"

func appCodeDirForProcess(codeDir string) string {
	if mode, _ := sandboxMode(); mode == SandboxBubblewrap {
		return "/app"
	}
	return codeDir
}

// installPlatformShim puts the shim into a virtualenv. Run on every start
// rather than at build time, so venvs built before it existed get it too.
func installPlatformShim(venv string) error {
	dirs, err := filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages"))
	if err != nil {
		return err
	}
	if len(dirs) != 1 {
		return fmt.Errorf("expected one site-packages in %s, found %d", venv, len(dirs))
	}
	for name, body := range map[string][]byte{
		platformShimModule + ".py":  platformShim,
		platformShimModule + ".pth": []byte("import " + platformShimModule + "\n"),
	} {
		file := filepath.Join(dirs[0], name)
		if old, err := os.ReadFile(file); err == nil && bytes.Equal(old, body) {
			continue // the venv is shared by every release with the same requirements
		}
		if err := writeFileAtomic(file, body); err != nil {
			return err
		}
	}
	return nil
}
