// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"os/exec"
	"testing"

	"gitea.dev/modules/translation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tag left open is reported at the line it was opened; a stray end tag
// at its own line; what HTML lets a page leave open is not reported.
func TestHTMLCheckFindsWhatABrowserWouldGuessAt(t *testing.T) {
	page := "<html><body>\n<div class=\"a\">\n<p>one\n<p>two<br>\n<span>x</div>\n</section>\n<ul><li>a<li>b</ul>\n</body></html>"
	findings := checkHTML("index.html", []byte(page))
	require.Len(t, findings, 2, findings)
	assert.Equal(t, 5, findings[0].Line)
	assert.Contains(t, findings[0].Message, "<span> is not closed before </div>")
	assert.Equal(t, 6, findings[1].Line)
	assert.Contains(t, findings[1].Message, "</section> closes nothing")

	assert.Empty(t, checkHTML("ok.html", []byte("<!doctype html><html><head><title>t</title></head><body><script>if (a < b) {}</script><p>fine</body></html>")))
	unclosed := checkHTML("u.html", []byte("<div>\n<table><tr><td>x</table>\n"))
	require.Len(t, unclosed, 1)
	assert.Equal(t, 1, unclosed[0].Line)
	assert.Contains(t, unclosed[0].Message, "<div> is never closed")
}

// Modern JavaScript passes; a missing bracket is reported where the parser
// stopped.
func TestJSCheckParsesCurrentSyntax(t *testing.T) {
	assert.Empty(t, checkJS("a.js", []byte("const f = async (x) => { const {a, ...rest} = await x?.y ?? {}; return [...rest, a]; };\nclass A { #p = 1; static m() {} }\nexport default f;")))
	findings := checkJS("b.js", []byte("function f() {\n  return 1;\n\nconst x = ;"))
	require.Len(t, findings, 1)
	assert.Equal(t, 4, findings[0].Line)
	assert.NotEmpty(t, findings[0].Message)
}

// The interpreter reports a syntax error with its line, and nothing runs:
// a file whose import would fail at run time still parses.
func TestPythonCheckParsesWithoutRunning(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	if _, err := pythonPath(); err != nil {
		t.Skip("no build interpreter on this host")
	}
	findings, err := checkPython(context.Background(), translation.NewLocale("en-US"), []checkFile{
		{Path: "ok.py", Source: "import module_that_does_not_exist\nraise SystemExit(3)\n"},
		{Path: "bad.py", Source: "def f(:\n    pass\n"},
		// Parses, and dies on import: the case that reached an approver.
		{Path: "main.py", Source: "TEST\n"},
		// Bound later in the file, inside a function, by a star import: none of these are reported.
		{Path: "fine.py", Source: "from os.path import *\napp = join('a')\n"},
		{Path: "later.py", Source: "import fastapi\napp = fastapi.FastAPI()\n\ndef f():\n    return helper\nhelper = 1\n"},
	})
	require.NoError(t, err)
	require.Len(t, findings, 3)
	assert.Equal(t, "bad.py", findings[0].Path)
	assert.Equal(t, 1, findings[0].Line)
	assert.Equal(t, "main.py", findings[1].Path)
	assert.Contains(t, findings[1].Message, "TEST")
	assert.Equal(t, "main.py", findings[2].Path) // no app
}
