// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The help link is admin-typed and rendered as an href on every page, so a
// scheme check is the difference between a wrong link and script execution
// for everyone who clicks Help. An administrator is trusted with the
// instance, not with a mistake that becomes everybody's.
func TestSafeLinkURLRejectsScriptSchemes(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD4=",
		"vbscript:msgbox(1)",
		"file:///etc/passwd",
		"/wiki/help",       // no host: relative links are not what this field is for
		"wiki.example.com", // parses, but as a path with no scheme
	} {
		_, ok := safeLinkURL(raw)
		assert.False(t, ok, "%q must be refused", raw)
	}
}

func TestSafeLinkURLAcceptsWebLinks(t *testing.T) {
	for _, raw := range []string{
		"https://wiki.example.com/deploy",
		"http://intranet:8080/help?team=po",
		"  https://example.com/x  ", // trimmed, not rejected
	} {
		got, ok := safeLinkURL(raw)
		assert.True(t, ok, "%q must be accepted", raw)
		assert.NotEmpty(t, got)
	}
	// Blank is how an administrator removes the link, not an error.
	got, ok := safeLinkURL("")
	assert.True(t, ok)
	assert.Empty(t, got)
}
