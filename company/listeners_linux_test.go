// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux

package company

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A captured /proc/<pid>/net/tcp: one LISTEN row (st 0A) on port 0x1F90 =
// 8080, one ESTABLISHED row that must not count, plus the tcp6 table.
func TestListeningPortsReadsOnlyListenRows(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tcp"), []byte(
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
			"   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1 0\n"+
			"   1: 0100007F:C350 0100007F:0BB8 01 00000000:00000000 00:00000000 00000000  1000        0 2 0\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tcp6"), []byte(
		"  sl  local_address rem_address st\n"+
			"   0: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A\n"), 0o600))

	assert.ElementsMatch(t, []int{8080, 22}, listeningPortsIn(dir))
	assert.Empty(t, listeningPortsIn(filepath.Join(dir, "missing")), "no table, no ports — never an error")
}
