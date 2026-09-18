// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func logLines(texts ...string) []LogLine {
	out := make([]LogLine, 0, len(texts))
	for _, t := range texts {
		out = append(out, LogLine{Text: t})
	}
	return out
}

func texts(lines []LogLine) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return out
}

// The point is to shorten what a person has to read, so the ordinary noise an
// app produces has to stay out — an unanchored "error" would match a request
// path and every INFO line about an error page, and bury the real one.
func TestErrorLinesIgnoresOrdinaryOutput(t *testing.T) {
	got := ErrorLines(logLines(
		"INFO:     Started server process [1]",
		"INFO:     Uvicorn running on http://0.0.0.0:8000",
		`INFO:     127.0.0.1:0 - "GET /error_handler HTTP/1.1" 200 OK`,
		"INFO:     Application startup complete.",
	))
	assert.Empty(t, got, "nothing here is a failure")
}

// A traceback without its frames names the exception and not the place, and
// the place is the answer somebody needs.
func TestErrorLinesKeepsTracebackFrames(t *testing.T) {
	got := texts(ErrorLines(logLines(
		"INFO:     Application startup complete.",
		"Traceback (most recent call last):",
		`  File "/app/main.py", line 12, in <module>`,
		"    conn = sqlite3.connect(DB_PATH)",
		"sqlite3.OperationalError: unable to open database file",
		"INFO:     127.0.0.1:0 - \"GET / HTTP/1.1\" 500 Internal Server Error",
	)))
	require.NotEmpty(t, got)
	assert.Contains(t, got, "Traceback (most recent call last):")
	assert.Contains(t, got, `  File "/app/main.py", line 12, in <module>`)
	assert.Contains(t, got, "sqlite3.OperationalError: unable to open database file")
	assert.NotContains(t, got, "INFO:     Application startup complete.")
}

// An app in a crash loop writes the same traceback every few seconds, and
// forty copies hide everything else that happened.
func TestErrorLinesCollapsesRepeats(t *testing.T) {
	same := "ModuleNotFoundError: No module named 'openpyxl'"
	got := texts(ErrorLines(logLines(same, same, same, "ValueError: bad input", same)))
	assert.Equal(t, []string{same, "ValueError: bad input", same}, got,
		"consecutive repeats collapse; a later recurrence after something else is real")
}

// pip's failures are the other half of what breaks these apps, and they are
// the case with an action the department can actually take.
func TestErrorLinesCatchesBuildFailures(t *testing.T) {
	got := texts(ErrorLines(logLines(
		"Collecting fastapi",
		"ERROR: Could not find a version that satisfies the requirement openpyxl==99.0",
		"ERROR: No matching distribution found for openpyxl==99.0",
	)))
	assert.Len(t, got, 2)
	assert.NotContains(t, got, "Collecting fastapi")
}

// The newest failure is the one being asked about, so a very long log keeps
// its tail rather than its head.
func TestErrorLinesKeepsTheMostRecent(t *testing.T) {
	var many []string
	for i := range maxDiagnoseLines + 20 {
		many = append(many, "ValueError: failure number "+string(rune('a'+i%26))+string(rune('0'+i%10)))
	}
	got := texts(ErrorLines(logLines(many...)))
	assert.Len(t, got, maxDiagnoseLines)
	assert.Equal(t, many[len(many)-1], got[len(got)-1], "the last failure survives")
}

// A traceback names absolute paths inside this instance — the data directory,
// the release hash, the shared virtualenv. None of it helps the explanation
// and all of it describes the host, so it must not be what leaves for
// somebody else's API.
func TestDiagnosisRedactsServerPathsBeforeSending(t *testing.T) {
	// A real value, not the empty default: with AppDataPath unset the helper
	// has nothing to strip and the assertion would pass while proving nothing.
	prev := setting.AppDataPath
	setting.AppDataPath = "/srv/gitea/data"
	t.Cleanup(func() { setting.AppDataPath = prev })

	line := `  File "/srv/gitea/data/company-apps/96462367/releases/f542fe/app/main.py", line 12`
	redacted := RedactServerPaths(line)

	assert.NotContains(t, redacted, "/srv/gitea/data", "the instance's own path must not travel")
	assert.Contains(t, redacted, "main.py", "the part that identifies the file still has to survive")
}

// The shape the server actually produced: uvicorn failing to import an app
// whose SQLite path points into its read-only code folder, three times over.
func serverStartupFailure() []string {
	release := "/srv/gitea/data/company-apps/914369b4/releases/858ad654"
	tb := []string{
		"Traceback (most recent call last):",
		`  File "` + release + `/.venv/bin/uvicorn", line 8, in <module>`,
		"    sys.exit(main())",
		"             ^^^^^^",
		`  File "<frozen importlib._bootstrap>", line 488, in _call_with_frames_removed`,
		`  File "` + release + `/app/main.py", line 80, in <module>`,
		"    init_db()",
		`  File "` + release + `/app/main.py", line 34, in get_db`,
		"    conn = sqlite3.connect(DB_PATH, timeout=10)",
		"           ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^",
		"sqlite3.OperationalError: unable to open database file",
	}
	var out []string
	for range 3 {
		out = append(out, tb...)
		out = append(out, "INFO:     Started server process [1]", "INFO:     Shutting down")
	}
	return out
}

func TestErrorLinesCollapsesRepeatedTracebacks(t *testing.T) {
	got := ErrorLines(logLines(serverStartupFailure()...))
	assert.Len(t, got, 11, "one copy of the traceback, frames and all")
	assert.Equal(t, "Traceback (most recent call last):", got[0].Text)
	assert.Equal(t, 3, got[0].Repeats)
	assert.Equal(t, "sqlite3.OperationalError: unable to open database file", got[len(got)-1].Text)
}

func TestFindFailurePointsAtTheAppsOwnCode(t *testing.T) {
	f := FindFailure(logLines(serverStartupFailure()...))
	require.NotNil(t, f)
	assert.Equal(t, "sqlite3.OperationalError: unable to open database file", f.Exception)
	assert.Equal(t, "main.py", f.File, "the innermost frame in the code folder, not uvicorn's or the stdlib's")
	assert.Equal(t, 34, f.Line)
	assert.Equal(t, "get_db", f.Func)
	assert.Equal(t, "company.app.finding.database_path", f.Hint)
	assert.Equal(t, 3, f.Repeats)
}

func TestFindFailureHints(t *testing.T) {
	cases := []struct {
		name, source, exception, hint, arg string
	}{
		{"pip name differs from import", "    import cv2", "ModuleNotFoundError: No module named 'cv2'", "company.app.finding.missing_package", "opencv-python"},
		{"unknown module", "    import openpyxl.styles", "ModuleNotFoundError: No module named 'openpyxl.styles'", "company.app.finding.missing_module", "openpyxl"},
		{"environment variable", `    url = os.environ["ERP_URL"]`, "KeyError: 'ERP_URL'", "company.app.finding.missing_env", "ERP_URL"},
		{"a dict of its own", `    row = cache["x"]`, "KeyError: 'x'", "", ""},
		{"syntax", "    def f(", "SyntaxError: '(' was never closed", "company.app.finding.syntax", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := FindFailure(logLines(
				"Traceback (most recent call last):",
				`  File "/app/main.py", line 3`, // bubblewrap's view; SyntaxError frames carry no function
				c.source,
				c.exception,
			))
			require.NotNil(t, f)
			assert.Equal(t, "main.py", f.File)
			assert.Equal(t, c.hint, f.Hint)
			assert.Equal(t, c.arg, f.HintArg)
		})
	}

	f := FindFailure(logLines(`ERROR:    Error loading ASGI app. Attribute "app" not found in module "main".`))
	require.NotNil(t, f, "uvicorn reports this one without a traceback")
	assert.Equal(t, "company.app.finding.no_app_object", f.Hint)
	assert.Nil(t, FindFailure(logLines("INFO:     Application startup complete.")))
}
