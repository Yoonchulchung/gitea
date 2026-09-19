// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gitea.dev/modules/log"
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
// A failure is one line, or a traceback from its header to its exception, and
// consecutive identical failures collapse into their newest copy with Repeats
// counting them: an app in a crash loop writes the same traceback every few
// seconds, and showing it forty times hides everything else that happened.
func ErrorLines(lines []LogLine) []LogLine {
	var out, prev, cur []LogLine
	flush := func() {
		if len(cur) == 0 {
			return
		}
		cur[0].Repeats = 1
		if slices.EqualFunc(cur, prev, func(a, b LogLine) bool { return a.Text == b.Text }) {
			out = out[:len(out)-len(prev)]
			cur[0].Repeats = prev[0].Repeats + 1
		}
		out = append(out, cur...)
		prev, cur = cur, nil
	}
	inTraceback := false
	for _, line := range lines {
		text := strings.TrimRight(line.Text, "\r\n")
		if inTraceback && (tracebackContinuation.MatchString(text) || strings.HasPrefix(text, "    ")) {
			cur = append(cur, line)
			continue
		}
		closing := inTraceback // the exception line ends the traceback it follows
		inTraceback = false
		if !isErrorLine(text) {
			flush()
			continue
		}
		header := strings.Contains(strings.ToLower(text), "traceback")
		if header || !closing {
			flush()
		}
		cur = append(cur, line)
		if inTraceback = header; !header {
			flush()
		}
	}
	flush()
	if len(out) > maxDiagnoseLines {
		out = out[len(out)-maxDiagnoseLines:] // the most recent failure is the one being asked about
	}
	return out
}

func isErrorLine(text string) bool {
	return slices.ContainsFunc(errorLinePatterns, func(re *regexp.Regexp) bool { return re.MatchString(text) })
}

// LogFinding is the most recent failure in a log, read without a model: what
// was raised, where in the department's own code, and — for the failures
// these apps keep hitting — what to change.
type LogFinding struct {
	// Exception is the traceback's last line, verbatim. It can carry the
	// app's data, so it goes only where the log itself may.
	Exception string
	File      string // relative to the app's code directory; "" when no frame is in it
	Line      int
	Func      string
	Hint      string // locale key, "" when the failure is not a known one
	HintArg   string
	AdminHint string // locale key
	Repeats   int
	At        int64
}

var (
	tracebackHeader = regexp.MustCompile(`(?i)traceback \(most recent call last\):\s*$`)
	tracebackFrame  = regexp.MustCompile(`^\s+File "([^"]+)", line (\d+)(?:, in (.+?))?\s*$`)
	// A dotted name whose last part has a lower-case letter, so "INFO:" and
	// "ERROR:" from uvicorn do not pass for an exception.
	exceptionLine = regexp.MustCompile(`^(?:[A-Za-z_]\w*\.)*[A-Z]\w*[a-z]\w*(?::|$)`)
	// Code lives at /app under bubblewrap and inside the release elsewhere.
	appCodeFrame   = regexp.MustCompile(`^/app/(.+)$|/releases/[^/]+/app/(.+)$`)
	asgiLoadError  = regexp.MustCompile(`Error loading ASGI app\. (.+)$`)
	missingEnvName = regexp.MustCompile(`^KeyError: '([^']+)'`)
)

// failureHints are the failures department apps actually hit, matched
// against the exception line; the first capture group is the hint's argument.
var failureHints = []struct {
	re        *regexp.Regexp
	hint      string
	adminHint string
}{
	{regexp.MustCompile(`unable to open database file|attempt to write a readonly database`), "company.app.finding.database_path", "company.app.finding.database_path.admin"},
	{regexp.MustCompile(`\[Errno 30\] Read-only file system`), "company.app.finding.read_only", ""},
	{regexp.MustCompile(`^PermissionError: \[Errno 13\]`), "company.app.finding.permission", ""},
	{regexp.MustCompile(`^ModuleNotFoundError: No module named '([^'.]+)`), "company.app.finding.missing_module", ""},
	{regexp.MustCompile(`requires "([^"]+)" to be installed`), "company.app.finding.missing_package", ""}, // FastAPI forms without python-multipart
	{regexp.MustCompile(`^(?:SyntaxError|IndentationError|TabError)\b`), "company.app.finding.syntax", ""},
	{regexp.MustCompile(`^NameError: name '([^']+)' is not defined`), "company.app.finding.undefined_name", ""},
	{regexp.MustCompile(`^MemoryError\b`), "company.app.finding.memory", ""},
	{regexp.MustCompile(`Temporary failure in name resolution|Name or service not known|Network is unreachable|\bConnectError\b`), "company.app.finding.network", ""},
	{regexp.MustCompile(`^Attribute "app" not found in module "main"`), "company.app.finding.no_app_object", ""},
}

// pipNames are import names whose package is called something else, where a
// department adding the import name to requirements.txt would fail again.
var pipNames = map[string]string{
	"PIL": "Pillow", "cv2": "opencv-python", "yaml": "PyYAML", "sklearn": "scikit-learn",
	"bs4": "beautifulsoup4", "dotenv": "python-dotenv", "docx": "python-docx", "pptx": "python-pptx",
	"dateutil": "python-dateutil", "jwt": "PyJWT", "multipart": "python-multipart",
}

type tracebackFrameInfo struct {
	path, fn, source string
	line             int
}

// FindFailure returns the most recent failure in an app's own output, or nil.
func FindFailure(lines []LogLine) *LogFinding {
	var found []*LogFinding
	var frames []tracebackFrameInfo
	inTraceback := false
	for _, line := range lines {
		if line.Build {
			continue // a failed build is explained from its own output (summarizeInstallFailure)
		}
		text := strings.TrimRight(line.Text, "\r\n")
		if tracebackHeader.MatchString(text) {
			inTraceback, frames = true, frames[:0]
			continue
		}
		if inTraceback {
			if m := tracebackFrame.FindStringSubmatch(text); m != nil {
				n, _ := strconv.Atoi(m[2])
				frames = append(frames, tracebackFrameInfo{path: m[1], line: n, fn: m[3]})
				continue
			}
			if strings.HasPrefix(text, " ") {
				if n := len(frames); n > 0 && frames[n-1].source == "" && strings.Trim(text, " ^~") != "" {
					frames[n-1].source = strings.TrimSpace(text)
				}
				continue
			}
			inTraceback = false
			if exceptionLine.MatchString(text) {
				found = append(found, newLogFinding(text, frames, line.At))
				continue
			}
		}
		if m := asgiLoadError.FindStringSubmatch(text); m != nil {
			found = append(found, newLogFinding(m[1], nil, line.At))
		}
	}
	if len(found) == 0 {
		return nil
	}
	last := found[len(found)-1]
	for _, f := range found {
		if f.Exception == last.Exception && f.File == last.File && f.Line == last.Line {
			last.Repeats++
		}
	}
	return last
}

func newLogFinding(exception string, frames []tracebackFrameInfo, at int64) *LogFinding {
	f := &LogFinding{Exception: exception, At: at}
	source := ""
	for _, fr := range slices.Backward(frames) {
		m := appCodeFrame.FindStringSubmatch(fr.path)
		if m == nil {
			continue
		}
		f.File, f.Line, source = m[1]+m[2], fr.line, fr.source
		if !strings.HasPrefix(fr.fn, "<") { // <module>, <lambda>
			f.Func = fr.fn
		}
		break
	}
	// Only when the failing line reads the environment: a KeyError elsewhere
	// is a dictionary the app built itself.
	if m := missingEnvName.FindStringSubmatch(exception); m != nil && strings.Contains(source, "environ[") {
		f.Hint, f.HintArg = "company.app.finding.missing_env", m[1]
		return f
	}
	for _, h := range failureHints {
		m := h.re.FindStringSubmatch(exception)
		if m == nil {
			continue
		}
		f.Hint, f.AdminHint = h.hint, h.adminHint
		if len(m) > 1 {
			f.HintArg = m[1]
		}
		if pkg, ok := pipNames[f.HintArg]; ok && f.Hint == "company.app.finding.missing_module" {
			f.Hint, f.HintArg = "company.app.finding.missing_package", pkg
		}
		break
	}
	return f
}

// logFindingWindow is how far before a failure its traceback may be. Wide
// enough for a health check's full wait, narrow enough that an old
// traceback is not offered as the reason for a new failure.
const logFindingWindow = 15 * 60

// LogFindingFor reads the failure behind cause out of the app's log, or nil
// when the cause is not one the app's own output explains.
func LogFindingFor(st *AppState, cause *AppCause) *LogFinding {
	if cause == nil || !slices.Contains([]string{ReasonHealthTimeout, ReasonCrashLoop, ReasonRolledBack, ReasonUnresponsive}, st.Reason) {
		return nil
	}
	lines, _, err := ReadAppLogs(st.Owner, st.Repo, LogQuery{Limit: logDiagnoseScan})
	if err != nil {
		log.Warn("company: %s/%s: reading the log for a failure: %v", st.Owner, st.Repo, err)
		return nil
	}
	f := FindFailure(lines)
	if f == nil || (f.At != 0 && cause.At != 0 && f.At < cause.At-logFindingWindow) {
		return nil
	}
	return f
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
	if AILogAccess() == AILogAccessOff {
		return "", userKeyError("company.logs.diagnose_off")
	}
	// The same policy as the assistant's: which lines may leave, masked,
	// with the host's paths gone (company/ailogpolicy.go).
	lines = aiLogLines(owner, repo, lines)
	if len(lines) == 0 {
		return "", userKeyError("company.logs.diagnose_nothing")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "App: %s/%s\n\nError lines from the log:\n", owner, repo)
	for _, line := range lines {
		b.WriteString(strings.TrimRight(line.Text, "\r\n"))
		b.WriteByte('\n')
	}
	return aiChat(ctx, userID, logDiagnosePrompt, b.String())
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
