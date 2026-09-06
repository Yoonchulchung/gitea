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
}

func (e *userError) Error() string { return e.staff }

// userErrorf builds an error whose text suits either audience. Use it only
// for sentences a non-developer can act on — never to wrap an error that
// came from the filesystem, git, or an external command.
func userErrorf(format string, a ...any) error {
	return &userError{staff: fmt.Sprintf(format, a...)}
}

// audienceError builds an error that says different things to a department
// and to an administrator, because the thing each of them should do next is
// different.
func audienceError(staff, admin string) error {
	return &userError{staff: staff, admin: admin}
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
// line is all an admin gets once the original error is dropped.
func DepartmentSafeError(context string, err error) string {
	if err == nil {
		return ""
	}
	if msg, ok := userMessage(err); ok {
		return msg
	}
	log.Error("company: %s: %v", context, err)
	return "처리 중 문제가 발생했습니다. 관리자에게 문의해 주세요."
}

// AdminError returns the message for an administrator.
//
// Nothing is withheld — an admin gets the raw error when there is no written
// sentence — but where one audience's advice would be wrong for the other,
// the admin's version is used. Telling an operator to press "Deploy Request"
// describes the department's job, not theirs.
func AdminError(err error) string {
	if err == nil {
		return ""
	}
	if u, ok := asUserError(err); ok {
		if u.admin != "" {
			return u.admin
		}
		return u.staff
	}
	return err.Error()
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
		text = strings.ReplaceAll(text, root, "<앱데이터>")
	}
	// Anything still absolute and pointing at a home directory is redacted
	// wholesale: those are the paths that vary per install and leak layout.
	return absolutePathPattern.ReplaceAllStringFunc(text, func(match string) string {
		if strings.HasPrefix(match, "/home/") || strings.HasPrefix(match, "/Users/") || strings.HasPrefix(match, "/root/") {
			return "<서버경로>"
		}
		return match
	})
}
