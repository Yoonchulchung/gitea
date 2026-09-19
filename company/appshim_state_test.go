// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What an app puts in platform.state is on disk when the process ends
// normally and back in the dictionary before its code runs next time.
func TestShimStateSurvivesAStop(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(data, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, platformShimModule+".py"), platformShim, 0o600))
	env := append(os.Environ(), "DB_PATH="+filepath.Join(data, "app.db"), "COMPANY_APP_DIR="+dir, "PYTHONPATH="+dir)

	run := func(code string) string {
		cmd := exec.Command(python, "-c", code) //nolint:gosec // the system interpreter running a fixed test script
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}
	run("import _company_platform as p; p.state['cache'] = {'n': 3}")
	assert.FileExists(t, filepath.Join(data, "platform-state.json"))
	out := run("import _company_platform as p; print(p.state['cache']['n'])")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(out), "3"), out)
}
