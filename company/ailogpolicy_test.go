// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMaskForAIHidesSecretsAndShapes(t *testing.T) {
	in := `user kim@example.com (010-1234-5678, 900101-1234567) paid with 1234-5678-9012-3456
Authorization: Bearer abcdefghijklmnop123456 password=hunter2x api_key: "sk-live-0123456789abcdef"
connect 10.0.12.7 with token ERPKEY-9f8e7d6c5b4a3210 sha 5af9ccc968d0f3e2b1a0c9d8e7f6a5b4c3d2e1f0
NameError: name 'TEST' is not defined`
	out := maskForAI(in, []string{"ERPKEY-9f8e7d6c5b4a3210", "ab"})
	for _, gone := range []string{"kim@example.com", "010-1234-5678", "900101-1234567", "1234-5678-9012-3456", "abcdefghijklmnop123456", "hunter2x", "sk-live-0123456789abcdef", "10.0.12.7", "ERPKEY-9f8e7d6c5b4a3210", "5af9ccc968d0f3e2b1a0c9d8e7f6a5b4c3d2e1f0"} {
		assert.NotContains(t, out, gone)
	}
	assert.Contains(t, out, "NameError: name 'TEST' is not defined", "what the fix needs stays")
	assert.Contains(t, out, "[secret]")
	assert.Contains(t, out, "[email]")
	assert.NotContains(t, out, "[secret]bcdefgh", "a two-letter secret is not applied")
}

func TestAILogLinesDefaultToFailuresOnly(t *testing.T) {
	assert.Equal(t, AILogAccessErrors, AILogAccess(), "unset means errors")
	lines := []LogLine{
		{Text: "INFO: row {'name': '홍길동', 'salary': 5200}"},
		{Text: "Traceback (most recent call last):"},
		{Text: `  File "main.py", line 3, in <module>`},
		{Text: "    TEST"},
		{Text: "NameError: name 'TEST' is not defined"},
	}
	kept := aiLogLines("PO", "app", lines)
	assert.Len(t, kept, 4)
	assert.Equal(t, "Traceback (most recent call last):", kept[0].Text)
	assert.Equal(t, "", aiOutputText("PO", "app", "INFO: started\nINFO: row 1\n"), "no failure, nothing leaves")
}
