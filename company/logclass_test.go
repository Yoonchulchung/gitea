// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFilterLines(t *testing.T) {
	lines := []LogLine{
		{Text: "Collecting fastapi==0.110.0", Build: true},
		{Text: "INFO:     Started server process [42]"},
		{Text: "INFO:     Application startup complete."},
		{Text: `INFO:      - "GET /apps/PO/Test/health HTTP/1.1" 404 Not Found`},
		{Text: "Traceback (most recent call last):"},
		{Text: `  File "/app/main.py", line 3, in <module>`},
		{Text: "ValueError: bad config"},
		{Text: "processed 12 rows"},
	}

	// The view a reader reaches for: the server's own narration gone, their
	// app's output and the build left.
	assert.Equal(t, []string{
		"Collecting fastapi==0.110.0",
		"Traceback (most recent call last):",
		`  File "/app/main.py", line 3, in <module>`,
		"ValueError: bad config",
		"processed 12 rows",
	}, texts(FilterLines(lines, LogViewApp)))

	// Requests only — and build output is not a request.
	assert.Equal(t, []string{`INFO:      - "GET /apps/PO/Test/health HTTP/1.1" 404 Not Found`},
		texts(FilterLines(lines, LogViewRequests)))

	assert.Len(t, texts(FilterLines(lines, LogViewAll)), len(lines))
	assert.Len(t, texts(FilterLines(lines, "nonsense")), len(lines), "an unknown view shows too much, not too little")
}
