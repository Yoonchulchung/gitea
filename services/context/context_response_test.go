// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package context

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The reference is read out loud by somebody who cannot see the log, to
// somebody who can. Two properties matter: it never contains a character
// that is heard or seen as another one, and two errors do not share a
// number. See docs/company/error-reference.md.
func TestCompanyErrorRefIsReadableAndUnique(t *testing.T) {
	seen := make(map[string]bool, 500)
	for range 500 {
		ref := companyErrorRef()
		assert.Regexp(t, `^[2-9A-HJ-NP-Z]{3}-[2-9A-HJ-NP-Z]{3}$`, ref, "no 0/O/1/I, and grouped for reading")
		assert.False(t, seen[ref], "%s came up twice", ref)
		seen[ref] = true
	}
}
