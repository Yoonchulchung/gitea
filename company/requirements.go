// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"

	"gitea.dev/modules/translation"
)

// requirements.txt is written by department staff and fed straight to pip,
// which runs as the Gitea account *outside* the sandbox. That makes this
// file the most security-relevant thing a non-developer can author on this
// platform — see docs/company/app-platform.md ("가장 현실적인 경로는 pip").
//
// Two layers here, both needed:
//
//  1. Shape validation. pip's own format accepts far more than a package
//     name: `git+https://…`, a bare URL, `-e ./local`, and `--index-url`
//     all redirect where code comes from. Those are exactly the "이상한
//     레포 가져오기" cases, and none of them have a legitimate use here, so
//     the parser refuses anything that isn't `name==version`.
//
//  2. Allowlist. Shape validation still lets through any name on PyPI,
//     including a typosquat one character away from a real package. Only
//     an admin-approved list closes that.
//
// Neither layer is a substitute for the sandbox: a package that gets past
// both still executes inside it when the app imports it. The order is
// "don't install it" → "installing it doesn't help you".

// validRequirement is deliberately strict: an exact `name==version` pin and
// nothing else. Version ranges (`>=`, `~=`) are rejected too — a range means
// the code that ships can change without anyone approving it again, which
// defeats the point of approving a package at all.
var validRequirement = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*==[A-Za-z0-9][A-Za-z0-9.!+*_-]*$`)

// requirementPrefixDenied lists line prefixes that redirect where a package
// comes from. Checked before the shape regex only so the error can say
// *which* problem it is — the regex alone would reject these anyway, with a
// message a non-developer couldn't act on.
var requirementPrefixDenied = []struct {
	prefix string
	reason string
}{
	{"-e", "company.req.no_editable"},
	{"--editable", "company.req.no_editable"},
	{"-r", "company.req.no_include"},
	{"--requirement", "company.req.no_include"},
	{"-i", "company.req.no_index"},
	{"--index-url", "company.req.no_index"},
	{"--extra-index-url", "company.req.no_index"},
	{"--find-links", "company.req.no_find_links"},
	{"-f", "company.req.no_find_links"},
	{"--trusted-host", "company.req.no_trusted_host"},
	{"git+", "company.req.no_git"},
	{"http://", "company.req.no_url"},
	{"https://", "company.req.no_url"},
	{"file://", "company.req.no_local_path"},
}

// Requirement is one accepted dependency.
type Requirement struct {
	Name    string // normalized for comparison (lowercase, - and _ and . unified)
	Version string
	Raw     string // exactly as written, for passing back to pip
}

// RequirementError explains one rejected line in language a non-developer
// can act on. Line is 1-based so it matches what an editor shows.
//
// The explanation is a locale key rather than a sentence: the same error is
// shown on the deploy form, where the reader's language is known, and stored
// by the build worker, where it is not (company/usererror.go).
type RequirementError struct {
	Line   int
	Text   string
	Reason string
	Args   []any
}

func (e RequirementError) Error() string {
	return fmt.Sprintf("line %d (%q): %s", e.Line, e.Text,
		translation.NewLocale("en-US").TrString(e.Reason, e.Args...))
}

// normalizePackageName applies PEP 503 normalization so the allowlist can't
// be sidestepped by spelling: `Foo_Bar`, `foo-bar` and `foo.bar` are all one
// package to pip, and would be three different strings to a naive compare.
func normalizePackageName(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		if r == '-' || r == '_' || r == '.' {
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
			continue
		}
		b.WriteRune(r)
		prevDash = false
	}
	return b.String()
}

// ParseRequirements validates a requirements.txt and returns the accepted
// entries. Every rejected line is reported — not just the first — so a
// department fixes them in one pass instead of one deploy attempt per typo.
func ParseRequirements(content string) ([]Requirement, []RequirementError) {
	var reqs []Requirement
	var errs []RequirementError

	scanner := bufio.NewScanner(strings.NewReader(content))
	// requirements.txt lines are short; a long one is malformed, and the
	// default 64KB limit is plenty. Guard anyway so a pathological file
	// fails cleanly rather than mid-scan.
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)

	for line := 1; scanner.Scan(); line++ {
		raw := scanner.Text()
		text := strings.TrimSpace(raw)
		// Strip trailing comments, but only when the "#" starts a token —
		// "#" is not legal inside a name or version, so this can't eat part
		// of a valid requirement.
		if i := strings.Index(text, " #"); i >= 0 {
			text = strings.TrimSpace(text[:i])
		}
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		lower := strings.ToLower(text)
		denied := false
		for _, d := range requirementPrefixDenied {
			if strings.HasPrefix(lower, d.prefix) {
				errs = append(errs, RequirementError{Line: line, Text: text, Reason: d.reason})
				denied = true
				break
			}
		}
		if denied {
			continue
		}

		if !validRequirement.MatchString(text) {
			reason, args := shapeProblem(text)
			errs = append(errs, RequirementError{Line: line, Text: text, Reason: reason, Args: args})
			continue
		}
		name, version, _ := strings.Cut(text, "==")
		reqs = append(reqs, Requirement{Name: normalizePackageName(name), Version: version, Raw: text})
	}
	if err := scanner.Err(); err != nil {
		errs = append(errs, RequirementError{Reason: "company.req.unreadable", Args: []any{err.Error()}})
	}
	return reqs, errs
}

// bareName is a package name with no version at all — by far the most common
// way to get this wrong, since it is what pip itself accepts and what every
// example on the internet shows.
var bareName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// rangeOperator catches the other common shape: a version *range*. Rejected
// on purpose — a range means the code that ships can change without anyone
// approving it again — so the message has to say that rather than repeat the
// required form at someone who plainly knows what a version is.
var rangeOperator = regexp.MustCompile(`[><~!]=?|===`)

// shapeProblem says which mistake this line is, as a key and its arguments.
//
// One message for every malformed line told a non-developer with six bare
// names the same sentence six times and named none of them, which is how
// "fastapi" and "-e ." come to look like the same problem.
func shapeProblem(text string) (string, []any) {
	switch {
	case bareName.MatchString(text):
		return "company.req.needs_version", []any{text, text}
	case rangeOperator.MatchString(text):
		name, _, _ := strings.Cut(strings.FieldsFunc(text, func(r rune) bool {
			return strings.ContainsRune("><~!= ", r)
		})[0], "[")
		return "company.req.no_ranges", []any{text, name}
	default:
		return "company.req.one_per_line", []any{text}
	}
}

// DeniedPackages returns the requirements that are not in allowed, in the
// order they appeared. An empty allowlist denies everything: an app whose
// packages have never been approved must not deploy just because nobody has
// filled the list in yet.
func DeniedPackages(reqs []Requirement, allowed []string) []Requirement {
	allow := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allow[normalizePackageName(a)] = true
	}
	var denied []Requirement
	for _, r := range reqs {
		if !allow[r.Name] {
			denied = append(denied, r)
		}
	}
	return denied
}

// validBasePackage is a name, optionally pinned. Unlike a department's
// requirements.txt this may be unpinned, and that is deliberate: the base
// packages are the platform's own, and the Python they have to work with is
// whatever the host provides. Pinning fastapi to a version whose wheels
// predate the host's interpreter breaks every app at once — exactly the
// failure this list exists to prevent.
var validBasePackage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(?:==[A-Za-z0-9][A-Za-z0-9.!+*_-]*)?$`)

// ParseBasePackages reads the admin-managed list. The locale is the caller's
// because this runs inside an admin's own request.
func ParseBasePackages(l translation.Locale, content string) (packages, problems []string) {
	seen := map[string]bool{}
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		// The same redirections a requirements.txt may not use. An admin is
		// trusted, but a base package installs into *every* app, so a typo
		// that fetches from an arbitrary URL is worth catching here too.
		denied := false
		for _, d := range requirementPrefixDenied {
			if strings.HasPrefix(strings.ToLower(line), d.prefix) {
				problems = append(problems, l.TrString("company.req.line_prefix", i+1, l.TrString(d.reason)))
				denied = true
				break
			}
		}
		if denied {
			continue
		}
		if !validBasePackage.MatchString(line) {
			problems = append(problems, l.TrString("company.req.base_shape", i+1, line))
			continue
		}
		name, _, _ := strings.Cut(line, "==")
		key := normalizePackageName(name)
		if seen[key] {
			problems = append(problems, l.TrString("company.req.duplicate", i+1, name))
			continue
		}
		seen[key] = true
		packages = append(packages, line)
	}
	return packages, problems
}

// BasePackageNames strips the version pins, for allowlist comparison.
func BasePackageNames(packages []string) []string {
	names := make([]string, 0, len(packages))
	for _, p := range packages {
		name, _, _ := strings.Cut(p, "==")
		names = append(names, name)
	}
	return names
}
