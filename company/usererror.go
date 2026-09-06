// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/translation"
)

// Errors from the app platform reach two very different audiences, and most
// of them are not safe for the second one.
//
// Go's os errors are *fs.PathError: every one carries an absolute path, so
// `os.Readlink(current)` failing produces
// "readlink /home/git/gitea/data/company-apps/9646…/current: no such file or
// directory". Handing that to a department tells them nothing they can act
// on while telling them exactly where Gitea keeps its data — the same data
// the sandbox exists to keep away from their apps.
//
// The rule here is opt-in disclosure, not redaction. Redaction means
// enumerating every shape a leak can take and losing the day one is missed.
// Instead a message reaches a department only if it was deliberately written
// for one, and everything else becomes a generic sentence plus a log line an
// administrator can read.

// userError marks a message written to be shown to a person rather than
// logged, and records who it was written for.
//
// Being safe to show is not the same as being addressed to the reader.
// "Press Deploy Request and an administrator will approve it" is perfectly
// safe, and useless to the administrator who would be doing the approving —
// so the two audiences get their own sentences whenever the advice differs.
type userError struct {
	staff string
	admin string // falls back to staff when the advice is the same for both

	// The key-based form. An error is raised deep in the stack, far from any
	// request and therefore from any language setting, so it cannot be
	// translated where it is created — it carries the key and the arguments,
	// and whoever finally shows it to a person translates it in *that*
	// person's language. The strings above are the pre-i18n form and remain
	// the fallback while call sites migrate.
	staffKey string
	adminKey string
	// Arguments per audience, never shared: the admin half regularly carries
	// a path or a raw tool error, and formatting the staff sentence with the
	// admin's arguments would either garble it (%!(EXTRA …)) or hand the
	// department the very detail the split exists to withhold.
	staffArgs []any
	adminArgs []any
}

// Error returns the English rendering, which is what lands in logs — a log
// line in whatever language a request happened to arrive in is a log an
// operator cannot grep.
func (e *userError) Error() string {
	if e.staffKey != "" {
		return translation.NewLocale("en-US").TrString(e.staffKey, e.staffArgs...)
	}
	return e.staff
}

// userErrorf builds an error whose text suits either audience. Use it only
// for sentences a non-developer can act on — never to wrap an error that
// came from the filesystem, git, or an external command.
func userErrorf(format string, a ...any) error {
	return &userError{staff: fmt.Sprintf(format, a...)}
}

// userKeyError is the translated form: a locale key plus its arguments,
// rendered in the reader's language at the moment of display.
func userKeyError(key string, args ...any) error {
	return &userError{staffKey: key, staffArgs: args}
}

// audienceError builds an error that says different things to a department
// and to an administrator, because the thing each of them should do next is
// different.
func audienceError(staff, admin string) error {
	return &userError{staff: staff, admin: admin}
}

// audienceKeyError is audienceError in locale keys. The variadic arguments
// belong to the admin sentence alone — see the field comment above.
func audienceKeyError(staffKey, adminKey string, adminArgs ...any) error {
	return &userError{staffKey: staffKey, adminKey: adminKey, adminArgs: adminArgs}
}

// render translates one half of the error for a locale, falling back through
// the legacy string.
func (e *userError) render(l translation.Locale, key, legacy string, args []any) string {
	if key != "" {
		return l.TrString(key, args...)
	}
	return legacy
}

func asUserError(err error) (*userError, bool) {
	if err == nil {
		return nil, false
	}
	u := new(*userError)
	if errors.As(err, u) {
		return *u, true
	}
	return nil, false
}

// userMessage returns the department-safe text of err, if it has any.
func userMessage(err error) (string, bool) {
	if u, ok := asUserError(err); ok {
		return u.staff, true
	}
	return "", false
}

// DepartmentSafeError returns something a department can be shown, and logs
// what actually went wrong so an administrator still has it.
//
// context should name the operation ("starting PO/report"), because the log
// line is all an admin gets once the original error is dropped. Renders in
// Korean for callers that have no locale; handlers should prefer
// DepartmentSafeErrorL.
func DepartmentSafeError(context string, err error) string {
	return DepartmentSafeErrorL(translation.NewLocale("ko-KR"), context, err)
}

// DepartmentSafeErrorL is DepartmentSafeError in the reader's own language.
func DepartmentSafeErrorL(l translation.Locale, context string, err error) string {
	if err == nil {
		return ""
	}
	if u, ok := asUserError(err); ok {
		return u.render(l, u.staffKey, u.staff, u.staffArgs)
	}
	log.Error("company: %s: %v", context, err)
	return l.TrString("company.error.generic")
}

// AdminError returns the message for an administrator.
//
// Nothing is withheld — an admin gets the raw error when there is no written
// sentence — but where one audience's advice would be wrong for the other,
// the admin's version is used. Telling an operator to press "Deploy Request"
// describes the department's job, not theirs.
func AdminError(err error) string {
	return AdminErrorL(translation.NewLocale("ko-KR"), err)
}

// AdminErrorL is AdminError in the reader's own language.
func AdminErrorL(l translation.Locale, err error) string {
	if err == nil {
		return ""
	}
	if u, ok := asUserError(err); ok {
		if u.adminKey != "" || u.admin != "" {
			return u.render(l, u.adminKey, u.admin, u.adminArgs)
		}
		return u.render(l, u.staffKey, u.staff, u.staffArgs)
	}
	return err.Error()
}

// platformLocale is the language for text the platform composes with no
// reader in front of it — a build worker's failure message, a watchdog's
// note. Those are shown to someone later, so English would be wrong for a
// Korean instance and Korean wrong for an English one; the instance's own
// configured language is the only answer available at that point.
//
// Distinct from Error(), which stays English: that goes to the log, and a log
// line whose language depends on configuration is one an operator cannot
// grep.
func platformLocale() translation.Locale {
	if len(setting.Langs) > 0 && setting.Langs[0] != "" {
		return translation.NewLocale(setting.Langs[0])
	}
	return translation.NewLocale("en-US")
}

// absolutePathPattern matches a unix path deep enough to be a real location
// on this server rather than an incidental "/" in prose.
var absolutePathPattern = regexp.MustCompile(`(?:/[\w.@+-]+){2,}/?`)

// RedactServerPaths strips absolute paths out of text that came from a tool
// rather than from us.
//
// Belt to userError's braces, for the one case where a department genuinely
// needs an external tool's own words: pip's "ERROR:" lines name the package
// that could not be installed, which is exactly what they have to fix — but
// the same lines can read "Permission denied: '/home/git/gitea/data/…'".
// The useful half is kept and the path is not.
func RedactServerPaths(text string) string {
	if text == "" {
		return text
	}
	// Only paths that actually point into this instance are worth hiding;
	// "/usr/lib/python3" tells nobody anything they could not guess, and
	// blanking every slash-separated token would mangle package names.
	// Longest first: AppDataPath usually sits inside AppWorkPath, and
	// replacing the shorter one first would leave the tail of the longer.
	sensitive := []string{setting.AppDataPath, setting.CustomPath, setting.AppWorkPath}
	slices.SortFunc(sensitive, func(a, b string) int { return len(b) - len(a) })
	for _, root := range sensitive {
		if root == "" {
			continue
		}
		// A named marker rather than an ellipsis: a traceback with
		// "<앱데이터>/company-apps/…/uvicorn/server.py" still reads as a path,
		// which is most of what makes a traceback useful.
		text = strings.ReplaceAll(text, root, "<app-data>")
	}
	// Anything still absolute and pointing at a home directory is redacted
	// wholesale: those are the paths that vary per install and leak layout.
	return absolutePathPattern.ReplaceAllStringFunc(text, func(match string) string {
		if strings.HasPrefix(match, "/home/") || strings.HasPrefix(match, "/Users/") || strings.HasPrefix(match, "/root/") {
			return "<server-path>"
		}
		return match
	})
}
