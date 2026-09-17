// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"gitea.dev/modules/setting"
	gitea_context "gitea.dev/services/context"
)

// Finding the failure in the log, and explaining it.
//
// An app's log is mostly uvicorn telling you it is alive. The thing that went
// wrong is a handful of lines somewhere in the middle, and the person who
// needs them is a department member who has never read a Python traceback —
// they scroll, see nothing they recognise, and open a ticket.
//
// So two steps, in that order and deliberately separate. First pull out what
// looks like a failure, mechanically, with no model involved: that alone is
// often enough, it costs nothing, and it works when AI is switched off — which
// on this platform is the default. Only then, and only if someone asks, send
// those lines to be explained.

// errorLinePatterns are what a failure looks like in the logs these apps
// actually produce: Python tracebacks, uvicorn and pip failures, and the
// platform's own refusals.
//
// Anchored where it matters. "error" unanchored matches "error_handler" in a
// request path and every INFO line mentioning an error page, which buries the
// real one — the whole point is to shorten what a person has to read.
var errorLinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*traceback \(most recent call last\)`),
	regexp.MustCompile(`(?i)^\s*(\w+\.)*\w*(error|exception)\b.*:`), // ValueError:, sqlite3.OperationalError:, …
	regexp.MustCompile(`(?i)^\s*\[(error|critical|fatal)\]`),
	regexp.MustCompile(`(?i)\b(critical|fatal)\b`),
	regexp.MustCompile(`(?i)^\s*error:\s`),
	regexp.MustCompile(`(?i)\bmodulenotfounderror\b|\bimporterror\b|\bsyntaxerror\b`),
	regexp.MustCompile(`(?i)\bkilled\b|\bout of memory\b|\boom\b`),
	regexp.MustCompile(`(?i)^\s*ERROR: `),                                     // pip
	regexp.MustCompile(`(?i)\bno matching distribution\b|\bcould not find\b`), // pip
	regexp.MustCompile(`\b(5\d{2}) (Internal Server Error|Bad Gateway|Service Unavailable)\b`),
}

// tracebackContinuation is a line that belongs to the traceback above it
// rather than standing on its own — the "  File ..." and source echoes. Kept
// because a traceback without its frames names the exception and not the
// place, and the place is the answer.
var tracebackContinuation = regexp.MustCompile(`^\s+(File "|\.\.\.|\^+\s*$)`)

// maxDiagnoseLines bounds what is collected and what is sent. A log that is
// failing is usually failing repeatedly, and the fiftieth copy of the same
// traceback adds nothing but cost.
const maxDiagnoseLines = 60

// ErrorLines picks the failures out of a log, newest last so a traceback
// still reads top to bottom.
//
// Consecutive identical texts collapse: an app in a crash loop writes the
// same traceback every few seconds, and showing it forty times hides
// everything else that happened.
func ErrorLines(lines []LogLine) []LogLine {
	var out []LogLine
	inTraceback := false
	for _, line := range lines {
		text := strings.TrimRight(line.Text, "\r\n")
		hit := false
		switch {
		case inTraceback && (tracebackContinuation.MatchString(text) || strings.HasPrefix(text, "    ")):
			hit = true
		default:
			for _, re := range errorLinePatterns {
				if re.MatchString(text) {
					hit = true
					break
				}
			}
			// A traceback's frames follow its header; everything until the
			// exception line belongs with it.
			inTraceback = hit && strings.Contains(strings.ToLower(text), "traceback")
		}
		if !hit {
			inTraceback = false
			continue
		}
		if n := len(out); n > 0 && out[n-1].Text == line.Text {
			continue // the same failure again
		}
		out = append(out, line)
	}
	if len(out) > maxDiagnoseLines {
		out = out[len(out)-maxDiagnoseLines:] // the most recent failure is the one being asked about
	}
	return out
}

// logDiagnosePrompt is written for the reader, not for the log.
//
// The audience is someone who deployed a Python app without being a Python
// developer. What they need is which line of their own code to look at and
// what to change — not a lecture on exceptions, and not a suggestion to
// inspect the platform, which they cannot do anything about.
const logDiagnosePrompt = `You are helping an office worker who is not a software developer. They deployed a small internal Python web app (FastAPI) on a company platform, and it is failing. Below are the error lines pulled from its log.

Reply in Korean, and keep it under 150 words. Structure it as:
1. 무엇이 잘못됐는지 — one or two plain sentences. No jargon; if you must name an exception, say what it means.
2. 어디를 고쳐야 하는지 — the file and line from the traceback if there is one, and what to change there.
3. 확실하지 않다면 그렇게 말하세요.

Rules:
- Only talk about their own code and their own files. Never tell them to change platform settings, install system packages, restart servers, or contact anyone — none of that is theirs to do.
- If the log shows a missing package, tell them to add it to requirements.txt and raise a deploy request. That is the route this platform gives them.
- If the lines do not actually show a failure, say so in one sentence rather than inventing one.`

// DiagnoseLog asks the model what the failure means.
//
// The caller's own AI settings and their own key — the same ones every other
// AI feature here uses, so there is no second credential and no shared one.
func DiagnoseLog(ctx context.Context, userID int64, owner, repo string, lines []LogLine) (string, error) {
	if len(lines) == 0 {
		return "", userKeyError("company.logs.diagnose_nothing")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "App: %s/%s\n\nError lines from the log:\n", owner, repo)
	for _, line := range lines {
		b.WriteString(strings.TrimRight(line.Text, "\r\n"))
		b.WriteByte('\n')
	}
	// Tracebacks name absolute paths inside this instance — the data
	// directory, the release hash, the shared virtualenv. None of it helps
	// the explanation, all of it describes the host, and this text is about
	// to leave for somebody else's API. Redacted with the same helper the
	// platform already uses before showing an error to a department
	// (company/usererror.go).
	return aiChat(ctx, userID, logDiagnosePrompt, RedactServerPaths(b.String()))
}

// AdminAppLogDiagnose answers the button on the admin log page.
func AdminAppLogDiagnose(ctx *gitea_context.Context) {
	st, _, ok := adminAppContext(ctx)
	if !ok {
		return
	}
	back := setting.AppSubURL + "/-/admin/company-deploys/" + st.Owner + "/" + st.Repo + "/logs?errors=1"

	lines, _, err := ReadAppLogs(st.Owner, st.Repo, LogQuery{Limit: logDiagnoseScan})
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		ctx.Redirect(back)
		return
	}
	answer, err := DiagnoseLog(ctx, ctx.Doer.ID, st.Owner, st.Repo, ErrorLines(lines))
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		ctx.Redirect(back)
		return
	}
	// Through the flash rather than a stored field: a diagnosis is about the
	// log as it was a moment ago, and one kept on the page would still be
	// there, confidently, after the app was fixed and redeployed.
	ctx.Flash.Info(answer)
	ctx.Redirect(back)
}

// logDiagnoseScan is how much of the log is read before filtering. Larger
// than what is sent: the failure may be well above the newest lines.
const logDiagnoseScan = 2000
