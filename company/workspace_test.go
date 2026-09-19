// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	files_service "gitea.dev/services/repository/files"

	"github.com/stretchr/testify/assert"
)

// One save is one commit, and its message says what happened in words a
// department reads back later: a rename within a folder is a rename.
func TestCommitMessageNamesRenamesAndMoves(t *testing.T) {
	rename := &files_service.ChangeRepoFile{Operation: "rename", FromTreePath: "docs/a.md", TreePath: "docs/b.md"}
	upload := &files_service.ChangeRepoFile{Operation: "upload", TreePath: "docs/b.md"}
	assert.Equal(t, "docs/a.md renamed to b.md", commitMessageFor([]*files_service.ChangeRepoFile{rename, upload}, 1, 0, 1))

	move := &files_service.ChangeRepoFile{Operation: "rename", FromTreePath: "a.md", TreePath: "docs/a.md"}
	assert.Equal(t, "a.md moved to docs/a.md, x.txt deleted", commitMessageFor([]*files_service.ChangeRepoFile{
		move,
		{Operation: "upload", TreePath: "docs/a.md"},
		{Operation: "delete", TreePath: "x.txt"},
	}, 1, 1, 1))
	assert.Equal(t, "3 files moved", commitMessageFor(nil, 3, 0, 3))
}
