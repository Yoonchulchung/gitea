// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.dev/modules/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/korean"
)

// Nothing typed on the page reaches the engine as a statement: names are
// checked, values are parameters.
func TestDataStatementsAreBuiltFromCheckedNamesAndParameters(t *testing.T) {
	st, err := insertStatement("records", []string{"name", "hours"}, []any{"홍길동", "3.5"})
	require.NoError(t, err)
	assert.Equal(t, `INSERT INTO "records" ("name", "hours") VALUES (?, ?)`, st.SQL)
	assert.Equal(t, []any{"홍길동", "3.5"}, st.Params)

	st, err = updateStatement("records", 7, []string{"name"}, []any{nil})
	require.NoError(t, err)
	assert.Equal(t, `UPDATE "records" SET "name" = ? WHERE rowid = ?`, st.SQL)
	assert.Equal(t, []any{nil, int64(7)}, st.Params)

	for _, bad := range []string{`records"; DROP TABLE x; --`, "1st", "", "a b", strings.Repeat("a", 65)} {
		_, err := insertStatement(bad, nil, nil)
		assert.Error(t, err, bad)
	}
	_, err = addColumnStatement("records", "note", "BLOB", "")
	assert.Error(t, err, "only the offered types")

	st, err = addColumnStatement("records", "note", "text", "n/a's")
	require.NoError(t, err)
	assert.Equal(t, `ALTER TABLE "records" ADD COLUMN "note" TEXT DEFAULT 'n/a''s'`, st.SQL)

	st, err = createTableStatement("visits", []newColumn{{Name: "who", Type: "TEXT", Required: true}, {Name: "count", Type: "INTEGER"}})
	require.NoError(t, err)
	assert.Equal(t, `CREATE TABLE "visits" ("id" INTEGER PRIMARY KEY AUTOINCREMENT, "who" TEXT NOT NULL, "count" INTEGER)`, st.SQL)
}

// An empty field is no value; a required column with no value is refused
// by name before anything runs; the id SQLite assigns is never asked for.
func TestRowValues(t *testing.T) {
	schema := []BrowseColumn{
		{Name: "id", Type: "INTEGER", PK: true, Auto: true},
		{Name: "name", Type: "TEXT", NotNull: true},
		{Name: "note", Type: "TEXT"},
	}
	form := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	cols, params, err := rowValues(form(map[string]string{"col_id": "", "col_name": "a", "col_note": ""}), schema)
	require.NoError(t, err)
	assert.Equal(t, []string{"name", "note"}, cols)
	assert.Equal(t, []any{"a", nil}, params)

	_, _, err = rowValues(form(map[string]string{"col_name": ""}), schema)
	assert.ErrorContains(t, err, "")
	assert.Equal(t, "company.err.data_required", err.(*userError).staffKey) //nolint:forcetypeassert // the test asserts the type
}

// A file saved by Excel on a Korean Windows is in the system code page and
// has a BOM when it is not; a spreadsheet's empty cells and a stray blank
// line are not rows; its header is matched to the table's columns by name.
func TestCSVImportReadsWhatASpreadsheetSaves(t *testing.T) {
	eucKR, err := korean.EUCKR.NewEncoder().Bytes([]byte("이름,시간\n홍길동,3\n\n,\n"))
	require.NoError(t, err)
	header, rows, err := csvRows(strings.NewReader(string(eucKR)))
	require.NoError(t, err)
	assert.Equal(t, []string{"이름", "시간"}, header)
	assert.Equal(t, [][]string{{"홍길동", "3"}}, rows)

	header, rows, err = csvRows(strings.NewReader("\xEF\xBB\xBFName, hours\nkim,'=1+1\n"))
	require.NoError(t, err)
	schema := []BrowseColumn{{Name: "name"}, {Name: "hours"}}
	statements, err := importStatements("t", schema, header, rows, true)
	require.NoError(t, err)
	require.Len(t, statements, 2)
	assert.Equal(t, `DELETE FROM "t"`, statements[0].SQL)
	assert.Equal(t, `INSERT INTO "t" ("name", "hours") VALUES (?, ?)`, statements[1].SQL)
	assert.Equal(t, []any{"kim", "=1+1"}, statements[1].Params, "Excel's own text marker is dropped, as Excel drops it")

	_, err = importStatements("t", schema, []string{"name", "nope"}, rows, false)
	assert.Error(t, err, "an unknown column refuses the whole file")
}

// The helper's edit mode really is one transaction, and its browse mode
// hands back the rowids the page edits by. Run against the system Python,
// outside any sandbox — the script is what is being tested.
func TestDataHelperEditsAndAddressesRows(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "app.db")
	run := func(payload map[string]any) dataToolResult {
		t.Helper()
		payload["db"] = db
		payload["snapshotDir"] = filepath.Join(dir, "snapshots")
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		file := filepath.Join(dir, "payload.json")
		require.NoError(t, os.WriteFile(file, body, 0o600))
		cmd := exec.CommandContext(context.Background(), python, "-", file)
		cmd.Stdin = strings.NewReader(dataToolScript)
		out, err := cmd.Output()
		require.NoError(t, err)
		var result dataToolResult
		require.NoError(t, json.Unmarshal(out, &result))
		return result
	}
	require.NoError(t, os.WriteFile(db, nil, 0o600)) // the helper refuses an absent database

	create, err := createTableStatement("t", []newColumn{{Name: "name", Type: "TEXT", Required: true}})
	require.NoError(t, err)
	insert, err := insertStatement("t", []string{"name"}, []any{"a"})
	require.NoError(t, err)
	result := run(map[string]any{"mode": "edit", "statements": []dataStatement{create, insert, insert}})
	require.True(t, result.OK, result.Error)
	assert.Equal(t, 2, result.Changed)

	bad, err := insertStatement("t", []string{"name"}, []any{nil})
	require.NoError(t, err)
	result = run(map[string]any{"mode": "edit", "statements": []dataStatement{insert, bad}})
	assert.False(t, result.OK)
	assert.Contains(t, result.Error, "NOT NULL")

	result = run(map[string]any{"mode": "browse", "table": "t", "limit": 10})
	require.True(t, result.OK, result.Error)
	assert.Equal(t, []string{"id", "name"}, result.Columns)
	assert.Len(t, result.Cells, 2, "the failed transaction added nothing")
	assert.Equal(t, []int64{1, 2}, result.RowIDs)

	del, err := deleteStatement("t", 1)
	require.NoError(t, err)
	require.True(t, run(map[string]any{"mode": "edit", "statements": []dataStatement{del}}).OK)
	result = run(map[string]any{"mode": "dump", "table": "t"})
	require.True(t, result.OK, result.Error)
	assert.Equal(t, [][]string{{"2", "a"}}, result.Cells)
}

// The data page's markup, rendered with the shapes the handlers put in
// ctx.Data. A field that does not exist is a 500 in front of a department —
// the open-request panel once did exactly that — and nothing but rendering
// finds it.
func TestDataPageTemplatesRenderWhatTheHandlerSets(t *testing.T) {
	funcs := template.FuncMap{
		"ctx":            func() map[string]any { return map[string]any{"Locale": stubLocale{}} },
		"svg":            func(string, ...any) template.HTML { return "<svg/>" },
		"FormatByteSize": func(int64) string { return "1 KiB" },
		"DateUtils":      func() stubDateUtils { return stubDateUtils{} },
		"Eval": func(args ...any) any {
			a, _ := args[0].(int)
			b, _ := args[2].(int)
			if args[1] == "-" {
				return a - b
			}
			return a + b
		},
	}
	tmpl, err := template.New("root").Funcs(funcs).ParseFiles(
		"../custom/templates/company/data_browse.tmpl",
		"../custom/templates/company/app_data.tmpl",
	)
	require.NoError(t, err)

	data := map[string]any{
		"Editable": true, "DataLink": "/PO/app/_app/data", "BrowseTable": "records", "BrowsePageSize": 50,
		"CsrfTokenHtml": template.HTML(""),
		"Browse": &BrowseResult{
			Tables:  []BrowseTable{{Name: "records", Rows: 2}},
			Table:   "records",
			Columns: []string{"id", "name"},
			Rows:    [][]string{{"1", "a"}, {"2", "b"}},
			RowIDs:  []int64{1, 2},
			Schema:  markAutoColumns([]BrowseColumn{{Name: "id", Type: "INTEGER", PK: true}, {Name: "name", Type: "TEXT", NotNull: true}}),
		},
		"EditRowID": int64(2), "EditRow": map[string]string{"id": "2", "name": "b"},
		"ColumnTypes": []string{"TEXT", "INTEGER", "REAL"}, "NewColumnRows": []int{0, 1},
		"Snapshots":     []Snapshot{{Name: "1-before-edit.db", Bytes: 4096, At: time.Now()}},
		"RecentChanges": []DataChange{{At: time.Now(), Actor: "kim", Label: "company.data.change.updated", Table: "records", Detail: "2", Changed: 1}},
	}
	var out strings.Builder
	require.NoError(t, tmpl.ExecuteTemplate(&out, "data_browse.tmpl", data))
	page := out.String()
	for _, want := range []string{
		`action="/PO/app/_app/data/insert"`, `action="/PO/app/_app/data/update"`, `name="rowid" value="2"`,
		`/PO/app/_app/data/csv?table=records`, `action="/PO/app/_app/data/create-table"`, `edit=1`,
	} {
		assert.Contains(t, page, want)
	}
	out.Reset()
	require.NoError(t, tmpl.ExecuteTemplate(&out, "company/data_history", data))
	assert.Contains(t, out.String(), `action="/PO/app/_app/data/restore"`)
	assert.Contains(t, out.String(), "company.data.change.updated")
}

type stubLocale struct{}

func (stubLocale) Tr(key string, _ ...any) template.HTML {
	return template.HTML(template.HTMLEscapeString(key))
}
func (stubLocale) HasKey(string) bool { return true }

type stubDateUtils struct{}

func (stubDateUtils) FullTime(any) template.HTML      { return "now" }
func (stubDateUtils) TimeSince(any) template.HTML     { return "now" }
func (stubDateUtils) AbsoluteShort(any) template.HTML { return "now" }
