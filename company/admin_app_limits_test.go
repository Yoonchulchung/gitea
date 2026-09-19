// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Empty means the default; a typo is refused rather than read as 0.
func TestParseLimitMB(t *testing.T) {
	for raw, want := range map[string]int{"": 0, " 256 ": 256, "16384": 16384} {
		got, ok := parseLimitMB(raw, 16384)
		assert.True(t, ok, raw)
		assert.Equal(t, want, got, raw)
	}
	for _, raw := range []string{"abc", "-1", "16385", "1.5"} {
		_, ok := parseLimitMB(raw, 16384)
		assert.False(t, ok, raw)
	}
}
