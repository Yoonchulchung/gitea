// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gitea.dev/modules/log"
	gitea_context "gitea.dev/services/context"

	"golang.org/x/text/encoding/korean"
)

// Changing an app's data from the platform itself.
//
// The data page used to be read-only for a department: anything that changed
// data was an administrator's SQL console, switched off by default. That put
// the ordinary cases — a row typed in wrong, a list that lives in a
// spreadsheet, a column the next version needs — one ticket away from the
// people whose data it is, and left them to work around the platform with a
// script when nobody answered. None of these people write SQL, and the
// platform should not ask them to.
//
// So the page offers the operations a spreadsheet offers, and builds the SQL
// itself: every name is checked against the schema, every value is a bound
// parameter, and nothing typed on the page reaches the engine as a
// statement. The safety story is the same one the SQL console has — a copy
// of the data is taken before a change — made automatic: once in any
// ten-minute stretch, so a row-by-row session does not fill the snapshot
// list, and never skipped. Every change is written to a log the page shows,
// because "who changed this" is the question that follows "this is wrong".

const (
	editSnapshotEvery = 10 * time.Minute
	editSnapshotLabel = "before-edit"
	importMaxBytes    = 20 << 20
	importMaxRows     = 50000
	dataAuditName     = "data.log"
	dataAuditShown    = 20
)

// identifierPattern is what a table or column may be called from the page.
// SQLite accepts nearly anything quoted; this refuses anything that a
// person could not read back, and anything a quoted name could be smuggled
// through.
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func quoteIdent(name string) (string, error) {
	if !identifierPattern.MatchString(name) {
		return "", userKeyError("company.err.data_bad_name", name)
	}
	return `"` + name + `"`, nil
}

// columnTypes are the types the page offers. Three, because they are the
// three a non-developer can tell apart, and SQLite stores anything else as
// one of them anyway.
var columnTypes = map[string]bool{"TEXT": true, "INTEGER": true, "REAL": true}

type dataStatement struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params"`
}

// runDataEdit applies statements the page built, after the safety copy.
func runDataEdit(ctx context.Context, owner, repo string, statements []dataStatement) (int, error) {
	if err := ensureEditSnapshot(ctx, owner, repo); err != nil {
		return 0, err
	}
	result, err := runDataTool(ctx, owner, repo, map[string]any{"mode": "edit", "statements": statements})
	if err != nil {
		if result != nil && result.Error != "" {
			// "NOT NULL constraint failed: records.name" is the one useful
			// sentence, whichever audience reads it. Paths are the helper's
			// own and are removed.
			return 0, userErrorf("%s", RedactServerPaths(result.Error))
		}
		return 0, err
	}
	return result.Changed, nil
}

// ensureEditSnapshot takes the copy a change is undone from, unless one was
// taken in the last few minutes. Works while the app is running: a snapshot
// is a consistent point in the write stream (company/appdatatool.go).
func ensureEditSnapshot(ctx context.Context, owner, repo string) error {
	dataDir, ok := AppDataDirIfPresent(ctx, owner, repo)
	if !ok {
		return userKeyError("company.err.data_none")
	}
	for _, s := range ListSnapshots(dataDir) {
		if time.Since(s.At) < editSnapshotEvery {
			return nil
		}
	}
	if _, err := CreateSnapshot(ctx, owner, repo, editSnapshotLabel); err != nil {
		return fmt.Errorf("could not save a copy of the data before changing it: %w", err)
	}
	return nil
}

// tableSchema is the columns of one table, or an error naming the table.
func tableSchema(ctx context.Context, owner, repo, table string) ([]BrowseColumn, error) {
	if _, err := quoteIdent(table); err != nil {
		return nil, err
	}
	result, err := runDataTool(ctx, owner, repo, map[string]any{"mode": "browse", "table": table, "limit": 1})
	if err != nil {
		return nil, userKeyError("company.err.data_no_table", table)
	}
	return markAutoColumns(result.Schema), nil
}

// markAutoColumns flags the column SQLite fills in by itself, so the form
// can say so instead of asking for it.
func markAutoColumns(schema []BrowseColumn) []BrowseColumn {
	for i := range schema {
		schema[i].Auto = schema[i].PK && strings.EqualFold(schema[i].Type, "INTEGER")
	}
	return schema
}

// rowValues reads one row's worth of fields from a form: the columns it
// named, and a value for each, with an empty field meaning no value.
func rowValues(form func(string) (string, bool), schema []BrowseColumn) (columns []string, params []any, err error) {
	for _, c := range schema {
		v, present := form("col_" + c.Name)
		if !present {
			continue
		}
		if v == "" {
			if c.NotNull && c.Default == "" && !c.Auto {
				return nil, nil, userKeyError("company.err.data_required", c.Name)
			}
			if c.Auto {
				continue // SQLite assigns it
			}
			columns = append(columns, c.Name)
			params = append(params, nil)
			continue
		}
		columns = append(columns, c.Name)
		params = append(params, v)
	}
	return columns, params, nil
}

func insertStatement(table string, columns []string, params []any) (dataStatement, error) {
	qt, err := quoteIdent(table)
	if err != nil {
		return dataStatement{}, err
	}
	if len(columns) == 0 {
		return dataStatement{SQL: "INSERT INTO " + qt + " DEFAULT VALUES"}, nil
	}
	names := make([]string, len(columns))
	marks := make([]string, len(columns))
	for i, c := range columns {
		if names[i], err = quoteIdent(c); err != nil {
			return dataStatement{}, err
		}
		marks[i] = "?"
	}
	return dataStatement{
		SQL:    "INSERT INTO " + qt + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")",
		Params: params,
	}, nil
}

func updateStatement(table string, rowid int64, columns []string, params []any) (dataStatement, error) {
	qt, err := quoteIdent(table)
	if err != nil {
		return dataStatement{}, err
	}
	if len(columns) == 0 {
		return dataStatement{}, userKeyError("company.err.data_nothing_to_save")
	}
	sets := make([]string, len(columns))
	for i, c := range columns {
		q, err := quoteIdent(c)
		if err != nil {
			return dataStatement{}, err
		}
		sets[i] = q + " = ?"
	}
	return dataStatement{
		SQL:    "UPDATE " + qt + " SET " + strings.Join(sets, ", ") + " WHERE rowid = ?",
		Params: append(params, rowid),
	}, nil
}

func deleteStatement(table string, rowid int64) (dataStatement, error) {
	qt, err := quoteIdent(table)
	if err != nil {
		return dataStatement{}, err
	}
	return dataStatement{SQL: "DELETE FROM " + qt + " WHERE rowid = ?", Params: []any{rowid}}, nil
}

// sqlLiteral renders a default value into DDL, which cannot take a parameter.
func sqlLiteral(typ, value string) string {
	if typ != "TEXT" {
		if _, err := strconv.ParseFloat(value, 64); err == nil {
			return value
		}
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func addColumnStatement(table, column, typ, def string) (dataStatement, error) {
	qt, err := quoteIdent(table)
	if err != nil {
		return dataStatement{}, err
	}
	qc, err := quoteIdent(column)
	if err != nil {
		return dataStatement{}, err
	}
	typ = strings.ToUpper(strings.TrimSpace(typ))
	if !columnTypes[typ] {
		return dataStatement{}, userKeyError("company.err.data_bad_type")
	}
	sql := "ALTER TABLE " + qt + " ADD COLUMN " + qc + " " + typ
	if def != "" {
		sql += " DEFAULT " + sqlLiteral(typ, def)
	}
	return dataStatement{SQL: sql}, nil
}

// newColumn is one column of a table made from the page.
type newColumn struct {
	Name, Type string
	Required   bool
}

func createTableStatement(table string, columns []newColumn) (dataStatement, error) {
	qt, err := quoteIdent(table)
	if err != nil {
		return dataStatement{}, err
	}
	if len(columns) == 0 {
		return dataStatement{}, userKeyError("company.err.data_no_columns")
	}
	// An id every row has, so the page can address rows and the app has a
	// key it did not have to think about.
	defs := []string{`"id" INTEGER PRIMARY KEY AUTOINCREMENT`}
	for _, c := range columns {
		q, err := quoteIdent(c.Name)
		if err != nil {
			return dataStatement{}, err
		}
		if strings.EqualFold(c.Name, "id") {
			continue
		}
		typ := strings.ToUpper(strings.TrimSpace(c.Type))
		if !columnTypes[typ] {
			return dataStatement{}, userKeyError("company.err.data_bad_type")
		}
		def := q + " " + typ
		if c.Required {
			def += " NOT NULL"
		}
		defs = append(defs, def)
	}
	return dataStatement{SQL: "CREATE TABLE " + qt + " (" + strings.Join(defs, ", ") + ")"}, nil
}

// decodeSpreadsheetText reads a file as a spreadsheet saved it. Excel on a
// Korean Windows saves "CSV" in the system code page unless "CSV UTF-8" was
// chosen, and a person who does not know the difference should not have to.
func decodeSpreadsheetText(raw []byte) string {
	raw = bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF"))
	if utf8.Valid(raw) {
		return string(raw)
	}
	if decoded, err := korean.EUCKR.NewDecoder().Bytes(raw); err == nil {
		return string(decoded)
	}
	return string(raw)
}

// csvRows parses an uploaded file into a header and its rows.
func csvRows(r io.Reader) ([]string, [][]string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, importMaxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > importMaxBytes {
		return nil, nil, userKeyError("company.err.csv_too_big", importMaxBytes>>20)
	}
	reader := csv.NewReader(strings.NewReader(decodeSpreadsheetText(raw)))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, nil, userKeyError("company.err.csv_unreadable")
	}
	var rows [][]string
	for _, rec := range records {
		empty := true
		for _, v := range rec {
			if strings.TrimSpace(v) != "" {
				empty = false
				break
			}
		}
		if !empty {
			rows = append(rows, rec)
		}
	}
	if len(rows) < 2 {
		return nil, nil, userKeyError("company.err.csv_empty")
	}
	if len(rows)-1 > importMaxRows {
		return nil, nil, userKeyError("company.err.csv_too_many_rows", importMaxRows)
	}
	header := rows[0]
	for i := range header {
		header[i] = strings.TrimSpace(header[i])
	}
	return header, rows[1:], nil
}

// importStatements turns a spreadsheet's rows into inserts, matching its
// header to the table's columns by name.
func importStatements(table string, schema []BrowseColumn, header []string, rows [][]string, replace bool) ([]dataStatement, error) {
	qt, err := quoteIdent(table)
	if err != nil {
		return nil, err
	}
	columns := make([]string, len(header))
	for i, h := range header {
		found := ""
		for _, c := range schema {
			if strings.EqualFold(c.Name, h) {
				found = c.Name
				break
			}
		}
		if found == "" {
			return nil, userKeyError("company.err.csv_unknown_column", h)
		}
		columns[i] = found
	}
	var out []dataStatement
	if replace {
		out = append(out, dataStatement{SQL: "DELETE FROM " + qt})
	}
	for _, row := range rows {
		var cols []string
		var params []any
		for i, c := range columns {
			if i >= len(row) {
				break
			}
			v := strings.TrimSpace(row[i])
			if v == "" {
				continue // no value, as an empty cell means in a spreadsheet
			}
			// Excel's own convention for text that looks like a formula, and
			// what the download wrote to keep a formula out of a spreadsheet.
			if len(v) > 1 && v[0] == '\'' && strings.ContainsRune("=+-@", rune(v[1])) {
				v = v[1:]
			}
			cols = append(cols, c)
			params = append(params, v)
		}
		st, err := insertStatement(table, cols, params)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// writeTableCSV sends one table as a file a spreadsheet opens, and that
// importCSV takes back.
func writeTableCSV(ctx context.Context, owner, repo, table string, w io.Writer) error {
	if _, err := quoteIdent(table); err != nil {
		return err
	}
	result, err := runDataTool(ctx, owner, repo, map[string]any{"mode": "dump", "table": table})
	if err != nil {
		return err
	}
	// The mark is what makes Excel read the file as UTF-8.
	if _, err := w.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(result.Columns); err != nil {
		return err
	}
	for _, row := range result.Cells {
		for i, v := range row {
			// A cell starting with = + - @ is a formula to a spreadsheet.
			if len(v) > 0 && strings.ContainsRune("=+-@", rune(v[0])) {
				row[i] = "'" + v
			}
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// restoreForDepartment puts a copy back on the department's behalf: the app
// is stopped for the moment the file is replaced and started again after.
// An administrator's console asks them to stop it first; a department
// should not have to know why.
func restoreForDepartment(ctx context.Context, owner, repo, name, actor string) error {
	wasRunning := IsAppRunning(owner, repo)
	if wasRunning {
		if err := StopApp(owner, repo, actor, false); err != nil {
			return err
		}
	}
	err := RestoreSnapshot(ctx, owner, repo, name, actor)
	if wasRunning {
		if startErr := startAppAs(owner, repo, actor, false); startErr != nil && err == nil {
			err = startErr
		}
	}
	return err
}

// DataChange is one line of the data log: who did what to which table.
type DataChange struct {
	At      time.Time
	Actor   string
	Label   string // locale key; the kind of change
	Table   string
	Detail  string
	Changed int
}

var dataChangeLabels = map[string]string{
	"added": "company.data.change.added", "updated": "company.data.change.updated",
	"deleted": "company.data.change.deleted", "imported": "company.data.change.imported",
	"replaced": "company.data.change.replaced", "column": "company.data.change.column",
	"table": "company.data.change.table", "restored": "company.data.change.restored",
}

// appendDataAudit records one change. Tab-separated: table names may hold
// spaces, and the file is read back by this package.
func appendDataAudit(owner, repo, actor, kind, table, detail string, changed int) {
	p := appPathsFor(owner, repo)
	if err := os.MkdirAll(p.logs, 0o700); err != nil {
		return
	}
	f, err := openRotatingLog(p.logs, dataAuditName)
	if err != nil {
		log.Warn("company: data log for %s/%s: %v", owner, repo, err)
		return
	}
	defer func() { _ = f.Close() }()
	clean := func(s string) string { return strings.NewReplacer("\t", " ", "\n", " ").Replace(s) }
	_, _ = fmt.Fprintf(f, "%s\t%s\t%s\t%s\t%s\t%d\n",
		time.Now().UTC().Format(time.RFC3339), clean(actor), kind, clean(table), clean(detail), changed)
}

// recentDataChanges is the newest entries of the data log, newest first.
func recentDataChanges(owner, repo string, limit int) []DataChange {
	f, err := os.Open(filepath.Join(appPathsFor(owner, repo).logs, dataAuditName))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []DataChange
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 6 {
			continue
		}
		at, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}
		label, ok := dataChangeLabels[fields[2]]
		if !ok {
			continue
		}
		changed, _ := strconv.Atoi(fields[5])
		out = append(out, DataChange{At: at, Actor: fields[1], Label: label, Table: fields[3], Detail: fields[4], Changed: changed})
		if len(out) > limit {
			out = out[1:]
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// dataEditAction performs one of the page's changes. Shared by a
// department's page and the admin console, which differ only in how a
// restore is done and how an error is worded.
func dataEditAction(ctx *gitea_context.Context, owner, repo, actor, verb string, admin bool) (note string, err error) {
	table := ctx.FormString("table")
	form := func(name string) (string, bool) {
		if _, ok := ctx.Req.Form[name]; !ok {
			return "", false
		}
		return ctx.FormString(name), true
	}
	tr := ctx.Locale.TrString

	switch verb {
	case "insert":
		schema, err := tableSchema(ctx, owner, repo, table)
		if err != nil {
			return "", err
		}
		columns, params, err := rowValues(form, schema)
		if err != nil {
			return "", err
		}
		st, err := insertStatement(table, columns, params)
		if err != nil {
			return "", err
		}
		if _, err := runDataEdit(ctx, owner, repo, []dataStatement{st}); err != nil {
			return "", err
		}
		appendDataAudit(owner, repo, actor, "added", table, "", 1)
		return tr("company.data.flash_row_added"), nil

	case "update", "delete":
		rowid := ctx.FormInt64("rowid")
		if rowid <= 0 {
			return "", userKeyError("company.err.data_no_rowid")
		}
		st, err := rowStatement(ctx, owner, repo, table, rowid, verb == "delete", form)
		if err != nil {
			return "", err
		}
		changed, err := runDataEdit(ctx, owner, repo, []dataStatement{st})
		if err != nil {
			return "", err
		}
		appendDataAudit(owner, repo, actor, verb+"d", table, strconv.FormatInt(rowid, 10), changed)
		return tr("company.data.flash_row_" + verb + "d"), nil

	case "import":
		file, _, err := ctx.Req.FormFile("csv")
		if err != nil {
			return "", userKeyError("company.err.csv_missing")
		}
		defer func() { _ = file.Close() }()
		schema, err := tableSchema(ctx, owner, repo, table)
		if err != nil {
			return "", err
		}
		header, rows, err := csvRows(file)
		if err != nil {
			return "", err
		}
		replace := ctx.FormString("mode") == "replace"
		statements, err := importStatements(table, schema, header, rows, replace)
		if err != nil {
			return "", err
		}
		if _, err := runDataEdit(ctx, owner, repo, statements); err != nil {
			return "", err
		}
		kind := "imported"
		if replace {
			kind = "replaced"
		}
		appendDataAudit(owner, repo, actor, kind, table, strconv.Itoa(len(rows)), len(rows))
		return tr("company.data.flash_imported", len(rows)), nil

	case "add-column":
		column := strings.TrimSpace(ctx.FormString("column"))
		st, err := addColumnStatement(table, column, ctx.FormString("type"), strings.TrimSpace(ctx.FormString("default")))
		if err != nil {
			return "", err
		}
		if _, err := runDataEdit(ctx, owner, repo, []dataStatement{st}); err != nil {
			return "", err
		}
		appendDataAudit(owner, repo, actor, "column", table, column, 0)
		return tr("company.data.flash_column_added", column), nil

	case "create-table":
		table = strings.TrimSpace(ctx.FormString("name"))
		var columns []newColumn
		for i := range 8 {
			name := strings.TrimSpace(ctx.FormString("col_name_" + strconv.Itoa(i)))
			if name == "" {
				continue
			}
			columns = append(columns, newColumn{
				Name: name, Type: ctx.FormString("col_type_" + strconv.Itoa(i)),
				Required: ctx.FormString("col_required_"+strconv.Itoa(i)) != "",
			})
		}
		st, err := createTableStatement(table, columns)
		if err != nil {
			return "", err
		}
		if _, err := runDataEdit(ctx, owner, repo, []dataStatement{st}); err != nil {
			return "", err
		}
		appendDataAudit(owner, repo, actor, "table", table, "", 0)
		return tr("company.data.flash_table_created", table), nil

	case "snapshot":
		name, err := CreateSnapshot(ctx, owner, repo, "manual")
		if err != nil {
			return "", err
		}
		return tr("company.data.flash_snapshot", name), nil

	case "restore":
		name := ctx.FormString("name")
		if admin {
			err = RestoreSnapshot(ctx, owner, repo, name, actor)
		} else {
			err = restoreForDepartment(ctx, owner, repo, name, actor)
		}
		if err != nil {
			return "", err
		}
		appendDataAudit(owner, repo, actor, "restored", "", name, 0)
		return tr("company.data.flash_restored", name), nil
	}
	return "", userKeyError("company.err.data_unknown_action")
}

// rowStatement is the change to one existing row: its removal, or the values
// the form holds for it.
func rowStatement(ctx context.Context, owner, repo, table string, rowid int64, remove bool, form func(string) (string, bool)) (dataStatement, error) {
	if remove {
		return deleteStatement(table, rowid)
	}
	schema, err := tableSchema(ctx, owner, repo, table)
	if err != nil {
		return dataStatement{}, err
	}
	columns, params, err := rowValues(form, schema)
	if err != nil {
		return dataStatement{}, err
	}
	return updateStatement(table, rowid, columns, params)
}

// editRowValues reads one row in full, for the form that edits it.
func editRowValues(ctx context.Context, owner, repo, table string, rowid int64) map[string]string {
	if rowid <= 0 {
		return nil
	}
	result, err := runDataTool(ctx, owner, repo, map[string]any{"mode": "browse", "table": table, "rowid": rowid, "full": true})
	if err != nil || len(result.Cells) == 0 {
		return nil
	}
	values := make(map[string]string, len(result.Columns))
	for i, c := range result.Columns {
		if i < len(result.Cells[0]) {
			values[c] = result.Cells[0][i]
		}
	}
	return values
}

// setDataEditData attaches what the editing controls need to a data page.
func setDataEditData(ctx *gitea_context.Context, owner, repo, table string, snapshots []Snapshot) {
	ctx.Data["Editable"] = true
	ctx.Data["Snapshots"] = snapshots
	ctx.Data["RecentChanges"] = recentDataChanges(owner, repo, dataAuditShown)
	ctx.Data["ColumnTypes"] = []string{"TEXT", "INTEGER", "REAL"}
	ctx.Data["NewColumnRows"] = []int{0, 1, 2, 3, 4, 5, 6, 7}
	if browse, ok := ctx.Data["Browse"].(*BrowseResult); ok && browse != nil {
		browse.Schema = markAutoColumns(browse.Schema)
	}
	if rowid := ctx.FormInt64("edit"); rowid > 0 && table != "" {
		if values := editRowValues(ctx, owner, repo, table, rowid); values != nil {
			ctx.Data["EditRowID"] = rowid
			ctx.Data["EditRow"] = values
		}
	}
}

// AppDataAction handles a department's own changes to its data.
func AppDataAction(ctx *gitea_context.Context) {
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name
	note, err := dataEditAction(ctx, owner, name, ctx.Doer.Name, ctx.PathParam("verb"), false)
	if err != nil {
		ctx.Flash.Error(DepartmentSafeErrorL(ctx.Locale, "data edit on "+owner+"/"+name, err))
	} else {
		ctx.Flash.Success(note)
	}
	ctx.Redirect(dataPageLink(ctx.Repo.RepoLink+"/_app/data", ctx.FormString("table")))
}

// AppDataCSV sends one table of a department's data as a spreadsheet file.
func AppDataCSV(ctx *gitea_context.Context) {
	serveTableCSV(ctx, ctx.Repo.Owner.Name, ctx.Repo.Repository.Name)
}

func serveTableCSV(ctx *gitea_context.Context, owner, repo string) {
	table := ctx.FormString("table")
	if _, err := quoteIdent(table); err != nil {
		ctx.HTTPError(http.StatusBadRequest, "bad table name")
		return
	}
	ctx.Resp.Header().Set("Content-Type", "text/csv; charset=utf-8")
	ctx.Resp.Header().Set("Content-Disposition", `attachment; filename="`+table+`.csv"`)
	if err := writeTableCSV(ctx, owner, repo, table, ctx.Resp); err != nil {
		log.Error("company: %s/%s: csv of %s: %v", owner, repo, table, err)
	}
}

func dataPageLink(base, table string) string {
	if table == "" {
		return base
	}
	return base + "?table=" + table
}
