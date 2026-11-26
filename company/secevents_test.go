// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSecurityKindForBlock(t *testing.T) {
	kind, detail := securityKindForBlock("probe for /apps/PO/x/.env")
	assert.Equal(t, SecProbe, kind)
	assert.Equal(t, "/.env", detail)
	kind, _ = securityKindForBlock("not a member of PO/x")
	assert.Equal(t, SecForbidden, kind)
	for _, normal := range []string{"", "sign-in required", "app disabled", "banned"} {
		kind, _ = securityKindForBlock(normal)
		assert.Empty(t, kind, normal) // the platform working, or already counted when the ban began
	}
}

func TestSummarizeSecurity(t *testing.T) {
	since := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return since.Add(time.Duration(h)*time.Hour + time.Minute) }
	events := []SecurityEvent{
		{At: at(1), Kind: SecLoginFailed, Source: "10.0.0.9", Detail: "admin"},
		{At: at(1), Kind: SecLoginFailed, Source: "10.0.0.9", Detail: "root"},
		{At: at(3), Kind: SecProbe, Source: "10.0.0.9", App: "PO/x", Detail: "/.env"},
		{At: at(3), Kind: SecOutboundRefused, App: "PO/y", Detail: "GET evil.example:443"},
		{At: at(4), Kind: SecRateLimit, Source: "10.0.0.7", App: "PO/x"}, // in the hour still filling
	}
	s := summarizeSecurity(events, since, since.Add(4*time.Hour+30*time.Minute), time.Hour, "15")
	assert.Equal(t, 5, s.Total)
	assert.True(t, s.Attacked())
	assert.Equal(t, "10.0.0.9", s.Sources[0].Name)
	assert.Equal(t, 3, s.Sources[0].Count)
	assert.Equal(t, []string{SecLoginFailed, SecProbe}, s.Sources[0].Kinds)
	assert.Equal(t, "/.env", s.Paths[0].Name)
	assert.Len(t, s.Buckets, 5)
	assert.Equal(t, []int{0, 2, 0, 2, 1}, []int{s.Buckets[0].Count, s.Buckets[1].Count, s.Buckets[2].Count, s.Buckets[3].Count, s.Buckets[4].Count})
	assert.Equal(t, SecRateLimit, s.Recent[0].Kind, "newest first")

	quiet := summarizeSecurity(events[:2], since, since.Add(4*time.Hour), time.Hour, "15")
	assert.False(t, quiet.Attacked(), "two failed sign-ins are background noise")
}

func TestSecurityAlertDue(t *testing.T) {
	assert.False(t, securityAlertDue(SecuritySummary{Total: 5, ByKind: map[string]int{SecLoginFailed: 5}}))
	assert.True(t, securityAlertDue(SecuritySummary{Total: 1, ByKind: map[string]int{SecBan: 1}}))
	assert.True(t, securityAlertDue(SecuritySummary{Total: securityAlertLogins, ByKind: map[string]int{SecLoginFailed: securityAlertLogins}}))
	assert.True(t, securityAlertDue(SecuritySummary{Total: securityAlertTotal, ByKind: map[string]int{SecRateLimit: securityAlertTotal}}))
}
