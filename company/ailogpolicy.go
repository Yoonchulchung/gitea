// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"regexp"
	"slices"
	"strings"
)

// An app's output is the app's data: a print() of a row, a value in an
// exception message, a query with its literals. Sent to a model it leaves
// for a provider, and nothing here can promise that what leaves carries no
// company data — masking finds shapes (an email, a card number, a token),
// not meaning (a name, an amount). So the policy comes first and masking
// last:
//
//	[company] AI_LOG_ACCESS = errors   the failures only — tracebacks and
//	                                   error lines — masked (default)
//	                          full     every line, masked
//	                          off      no app output reaches a model; the
//	                                   assistant gets files and line numbers
//
// Where output must never leave the company, the answer is "off" or an AI
// endpoint inside the company (AI_API_URL); masking is the layer under
// that, not a replacement for it. Applied everywhere app output meets a
// model: the workspace assistant's tools (company/workspace_ai_app.go) and
// the administrator's log diagnosis (company/logdiagnose.go).

const (
	AILogAccessErrors = "errors"
	AILogAccessFull   = "full"
	AILogAccessOff    = "off"
)

// AILogAccess is the policy in force.
func AILogAccess() string {
	switch v := strings.ToLower(companySetting("AI_LOG_ACCESS")); v {
	case AILogAccessFull, AILogAccessOff:
		return v
	default:
		return AILogAccessErrors
	}
}

// maskPatterns are the shapes masking knows. Order matters: a credential
// after its label is caught before the generic long-token rule renames it.
var maskPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*|bearer\s+)[A-Za-z0-9._\-+/=]{8,}`), "${1}[secret]"},
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret)\b(\s*[=:]\s*)["']?([^\s"',;)]+)`), "${1}${2}[secret]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[email]"},
	{regexp.MustCompile(`\b\d{6}-?[1-4]\d{6}\b`), "[주민번호]"},
	{regexp.MustCompile(`\b(?:\d{4}[- ]){3}\d{4}\b`), "[card]"},
	{regexp.MustCompile(`\b01[016789]-?\d{3,4}-?\d{4}\b`), "[phone]"},
	{regexp.MustCompile(`\b0\d{1,2}-\d{3,4}-\d{4}\b`), "[phone]"},
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "[ip]"},
	{regexp.MustCompile(`\b[A-Za-z0-9_\-]{32,}\b`), "[token]"},
}

// maskForAI hides the app's own secrets — exact values, so nothing is
// missed — and then every shape the patterns know.
func maskForAI(text string, secrets []string) string {
	if text == "" {
		return text
	}
	secrets = slices.DeleteFunc(slices.Clone(secrets), func(s string) bool { return len(s) < 4 })
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) }) // longest first, so a value inside another is not left half-masked
	for _, s := range secrets {
		text = strings.ReplaceAll(text, s, "[secret]")
	}
	for _, p := range maskPatterns {
		text = p.re.ReplaceAllString(text, p.repl)
	}
	return text
}

// appSecrets is what the department stored for the app: values that must
// never appear in what leaves.
func appSecrets(owner, repo string) []string {
	env, _, err := LoadAppEnv(owner, repo)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, v := range env {
		out = append(out, v)
	}
	return out
}

// aiLogLines applies the policy to log lines bound for a model: which
// lines at all, then masked, with the server's paths gone.
func aiLogLines(owner, repo string, lines []LogLine) []LogLine {
	switch AILogAccess() {
	case AILogAccessOff:
		return nil
	case AILogAccessErrors:
		lines = ErrorLines(lines)
	default:
		lines = slices.Clone(lines)
	}
	secrets := appSecrets(owner, repo)
	for i := range lines {
		lines[i].Text = maskForAI(RedactServerPaths(lines[i].Text), secrets)
	}
	return lines
}

// aiOutputText applies the policy to a block of app output (the startup
// check's), the same way.
func aiOutputText(owner, repo, output string) string {
	if output == "" {
		return ""
	}
	var lines []LogLine
	for l := range strings.SplitSeq(output, "\n") {
		lines = append(lines, LogLine{Text: l})
	}
	kept := aiLogLines(owner, repo, lines)
	texts := make([]string, 0, len(kept))
	for _, l := range kept {
		texts = append(texts, l.Text)
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

// aiErrorText applies the policy to one error message: an exception line, a
// traceback — content a model may see under "errors" and "full", masked,
// and not at all under "off".
func aiErrorText(owner, repo, text string) string {
	if AILogAccess() == AILogAccessOff {
		return ""
	}
	return maskForAI(RedactServerPaths(text), appSecrets(owner, repo))
}
