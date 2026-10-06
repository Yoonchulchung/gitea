// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAIReviewCardMarkdown(t *testing.T) {
	md := aiReviewCard{
		Title:    "자동 승인 보류",
		Purpose:  "주간 보고서를\n보여 줍니다.",
		Summary:  "환경 변수를 외부로 보냅니다.",
		Security: []aiFinding{{File: "main.py", Issue: "os.environ | 전체를 POST"}},
		Model:    "claude-fable-5",
		Checked:  []string{"요청에 포함된 변경 내용", "read_file main.py"},
	}.Markdown()

	assert.True(t, isAIReviewComment(md))
	assert.Contains(t, md, "#### AI 검토 · 자동 승인 보류")
	assert.Contains(t, md, "**이 코드가 하는 일** — 주간 보고서를 보여 줍니다.")
	assert.Contains(t, md, "| **보안** | `main.py` | os.environ \\| 전체를 POST |") // a pipe in model text cannot split the row
	assert.Contains(t, md, "모델: claude-fable-5 · 확인한 것: 요청에 포함된 변경 내용, read_file main.py")
	for _, icon := range []string{"🤖", "✅", "⏸️", "🚨", "🔴", "⚠️"} {
		assert.NotContains(t, md, icon)
	}

	// A decision no AI made says nothing about a model.
	assert.NotContains(t, aiReviewCard{Title: "자동 승인 보류", Summary: "권한 요청 1건"}.Markdown(), "모델:")

	// Comments posted before the cards are still told apart from a person's.
	assert.True(t, isAIReviewComment("🤖 **AI Review**\n\nold"))
	assert.False(t, isAIReviewComment("반려합니다"))
}

func TestParseAICodeReview(t *testing.T) {
	r, ok := parseAICodeReview("```json\n{\"purpose\": \"보고서 앱\", \"summary\": \"문제 없음\", \"security\": [], \"other\": [{\"file\": \"a.py\", \"issue\": \"오타\"}]}\n```")
	assert.True(t, ok)
	assert.Equal(t, "보고서 앱", r.Purpose)
	assert.Equal(t, []aiFinding{{File: "a.py", Issue: "오타"}}, r.Other)

	for _, bad := range []string{"", "그냥 텍스트", `{"security": []}`} {
		_, ok := parseAICodeReview(bad)
		assert.False(t, ok, bad)
	}
	assert.NotContains(t, aiReviewCommentMarker, "\n")
}

// The sweeper leaves a request alone once one of these cards is on it; a code
// review is not a decision.
func TestAutoApproveDecisionCards(t *testing.T) {
	for _, title := range []string{"자동 승인", "자동 승인 보류", "자동 승인 중지"} {
		assert.Contains(t, aiReviewCard{Title: title}.Markdown(), autoApproveDecisionTitle, title)
	}
	assert.NotContains(t, aiReviewCard{Title: "코드 리뷰"}.Markdown(), autoApproveDecisionTitle)
	assert.NotContains(t, aiReviewCard{Title: "보안 경고 (high)"}.Markdown(), autoApproveDecisionTitle)
}
