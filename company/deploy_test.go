// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func pathSet(paths ...string) map[string]bool {
	m := make(map[string]bool, len(paths))
	for _, p := range paths {
		m[p] = true
	}
	return m
}

// The absence of these deletes was a real bug: a file the staff removed was
// shown as "Removed" in the deploy preview and then silently survived the
// merge, so central kept serving code nobody could see any more.
func TestDeletePathsFor(t *testing.T) {
	t.Run("first-ever deploy has nothing live, so no deletes", func(t *testing.T) {
		assert.Empty(t, deletePathsFor(pathSet("main.py", "lib/a.py"), pathSet()))
	})

	t.Run("unchanged tree produces no deletes", func(t *testing.T) {
		files := pathSet("main.py", "lib/a.py")
		assert.Empty(t, deletePathsFor(files, files))
	})

	t.Run("removed files are deleted", func(t *testing.T) {
		got := deletePathsFor(pathSet("main.py"), pathSet("main.py", "old.py", "lib/gone.py"))
		assert.Equal(t, []string{"lib/gone.py", "old.py"}, got)
	})

	t.Run("deleting every file still yields deletes — this is how a department un-deploys", func(t *testing.T) {
		got := deletePathsFor(pathSet(), pathSet("main.py", "lib/a.py"))
		assert.Equal(t, []string{"lib/a.py", "main.py"}, got)
	})

	t.Run("rename deletes only the old path", func(t *testing.T) {
		got := deletePathsFor(pathSet("new.py"), pathSet("old.py"))
		assert.Equal(t, []string{"old.py"}, got)
	})

	t.Run("a path that became a directory deletes the old file", func(t *testing.T) {
		// central has file "a"; the department replaced it with a dir "a/b"
		got := deletePathsFor(pathSet("a/b"), pathSet("a"))
		assert.Equal(t, []string{"a"}, got)
	})

	t.Run("output is sorted so commit contents are deterministic", func(t *testing.T) {
		live := pathSet("z.py", "a.py", "m.py")
		assert.Equal(t, []string{"a.py", "m.py", "z.py"}, deletePathsFor(pathSet(), live))
	})
}

// row is a plain-value mirror of deploySplitRow for test assertions —
// comparing template.HTML content directly is brittle against
// modules/highlight's exact markup, so these only check line numbers/types.
type row struct {
	oldNum, newNum   int
	oldType, newType string
}

func rows(splitRows []deploySplitRow) []row {
	out := make([]row, len(splitRows))
	for i, r := range splitRows {
		out[i] = row{r.OldNum, r.NewNum, r.OldType, r.NewType}
	}
	return out
}

func TestDiffPreviewSplitRows(t *testing.T) {
	t.Run("substitution pairs old and new lines side by side", func(t *testing.T) {
		got := diffPreviewSplitRows("f.txt", []byte("a\nb\nc\n"), []byte("a\nX\nc\n"))
		assert.Equal(t, []row{
			{1, 1, "context", "context"},
			{2, 2, "del", "add"},
			{3, 3, "context", "context"},
		}, rows(got))
	})

	t.Run("pure addition leaves the old side blank", func(t *testing.T) {
		got := diffPreviewSplitRows("f.txt", []byte("a\nb\n"), []byte("a\nb\nc\n"))
		assert.Equal(t, []row{
			{1, 1, "context", "context"},
			{2, 2, "context", "context"},
			{0, 3, "", "add"},
		}, rows(got))
	})

	t.Run("pure deletion leaves the new side blank", func(t *testing.T) {
		got := diffPreviewSplitRows("f.txt", []byte("a\nb\nc\n"), []byte("a\nc\n"))
		assert.Equal(t, []row{
			{1, 1, "context", "context"},
			{2, 0, "del", ""},
			{3, 2, "context", "context"},
		}, rows(got))
	})

	t.Run("uneven substitution pads the shorter side", func(t *testing.T) {
		got := diffPreviewSplitRows("f.txt", []byte("a\nb\nc\n"), []byte("a\nX\nY\nZ\nc\n"))
		assert.Equal(t, []row{
			{1, 1, "context", "context"},
			{2, 2, "del", "add"},
			{0, 3, "", "add"},
			{0, 4, "", "add"},
			{3, 5, "context", "context"},
		}, rows(got))
	})
}

func contextRows(n int) []deploySplitRow {
	rows := make([]deploySplitRow, n)
	for i := range rows {
		rows[i] = deploySplitRow{OldNum: i + 1, OldType: "context", NewNum: i + 1, NewType: "context"}
	}
	return rows
}

func segmentSizes(segs []deployDiffSegment) (sizes []int, collapsed []bool) {
	for _, s := range segs {
		sizes = append(sizes, len(s.Rows))
		collapsed = append(collapsed, s.Collapsed)
	}
	return sizes, collapsed
}

func TestGroupDiffSegments(t *testing.T) {
	t.Run("short file with a change never collapses", func(t *testing.T) {
		rows := contextRows(5)
		rows[2] = deploySplitRow{OldNum: 3, OldType: "del"}
		segs := groupDiffSegments(rows)
		assert.Len(t, segs, 1)
		assert.False(t, segs[0].Collapsed)
		assert.Equal(t, rows, segs[0].Rows)
	})

	t.Run("a long context run between changes collapses, keeping the margin visible", func(t *testing.T) {
		rows := contextRows(20)
		rows[0] = deploySplitRow{OldNum: 1, OldType: "del"}
		rows[19] = deploySplitRow{NewNum: 20, NewType: "add"}
		sizes, collapsed := segmentSizes(groupDiffSegments(rows))
		// margin=3: rows[0:4] (change+3 after) stay visible, rows[16:20] (3
		// before+change) stay visible, the 12 rows between collapse as one.
		assert.Equal(t, []int{4, 12, 4}, sizes)
		assert.Equal(t, []bool{false, true, false}, collapsed)
	})

	t.Run("a short context gap between two changes stays uncollapsed", func(t *testing.T) {
		rows := contextRows(10)
		rows[0] = deploySplitRow{OldNum: 1, OldType: "del"}
		rows[9] = deploySplitRow{NewNum: 10, NewType: "add"}
		_, collapsed := segmentSizes(groupDiffSegments(rows))
		// margin=3 reaches in from both changes, leaving only a 2-row gap in
		// the middle — below deployDiffCollapseMin, so nothing collapses.
		for _, c := range collapsed {
			assert.False(t, c)
		}
	})
}
