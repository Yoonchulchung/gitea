// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The sidebar names the first few packages and counts the rest; a short
// list and every other row are left alone.
func TestSidebarPackagesRowIsTrimmed(t *testing.T) {
	rows := trimPackagesRow([]PermissionRow{
		{Label: "company.perm.access", ValueText: "a, b, c, d, e, f, g"},
		{Label: "company.perm.packages", ValueText: "fastapi, uvicorn, pydantic, jinja2, requests, lxml, certifi"},
	})
	assert.Equal(t, "a, b, c, d, e, f, g", rows[0].ValueText)
	assert.Equal(t, 0, rows[0].More)
	assert.Equal(t, "fastapi, uvicorn, pydantic, jinja2, requests", rows[1].ValueText)
	assert.Equal(t, 2, rows[1].More)

	short := trimPackagesRow([]PermissionRow{{Label: "company.perm.packages", ValueText: "fastapi, uvicorn"}})
	assert.Equal(t, "fastapi, uvicorn", short[0].ValueText)
	assert.Equal(t, 0, short[0].More)
}
