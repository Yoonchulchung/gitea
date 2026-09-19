// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCgroupWrapGivesTheWrapperTheBusAndTakesItFromTheApp(t *testing.T) {
	argv := cgroupWrap(cgroupTools{systemdRun: "/usr/bin/systemd-run", env: "/usr/bin/env"},
		AppLimits{MemoryMB: 192, CPUPercent: 100, Processes: 64}, []string{"/venv/bin/uvicorn", "main:app"})
	assert.Equal(t, []string{
		"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect",
		"-p", "MemoryMax=192M", "-p", "MemoryHigh=172M", "-p", "MemorySwapMax=0",
		"-p", "CPUQuota=100%", "-p", "TasksMax=64", "--",
		"/usr/bin/env", "-u", "XDG_RUNTIME_DIR", "-u", "DBUS_SESSION_BUS_ADDRESS",
		"/venv/bin/uvicorn", "main:app",
	}, argv)

	// The bus is looked for where systemd-run will look, from the platform's
	// own environment — which the app's environment never carries.
	dir := t.TempDir()
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", dir)
	_, err := sessionBusReachable()
	require.Error(t, err, "no socket yet")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bus"), nil, 0o600))
	socket, err := sessionBusReachable()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "bus"), socket)
	assert.Equal(t, []string{"XDG_RUNTIME_DIR=" + dir}, sessionBusEnv())
	assert.NotContains(t, buildEnv(appPathsFor("PO", "app"), "/apps/PO/app", "", nil, false), "XDG_RUNTIME_DIR="+dir)
}
