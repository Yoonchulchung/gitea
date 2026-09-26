// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The tabs label the list. Picking a department used to leave "전체 17" above
// a list of 13, because the counts were taken before the owner filter.
func TestFleetTabCountsFollowTheOwnerFilter(t *testing.T) {
	row := func(owner, repo, actual string, hasRelease bool) *adminDeployRow {
		return &adminDeployRow{State: &AppState{Owner: owner, Repo: repo, Actual: actual, HasRelease: hasRelease}}
	}
	rows := []*adminDeployRow{
		row("PO", "a", AppStateRunning, true),
		row("PO", "b", AppStateFailed, true),
		row("PO", "c", AppStateStopped, false),
		row("vv", "d", AppStateRunning, true),
		row("vv", "e", AppStateRunning, true),
	}

	all := filterDeployRows(rows, "", "")
	assert.Len(t, all, 5, "no owner means every app")

	scoped := filterDeployRows(rows, "", "PO")
	assert.Len(t, scoped, 3, "the tabs count this department's apps")

	// And the state tab narrows that further rather than starting over.
	assert.Len(t, filterDeployRows(scoped, "running", ""), 1)
	assert.Len(t, filterDeployRows(scoped, "failed", ""), 1)
	assert.Len(t, filterDeployRows(scoped, "undeployed", ""), 1)
}
