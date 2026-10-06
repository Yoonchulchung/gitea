// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func diffAdding(path string, lines ...string) string {
	out := "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -0,0 +1 @@\n"
	for _, l := range lines {
		out += "+" + l + "\n"
	}
	return out
}

func TestRiskyAdditions(t *testing.T) {
	assert.Empty(t, riskyAdditions(diffAdding("PO/da/main.py",
		"from os import path",
		"from fastapi import FastAPI",
		`    return {"message": "hello"}`,
	)))

	// Where, what, and why — not just a category.
	diff := "+++ b/PO/da/main.py\n@@ -3,2 +10,4 @@\n context\n-removed\n+import subprocess\n+key = os.environ['API_KEY']\n" // API_KEY: not set for this app
	got := riskyAdditions(diff)
	if assert.Len(t, got, 2) {
		assert.Equal(t, "main.py:11", got[0].File)
		assert.Contains(t, got[0].Issue, "명령 실행 — ")
		assert.Contains(t, got[0].Issue, "'import subprocess'")
		assert.Equal(t, "main.py:12", got[1].File)
		assert.Contains(t, got[1].Issue, "비밀값")
		assert.Contains(t, got[1].Issue, "os.environ['API_KEY']")
	}
	assert.Equal(t, "명령 실행, 환경 변수 읽기", riskyLabels(got))

	// A "#" in a string cannot hide the rest of the line.
	assert.Len(t, riskyAdditions(diffAdding("PO/da/util.py", `x = "#"; v = os.environ`)), 1)
	assert.Len(t, riskyAdditions(diffAdding("PO/da/a.py", "from os import system")), 1)
	assert.Len(t, riskyAdditions(diffAdding("PO/da/a.py", "f = getattr(__builtins__, name)")), 1)
	assert.Len(t, riskyAdditions(diffAdding("PO/da/a.py", "data = base64.b64decode(blob)")), 1)

	// Reading a variable by name is how an app gets the platform's paths and the
	// department's own settings; only bulk or computed access is a finding.
	for _, line := range []string{
		`conn = sqlite3.connect(os.environ["DB_PATH"], timeout=10)`,
		`root = os.getenv('ROOT_PATH', '/')`,
		`key = os.environ.get("ERP_TOKEN")`, // set by the department for this app
		`from os import environ`,
	} {
		assert.Empty(t, riskyAdditions(diffAdding("PO/da/main.py", line), "ERP_TOKEN"), line)
	}
	for _, line := range []string{
		`print(dict(os.environ))`,
		`v = os.environ[name]`,
		`x = os.environ["AWS_SECRET_ACCESS_KEY"]`, // a name nobody set for this app
		`a = os.environ["DB_PATH"]; b = os.environ`,
	} {
		assert.Len(t, riskyAdditions(diffAdding("PO/da/main.py", line), "ERP_TOKEN"), 1, line)
	}

	// One file doing the same thing everywhere does not bury the table.
	assert.Len(t, riskyAdditions(diffAdding("PO/da/a.py", "os.getenv(a)", "os.getenv(b)", "os.getenv(c)", "os.getenv(d)")), riskyFindingsPerFileMax)

	// Only added lines of Python files count: a removal, or a README, adds nothing.
	assert.Empty(t, riskyAdditions("+++ b/PO/da/README.md\n+use subprocess carefully\n+++ b/PO/da/a.py\n-import subprocess\n"))
}
