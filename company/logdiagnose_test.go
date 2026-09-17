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
