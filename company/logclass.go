// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"regexp"
	"slices"

	"gitea.dev/services/context"
)

// A deployed app's log is mostly the web server narrating itself. On this
// instance one app's log was 8304 lines of which 537 were uvicorn saying it
// had started, and one request to a health endpoint writes a line every time
// it is polled. Searching for a word works when you know the word; these
// filters are for the far more common case of not knowing it yet, and needing
// the routine traffic out of the way to see what is left.
//
// Named categories rather than a regexp box: the person reading this deployed
// a Python app without being a Python developer, and "hide the request log" is
// a thing they can want without being able to write the pattern for it.

// Log view modes, as they appear in the query string.
const (
	LogViewAll      = ""
	LogViewErrors   = "errors"
	LogViewRequests = "requests"
	LogViewApp      = "app"
)

// requestLine matches one served HTTP request, in the shape every Python web
// server writes it — uvicorn, gunicorn and werkzeug all quote the method,
// target and protocol the same way, so matching that is more durable than
// matching any one of their line formats.
var requestLine = regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|TRACE|CONNECT) [^"]*HTTP/[0-9.]+"`)

// lifecycleLine matches a server announcing its own startup or shutdown.
// These repeat once per deploy and per restart, and a crash loop turns them
// into the bulk of the file.
var lifecycleLine = regexp.MustCompile(`(?i)\b(` +
	`waiting for application (startup|shutdown)` +
	`|application (startup|shutdown) complete` +
	`|(started|finished) server process` +
	`|uvicorn running on` +
	`|shutting down` +
	`|starting gunicorn` +
	`|listening at:` +
	`|booting worker` +
	`|press ctrl\+c to quit` +
	`)`)

// FilterLines narrows a log to one view. An unknown mode returns everything,
// so a hand-edited query string shows too much rather than too little.
func FilterLines(lines []LogLine, mode string) []LogLine {
	switch mode {
	case LogViewErrors:
		return ErrorLines(lines)
	case LogViewRequests:
		return keepLines(lines, false, requestLine.MatchString)
	case LogViewApp:
		// Build output survives this one: it is not the running server's
		// chatter, and a deploy that failed has all of its detail there.
		return keepLines(lines, true, func(text string) bool {
			return !requestLine.MatchString(text) && !lifecycleLine.MatchString(text)
		})
	}
	return lines
}

func keepLines(lines []LogLine, keepBuild bool, keep func(string) bool) []LogLine {
	out := make([]LogLine, 0, len(lines))
	for _, l := range lines {
		if (keepBuild && l.Build) || keep(l.Text) {
			out = append(out, l)
		}
	}
	return out
}

// logView reads the requested view from the query string.
//
// "?errors=1" is still honoured: it is what the errors-only toggle sent
// before these views existed, and links to it are in people's history and in
// the deploy mails the platform has already sent.
func logView(ctx *context.Context) string {
	if view := ctx.FormString("view"); slices.Contains([]string{LogViewErrors, LogViewRequests, LogViewApp}, view) {
		return view
	}
	if ctx.FormBool("errors") {
		return LogViewErrors
	}
	return LogViewAll
}
