// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppDocsLoadAndAnswerInBothLanguages(t *testing.T) {
	docs := AppDocs()
	require.NotEmpty(t, docs)
	always := 0
	for _, d := range docs {
		assert.NotEmpty(t, d.Title, d.ID)
		assert.NotEmpty(t, d.Summary, d.ID)
		assert.NotEmpty(t, d.Keywords, d.ID)
		assert.NotEmpty(t, d.Sections, d.ID)
		if d.Always {
			always++
		}
	}
	assert.Equal(t, 1, always, "exactly the contract is sent with every request")

	// A Korean question about downloads reaches the download document, and
	// the same question in English reaches the same place.
	for _, q := range []string{"엑셀 다운로드가 안 돼요", "the excel download does not work"} {
		hits := SearchAppDocs(q, 3)
		require.NotEmpty(t, hits, q)
		assert.Equal(t, "60-downloads", hits[0].DocID, q)
	}
	hits := SearchAppDocs("sqlite3.OperationalError: attempt to write a readonly database", 3)
	require.NotEmpty(t, hits)
	assert.Contains(t, []string{"30-data", "90-errors"}, hits[0].DocID)

	ctx := AppDocsContext("입력 화면으로 가면 index.html 404가 나요")
	assert.Contains(t, ctx, "main.py", "the contract is always there")
	assert.Contains(t, ctx, "상대 경로", "and the section about links")
	assert.Less(t, len(ctx), docsContextBudget+6000, "the budget holds")
	assert.NotEmpty(t, strings.TrimSpace(AppDocsSearchText("nothing about this at all zzqq")))
}
