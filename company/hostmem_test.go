// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseMeminfo(t *testing.T) {
	m, ok := parseMeminfo("MemTotal:       16303484 kB\nMemFree:         1234 kB\nMemAvailable:    8120000 kB\nBuffers: 1 kB\n")
	assert.True(t, ok)
	assert.Equal(t, int64(16303484)<<10, m.TotalBytes)
	assert.Equal(t, int64(8120000)<<10, m.AvailableBytes)
	assert.True(t, m.HasAvailable)

	_, ok = parseMeminfo("nothing here")
	assert.False(t, ok)
}
