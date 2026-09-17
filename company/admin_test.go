// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	user_model "gitea.dev/models/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/builder"
)

// `action` is a feed table: services/feed/feed.go writes one row per reader,
// so a single repository creation is stored once for the actor, once for the
// organization and once for every watcher. Filtering only by repo_id showed
// all of them, and the same event appeared three times in a row.
func TestAdminActivityKeepsOneRowPerAction(t *testing.T) {
	sql, args, err := builder.ToSQL(oneRowPerAction())
	require.NoError(t, err)

	assert.Contains(t, sql, "user_id IN", "the feed must pick one reader's copy")
	assert.Contains(t, sql, "FROM user", "and that reader is the repository's organization")
	require.Len(t, args, 1)
	assert.EqualValues(t, user_model.UserTypeOrganization, args[0])
}
