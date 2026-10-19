// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestScanAttemptOutputTakesOnlyThatAttempt(t *testing.T) {
	log := `
===== 2026-09-06T20:00:00+09:00  OK  aaaaaaaaaaaa =====
deployed
===== 2026-09-06T20:32:22+09:00  FAILED  51f705abb655 =====
pip could not install pandas==9.9
ERROR: No matching distribution found
===== 2026-09-06T21:00:00+09:00  OK  bbbbbbbbbbbb =====
deployed again
`
	at, _ := time.Parse(time.RFC3339, "2026-09-06T20:32:22+09:00")
	got := scanAttemptOutput(bufio.NewScanner(strings.NewReader(log)), at.Unix())
	assert.Equal(t, []string{"pip could not install pandas==9.9", "ERROR: No matching distribution found"}, got)
	assert.Nil(t, scanAttemptOutput(bufio.NewScanner(strings.NewReader(log)), 1), "an attempt no longer in the log")
}
