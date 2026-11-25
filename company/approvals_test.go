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
