// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseAIHeaders(t *testing.T) {
	headers, err := parseAIHeaders("x-team-id: sales\r\n\nX-Gateway-Key:  abc:def \n")
	assert.NoError(t, err)
	assert.Equal(t, http.Header{"X-Team-Id": {"sales"}, "X-Gateway-Key": {"abc:def"}}, headers)

	headers, err = parseAIHeaders("")
	assert.NoError(t, err)
	assert.Empty(t, headers)

	for _, bad := range []string{"no colon", "Bad Name: x", "Host: evil", "content-length: 1", "X-A: a\x00b"} {
		_, err := parseAIHeaders(bad)
		assert.Error(t, err, bad)
	}
}

func TestApplyAIHeadersKeepsProviderAuth(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://gateway/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer key")
	applyAIHeaders(req, http.Header{"X-Team-Id": {"sales"}})
	assert.Equal(t, "Bearer key", req.Header.Get("Authorization"))
	assert.Equal(t, "sales", req.Header.Get("X-Team-Id"))
}

func TestFormAIHeaders(t *testing.T) {
	saved := http.Header{"X-Gateway-Key": {"secret"}, "X-Old": {"gone"}}
	text, err := formAIHeaders(
		[]string{"X-Gateway-Key", "x-team-id", "", ""},
		[]string{"", "sales", "", ""}, saved) // blank value keeps the saved one; X-Old's row was removed
	assert.NoError(t, err)
	headers, _ := parseAIHeaders(text)
	assert.Equal(t, http.Header{"X-Gateway-Key": {"secret"}, "X-Team-Id": {"sales"}}, headers)

	text, err = formAIHeaders(nil, nil, saved)
	assert.NoError(t, err)
	assert.Empty(t, text)

	for _, rows := range [][2][]string{
		{{"X-New"}, {""}},        // new header with no value
		{{""}, {"orphan"}},       // value with no name
		{{"Host"}, {"evil"}},     // reserved
		{{"X-A"}, {"a\nX-B: b"}}, // value smuggling a second header
		{{"X-A: b"}, {"c"}},      // name smuggling a value
	} {
		_, err := formAIHeaders(rows[0], rows[1], saved)
		assert.Error(t, err, "%v", rows)
	}
}
