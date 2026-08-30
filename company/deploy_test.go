// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
