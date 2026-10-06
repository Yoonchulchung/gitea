// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"

	"gitea.dev/modules/json"
)

// Every comment the platform AI leaves on a request has the same shape, so an
// administrator reads them the same way: what was decided, why, what was found
// where, and what the AI actually looked at. The model answers in JSON and the
// layout is ours — a model asked for prose formats it differently every time.

// aiReviewCommentMarker starts every AI comment. An HTML comment, so the page
// shows nothing for it; it is how the platform tells an AI comment from one a
// person wrote (company/deploystatus.go).
const aiReviewCommentMarker = "<!-- company:ai-review -->"

// legacyAIReviewCommentMarker is what comments posted before the cards began with.
const legacyAIReviewCommentMarker = "🤖 **AI Review**"

func isAIReviewComment(content string) bool {
	return strings.HasPrefix(content, aiReviewCommentMarker) || strings.HasPrefix(content, legacyAIReviewCommentMarker)
}

// aiFinding is one thing the AI points at, as the model writes it.
type aiFinding struct {
	File  string `json:"file"`
	Issue string `json:"issue"`
}

// aiReviewCard is one AI comment.
type aiReviewCard struct {
	Title    string      // what happened: "자동 승인", "자동 승인 보류", "코드 리뷰"
	Purpose  string      // what the code does for its users, so the verdict has a context
	Summary  string      // the decision or the gist, in a sentence or two
	Security []aiFinding // listed first: the reason the review exists
	Other    []aiFinding
	Footer   string   // one line under the table, e.g. what was sent
	Model    string   // which model judged; empty for a decision no AI made
	Checked  []string // what the AI read, for whoever asks what a verdict rests on
}

func (c aiReviewCard) Markdown() string {
	var b strings.Builder
	b.WriteString(aiReviewCommentMarker + "\n")
	b.WriteString("#### AI 검토 · " + oneLine(c.Title) + "\n\n")
	if s := oneLine(c.Purpose); s != "" {
		b.WriteString("**이 코드가 하는 일** — " + s + "\n\n")
	}
	if s := strings.TrimSpace(c.Summary); s != "" {
		if c.Purpose != "" {
			s = "**판단** — " + s
		}
		b.WriteString(s + "\n\n")
	}
	if len(c.Security)+len(c.Other) > 0 {
		b.WriteString("| 구분 | 파일 | 내용 |\n|---|---|---|\n")
		for _, f := range c.Security {
			b.WriteString("| **보안** | " + fileCell(f.File) + " | " + tableCell(f.Issue) + " |\n")
		}
		for _, f := range c.Other {
			b.WriteString("| 기타 | " + fileCell(f.File) + " | " + tableCell(f.Issue) + " |\n")
		}
		b.WriteString("\n")
	}
	if s := strings.TrimSpace(c.Footer); s != "" {
		b.WriteString(s + "\n\n")
	}
	var meta []string
	if c.Model != "" {
		meta = append(meta, "모델: "+oneLine(c.Model))
	}
	if len(c.Checked) > 0 {
		meta = append(meta, "확인한 것: "+oneLine(strings.Join(c.Checked, ", ")))
	}
	if len(meta) > 0 {
		b.WriteString("<sub>" + strings.Join(meta, " · ") + "</sub>\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// oneLine keeps model-written text from breaking the layout around it.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func tableCell(s string) string {
	if s = strings.ReplaceAll(oneLine(s), "|", `\|`); s == "" {
		return "-"
	}
	return s
}

func fileCell(s string) string {
	s = strings.Trim(oneLine(s), "`")
	if s == "" {
		return "-"
	}
	return "`" + strings.ReplaceAll(s, "|", `\|`) + "`"
}

// aiCodeReview is the JSON a code review answers with (aiReviewSystemPrompt).
type aiCodeReview struct {
	Purpose  string      `json:"purpose"`
	Summary  string      `json:"summary"`
	Security []aiFinding `json:"security"`
	Other    []aiFinding `json:"other"`
}

// parseAICodeReview reads the model's JSON; ok is false when it is not that,
// and the caller shows the text as it came instead.
func parseAICodeReview(reply string) (aiCodeReview, bool) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return aiCodeReview{}, false
	}
	var r aiCodeReview
	if err := json.Unmarshal([]byte(reply[start:end+1]), &r); err != nil || strings.TrimSpace(r.Summary) == "" {
		return aiCodeReview{}, false
	}
	return r, true
}
