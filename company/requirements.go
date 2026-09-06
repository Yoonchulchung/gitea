// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
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
	{"-e", "editable installs (-e) are not allowed"},
	{"--editable", "editable installs are not allowed"},
	{"-r", "including other requirements files (-r) is not allowed"},
	{"--requirement", "including other requirements files is not allowed"},
	{"-i", "changing the package index (-i) is not allowed"},
	{"--index-url", "changing the package index is not allowed"},
	{"--extra-index-url", "adding a package index is not allowed"},
	{"--find-links", "adding a package source (--find-links) is not allowed"},
	{"-f", "adding a package source (-f) is not allowed"},
	{"--trusted-host", "trusting an extra host is not allowed"},
	{"git+", "installing straight from a git repository is not allowed"},
	{"http://", "installing from a URL is not allowed"},
	{"https://", "installing from a URL is not allowed"},
	{"file://", "installing from a local path is not allowed"},
}

// Requirement is one accepted dependency.
type Requirement struct {
	Name    string // normalized for comparison (lowercase, - and _ and . unified)
	Version string
	Raw     string // exactly as written, for passing back to pip
}

// RequirementError explains one rejected line in language a non-developer
// can act on. Line is 1-based so it matches what an editor shows.
type RequirementError struct {
	Line   int
	Text   string
	Reason string
}

func (e RequirementError) Error() string {
	return fmt.Sprintf("line %d (%q): %s", e.Line, e.Text, e.Reason)
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
			errs = append(errs, RequirementError{
				Line: line, Text: text,
				Reason: `must be written as "package==version" (for example "fastapi==0.115.0")`,
			})
			continue
		}
		name, version, _ := strings.Cut(text, "==")
		reqs = append(reqs, Requirement{Name: normalizePackageName(name), Version: version, Raw: text})
	}
	if err := scanner.Err(); err != nil {
		errs = append(errs, RequirementError{Reason: "could not read requirements.txt: " + err.Error()})
	}
	return reqs, errs
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
