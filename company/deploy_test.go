// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// The central repository can be renamed or transferred from its own settings
// page like any other. What must not happen is the platform losing it: every
// policy read and every approval resolves it by owner and name, and app.ini
// still says where it used to be.
func TestCentralRepoFollowsAMove(t *testing.T) {
	withCompanyINI(t, "CENTRAL_DEPLOY_REPO = yoonchul/central-deploy")
	centralRepoMoved.Store("")
	t.Cleanup(func() { centralRepoMoved.Store("") })

	owner, name, err := centralDeployOwnerName()
	assert.NoError(t, err)
	assert.Equal(t, "yoonchul", owner)
	assert.Equal(t, "central-deploy", name)

	// Once moved, the new location wins over app.ini.
	centralRepoMoved.Store("platform/approvals")
	owner, name, err = centralDeployOwnerName()
	assert.NoError(t, err)
	assert.Equal(t, "platform", owner)
	assert.Equal(t, "approvals", name)
}

// A notification is prose with a few names in it. The names are escaped
// because an app or a person supplies them; the markup around them is what
// the administrator wrote, and must survive.
func TestMailPlaceholdersEscapeValuesNotMarkup(t *testing.T) {
	fields := MailFields{"app": "PO/<b>report</b>", "actor": "kim & lee"}
	body := renderMailText(`<p>App: {{app}} — by {{actor}}</p>`, fields, true)
	assert.Equal(t, `<p>App: PO/&lt;b&gt;report&lt;/b&gt; — by kim &amp; lee</p>`, body)

	// A subject is not markup, so nothing is escaped into entities there.
	assert.Equal(t, "kim & lee asked", renderMailText("{{actor}} asked", fields, false))

	// An unknown name is left as it was typed rather than silently emptied —
	// the administrator can see their mistake in the message that arrives.
	assert.Equal(t, "{{nope}}", renderMailText("{{nope}}", fields, true))
}

// Addresses are typed by hand into a textarea, and people paste lists in
// every shape.
func TestSplitAddressesTakesWhateverWasPasted(t *testing.T) {
	got := SplitAddresses(" a@x.com,b@x.com\n c@x.com ;a@X.com\n\n")
	assert.Equal(t, []string{"a@x.com", "b@x.com", "c@x.com"}, got, "blank entries and repeats are dropped")
	assert.Empty(t, SplitAddresses("  \n , ; "))
}

// The message has to be a message: an HTML body declared as one, a subject a
// mail server will not mangle, and every recipient on it.
func TestBuildMessageIsWellFormedHTMLMail(t *testing.T) {
	server := MailServer{From: "platform@example.com", FromName: "사내 플랫폼"}
	msg := string(buildMessage(server, []string{"a@x.com", "b@x.com"}, "배포 요청", "<p>안녕<br>하세요</p>"))

	headers, body, found := strings.Cut(msg, "\r\n\r\n")
	require.True(t, found, "headers and body are separated by a blank line")
	assert.Equal(t, "<p>안녕<br>하세요</p>", body, "the body is the HTML as written")
	assert.Contains(t, headers, "To: a@x.com, b@x.com")
	assert.Contains(t, headers, "Content-Type: text/html; charset=UTF-8")
	// Non-ASCII in a header is encoded rather than sent raw, and the address
	// stays readable beside the encoded name.
	assert.Contains(t, headers, "<platform@example.com>")
	assert.NotContains(t, headers, "Subject: 배포 요청")
	assert.Contains(t, headers, "Subject: =?utf-8?q?")
}
