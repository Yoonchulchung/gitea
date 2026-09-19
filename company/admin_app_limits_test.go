// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Empty means the default; a typo is refused rather than read as 0.
func TestParseLimitMB(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want int
	}{{"", 0}, {" 256 ", 256}, {"16384", 16384}} {
		got, ok := parseLimitMB(c.raw, 16384)
		assert.True(t, ok, c.raw)
		assert.Equal(t, c.want, got, c.raw)
	}
	for _, raw := range []string{"abc", "-1", "16385", "1.5"} {
		_, ok := parseLimitMB(raw, 16384)
		assert.False(t, ok, raw)
	}
}
