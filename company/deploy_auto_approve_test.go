// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	user_model "gitea.dev/models/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeployApprovalPolicyNeedsApproval(t *testing.T) {
	pkg := []PermissionRequest{{Kind: PermKindPackage, Value: "pandas"}}
	host := []PermissionRequest{{Kind: PermKindNetwork, Value: "erp.internal"}}
	waived := deployApprovalPolicy{WaivedBy: &user_model.User{ID: 1}}

	assert.True(t, deployApprovalPolicy{}.needsApproval(nil))
	assert.False(t, waived.needsApproval(nil), "a first deploy included")
	assert.False(t, waived.needsApproval(append(pkg, host...)))
	assert.True(t, deployApprovalPolicy{WaivedBy: waived.WaivedBy, PackageApproval: true}.needsApproval(pkg))
	assert.False(t, deployApprovalPolicy{WaivedBy: waived.WaivedBy, PackageApproval: true}.needsApproval(host))
	assert.True(t, deployApprovalPolicy{WaivedBy: waived.WaivedBy, NetworkApproval: true}.needsApproval(host))
	assert.True(t, waived.needsApproval([]PermissionRequest{{Kind: PermKindMemory, Value: "1024"}}))
}

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

func TestFreePackagesSkipTheInstalledCheck(t *testing.T) {
	venv := t.TempDir()
	dist := filepath.Join(venv, "lib", "python3.12", "site-packages", "humanize-4.9.0.dist-info")
	require.NoError(t, os.MkdirAll(dist, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "METADATA"), []byte("Name: humanize\nVersion: 4.9.0\n"), 0o600))

	extra, err := unapprovedInstalled(venv, AppSettings{}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"humanize"}, extra, "unapproved, so refused")
	extra, err = unapprovedInstalled(venv, AppSettings{packagesFree: true}, nil)
	require.NoError(t, err)
	assert.Empty(t, extra, "installed as asked while packages need no approval")
}
