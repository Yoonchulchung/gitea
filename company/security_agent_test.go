// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What the model asks to mail is checked: a known severity, a one-line summary
// (it becomes a subject), and the request's own fields left as they were.
func TestSecurityAlertFrom(t *testing.T) {
	base := MailFields{"app": "PO/da", "link": "http://x/pulls/1"}
	alert, fields, err := securityAlertFrom(" HIGH ", "line one\r\nBcc: someone@evil", "details", base)
	require.NoError(t, err)
	assert.Equal(t, "high", alert.Severity)
	assert.NotContains(t, fields["summary"], "\n")
	assert.Equal(t, "PO/da", fields["app"])
	assert.NotContains(t, base, "summary") // the request's fields are not changed for the next alert

	_, fields, err = securityAlertFrom("critical", strings.Repeat("가", 500), "d", base)
	require.NoError(t, err)
	assert.LessOrEqual(t, len([]rune(fields["summary"])), 201)

	for _, bad := range [][3]string{{"urgent", "s", "d"}, {"high", " ", "d"}, {"high", "s", ""}} {
		_, _, err := securityAlertFrom(bad[0], bad[1], bad[2], base)
		assert.Error(t, err, "%v", bad)
	}
}

func TestSecurityAlertRuleStartsWritten(t *testing.T) {
	assert.Contains(t, MailEvents(), MailEventSecurityAlert)
	assert.Contains(t, MailFieldsFor(MailEventSecurityAlert), "summary")
	for _, field := range []string{"app", "severity", "summary", "details", "link"} {
		assert.Contains(t, securityAlertDefaultSubject+securityAlertDefaultBody, "{{"+field+"}}")
	}
}
