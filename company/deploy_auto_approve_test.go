// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAutoApproveBlocker(t *testing.T) {
	ok := autoApproveFacts{StartupState: "passed", DiffChars: 100}
	assert.Empty(t, autoApproveBlocker(ok))
	for _, change := range []func(*autoApproveFacts){
		func(f *autoApproveFacts) { f.StartupState = "blocked" }, // a transitive package awaits approval
		func(f *autoApproveFacts) { f.StartupState = "" },
		func(f *autoApproveFacts) { f.Removal = true },
		func(f *autoApproveFacts) { f.VersionMismatch = true }, // the check saw other files than the request's
		func(f *autoApproveFacts) { f.Permissions = 1 },
		func(f *autoApproveFacts) { f.PendingPackages = 1 },
		func(f *autoApproveFacts) { f.DiffChars = 0 },
		func(f *autoApproveFacts) { f.DiffChars = autoApproveDiffMaxChars + 1 },
		func(f *autoApproveFacts) {
			f.RiskyCode = []aiFinding{{File: "main.py:3", Issue: "환경 변수 읽기 — ..."}}
		},
	} {
		f := ok
		change(&f)
		assert.NotEmpty(t, autoApproveBlocker(f), "%+v", f)
	}
}

func TestParseAutoApproveVerdict(t *testing.T) {
	v, ok := parseAutoApproveVerdict("```json\n{\"safe\": true, \"reason\": \"app.py 문구 변경뿐입니다.\"}\n```")
	assert.True(t, ok)
	assert.Equal(t, autoApproveVerdict{Safe: true, Reason: "app.py 문구 변경뿐입니다."}, v)

	v, ok = parseAutoApproveVerdict(`{"safe": false, "purpose": "보고서 앱", "reason": "외부 전송", "findings": [{"file": "main.py", "issue": "env POST"}]}`)
	assert.True(t, ok)
	assert.False(t, v.Safe)
	assert.Equal(t, "보고서 앱", v.Purpose)
	assert.Equal(t, []aiFinding{{File: "main.py", Issue: "env POST"}}, v.Findings)

	for _, reply := range []string{"", "safe", `{"reason": "x"}`, `{"safe": true}`, `{"safe": "true", "reason": "x"}`, `{"safe": true, "reason": "x"`} {
		_, ok := parseAutoApproveVerdict(reply)
		assert.False(t, ok, reply)
	}
}

// A request asking for a permission is never approved by the AI, and the
// reason given is the permission even when something else would also hold it.
func TestPermissionRequestAlwaysHolds(t *testing.T) {
	reason := autoApproveBlocker(autoApproveFacts{Permissions: 1, StartupState: "failed", DiffChars: 100})
	assert.Contains(t, reason, "권한 요청 1건")

	assert.Equal(t, "package: requests (외부 API 호출), access: login",
		describePermissionRequests([]PermissionRequest{{Kind: "package", Value: "requests", Reason: "외부 API 호출"}, {Kind: "access", Value: "login"}}))
}

func TestApprovalNeededRuleStartsWritten(t *testing.T) {
	assert.Contains(t, MailEvents(), MailEventApprovalNeeded)
	fields := MailFieldsFor(MailEventApprovalNeeded)
	for _, placeholder := range regexp.MustCompile(`\{\{(\w+)\}\}`).FindAllStringSubmatch(approvalNeededDefaultSubject+approvalNeededDefaultBody, -1) {
		assert.Contains(t, fields, placeholder[1])
	}
}
