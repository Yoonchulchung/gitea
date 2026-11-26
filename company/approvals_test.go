// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApprovalStatusKeyIsAlwaysOurs(t *testing.T) {
	for _, s := range []ApprovalStatus{ApprovalPending, ApprovalApproved, ApprovalRejected, ApprovalCancelled, 99, -1} {
		assert.True(t, strings.HasPrefix(s.Key(), "company.approvals.status_"), "status %d", s)
	}
}

func TestParseProjectReviewVerdict(t *testing.T) {
	v, ok := parseProjectReviewVerdict("Sure.\n{\"approve\": true, \"reason\": \"업무용 출장 신청 도구입니다\"}")
	assert.True(t, ok)
	assert.True(t, v.Approve)
	assert.Equal(t, "업무용 출장 신청 도구입니다", v.Reason)

	_, ok = parseProjectReviewVerdict(`{"approve": true}`)
	assert.False(t, ok, "no reason, no approval")
	_, ok = parseProjectReviewVerdict(`{"reason": "x"}`)
	assert.False(t, ok, "no decision, no approval")
	_, ok = parseProjectReviewVerdict("approve")
	assert.False(t, ok)
}
