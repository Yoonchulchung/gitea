// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A host, a domain with everything under it, a host with a port, a port
// alone — and nothing else.
func TestBlocklistEntriesAndMatching(t *testing.T) {
	for raw, want := range map[string]string{
		"Example.com":          "example.com",
		"https://example.com/": "example.com",
		"*.Example.com":        "*.example.com",
		"example.com:8443":     "example.com:8443",
		":25":                  ":25",
	} {
		got, err := normalizeBlockEntry(raw)
		assert.NoError(t, err, raw)
		assert.Equal(t, want, got, raw)
	}
	for _, raw := range []string{"", "   ", "bad host", "example.com:99999", "a/b", "*"} {
		_, err := normalizeBlockEntry(raw)
		assert.Error(t, err, raw)
	}

	entries := []string{"example.com", "*.social.test", "legacy.test:8443", ":25"}
	assert.Equal(t, "example.com", blockedBy(entries, "EXAMPLE.com", 443))
	assert.Equal(t, "*.social.test", blockedBy(entries, "social.test", 443))
	assert.Equal(t, "*.social.test", blockedBy(entries, "cdn.social.test", 443))
	assert.Equal(t, "", blockedBy(entries, "notsocial.test", 443))
	assert.Equal(t, "", blockedBy(entries, "legacy.test", 443), "another port of the same host is not the entry")
	assert.Equal(t, "legacy.test:8443", blockedBy(entries, "legacy.test", 8443))
	assert.Equal(t, ":25", blockedBy(entries, "anything.test", 25))
	assert.Equal(t, "", blockedBy(nil, "example.com", 443))
}
