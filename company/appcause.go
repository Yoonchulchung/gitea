// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import "strings"

// A department can start and stop its own app, so it has to be able to find
// out why the app stopped — otherwise every failure becomes a question for
// an administrator and the boundary this platform draws stops meaning
// anything.
//
// But raw logs cannot be the answer: an app prints whatever it prints, which
// for a department app includes the data it handles, and everyone with read
// access to the repository would see it. So the department gets a
// *classified* cause in plain language, and the raw output stays on the
// admin pages.
//
// Each cause carries what to do next, because knowing the reason without
// knowing the remedy still ends in a support request. See
// docs/company/app-platform.md.

// AppCause is what a non-developer sees when their app is not working.
//
// Fixed sentences travel as locale keys and are translated where they are
// rendered, so the reader's own language setting decides what they see. Text
// that is data — an admin's typed reason, the line pip printed — travels as
// text: it was written in whatever language its author used, and a locale
// file cannot know it.
type AppCause struct {
	// Summary is a locale key, written for someone who has never read a
	// stack trace.
	Summary string
	// Detail is a locale key for a fixed explanation, or "" when DetailText
	// carries the message instead.
	Detail string
	// DetailText is verbatim content — a package name, an admin's reason.
	// Never log output.
	DetailText string
	// DetailArg is the argument for Detail when its key takes one. A single
	// value rather than a slice because a template cannot spread one: passing
	// []any{192} to Tr formats "%d" as "[192]".
	DetailArg any
	// Action and ActionLabel (a locale key) point at the one thing that
	// fixes it. Empty when the fix is in the department's own code, where no
	// button helps.
	Action      string
	ActionLabel string
	// AdminHint is a locale key for what an *operator* should do about this,
	// which is often not what the department should do.
	AdminHint string
	// At is when this failure happened. "It is broken" and "it broke four
	// minutes ago" are different pieces of information, and the second is
	// what tells someone whether their last change caused it.
	At int64
}

// DepartmentCause explains an app's current problem, or returns nil when
// there isn't one. Nil is the normal case, and the screen shows nothing at
// all for it — a panel that is always saying something stops being read.
func DepartmentCause(st *AppState) *AppCause {
	cause := departmentCause(st)
	if cause == nil {
		return nil
	}
	cause.At = st.FailedAt
	if cause.At == 0 {
		// State written before failures carried their own timestamp. UpdatedAt
		// is the wrong answer in general — it moves on any change, so pressing
		// start on a broken app drags it forward — but for a record that has
		// nothing better it beats showing no time at all.
		cause.At = st.UpdatedAt
	}
	return cause
}

func departmentCause(st *AppState) *AppCause {
	// Keyed on the reason, not on Actual. A build that fails leaves the
	// previous version running — that is the point of swapping last — so the
	// app is Running *and* something went wrong, and keying on Actual would
	// hide the failure precisely when the department needs to see it. The
	// same held for a successful rollback, which is Running with a reason
	// worth reading.
	//
	// Reasons are cleared the moment a deploy or start succeeds, so a stale
	// one cannot linger here.
	if st.Reason == "" {
		return nil
	}
	switch st.Reason {
	case ReasonInstallFailed:
		return &AppCause{
			Summary:   "company.app.cause.install_failed",
			AdminHint: "company.app.cause.install_failed.admin",
			// Never st.Message: that is the raw pip output, which runs to
			// dozens of lines of download progress and is admin-only by
			// design. What a department needs is the one line naming the
			// package that could not be found.
			DetailText: summarizeInstallFailure(st.Message),
			// No button: a version that does not exist is fixed in
			// requirements.txt, and an approval request here would ask an
			// admin to approve a package nobody can install.
		}
	case ReasonPackageDenied:
		return &AppCause{
			Summary:     "company.app.cause.package_denied",
			AdminHint:   "company.app.cause.package_denied.admin",
			DetailText:  st.UserMessage,
			Action:      "deploy",
			ActionLabel: "company.app.cause.package_denied.action",
		}
	case ReasonOOM:
		return &AppCause{
			Summary:   "company.app.cause.oom",
			AdminHint: "company.app.cause.oom.admin",
			// The watchdog writes a key here: it runs with no reader and so no
			// language of its own.
			Detail:     st.UserMessageKey,
			DetailArg:  st.UserMessageArg,
			DetailText: st.UserMessage,

			Action:      "deploy",
			ActionLabel: "company.app.cause.oom.action",
		}
	case ReasonHealthTimeout:
		return &AppCause{
			Summary:   "company.app.cause.health_timeout",
			AdminHint: "company.app.cause.health_timeout.admin",
			Detail:    "company.app.cause.health_timeout.detail",
		}
	case ReasonCrashLoop:
		return &AppCause{
			Summary: "company.app.cause.crash_loop",
			Detail:  "company.app.cause.crash_loop.detail",
		}
	case ReasonSuspended:
		return &AppCause{
			Summary: "company.app.cause.suspended",
			// The reason is the entire value here: "an administrator stopped
			// it" with no explanation leaves the department with nothing to
			// act on and no idea who to ask about what. An admin types it, so
			// it is department-safe by definition — and it is their words,
			// not the platform's, so it is not translated.
			DetailText: st.UserMessage,
		}
	case ReasonNoPython:
		return &AppCause{
			Summary:   "company.app.cause.no_python",
			Detail:    "company.app.cause.no_python.detail",
			AdminHint: "company.app.cause.no_python.admin",
		}
	case ReasonSandboxUnavailable:
		return &AppCause{
			Summary:   "company.app.cause.sandbox_unavailable",
			AdminHint: "company.app.cause.sandbox_unavailable.admin",
			Detail:    "company.app.cause.sandbox_unavailable.detail",
		}
	case ReasonSecretError:
		return &AppCause{
			Summary:     "company.app.cause.secret_error",
			Detail:      "company.app.cause.secret_error.detail",
			Action:      "app",
			ActionLabel: "company.app.cause.secret_error.action",
		}
	case ReasonDeployQueueFull:
		return &AppCause{
			Summary: "company.app.cause.queue_full",
			Detail:  "company.app.cause.queue_full.detail",
		}
	case ReasonNoRelease:
		// Only ever reached when no deploy has been attempted at all — a
		// failed one keeps its own reason (company/appproc.go), because that
		// is the thing to fix and this sentence is not.
		return &AppCause{
			Summary:   "company.app.cause.no_release",
			AdminHint: "company.app.cause.no_release.admin",
			Detail:    "company.app.cause.no_release.detail",
			Action:    "deploy", ActionLabel: "company.app.cause.no_release.action",
		}
	case ReasonContractViolation:
		return &AppCause{
			Summary: "company.app.cause.contract_violation",
			// Only ever the deliberately-written half. The first version of
			// this read st.Message on the grounds that those messages "are
			// written for a department", which was true of some of them and
			// not of the filesystem errors that also land here.
			DetailText: st.UserMessage,
		}
	case ReasonRolledBack:
		return &AppCause{
			Summary: "company.app.cause.rolled_back",
			Detail:  "company.app.cause.rolled_back.detail",
		}
	default:
		// An unclassified failure must not fall back to st.Message: that is
		// admin-only detail and can carry absolute paths or build output.
		// Reaching here means a reason code was added without a sentence to
		// go with it.
		return &AppCause{
			Summary: "company.app.cause.unknown",
			Detail:  "company.app.cause.unknown.detail",
		}
	}
}

// summarizeInstallFailure pulls the actionable lines out of pip's output.
//
// pip prints one "Collecting …" and "Downloading …" line per package before
// it gets to the problem, so the raw text is mostly noise; the lines that
// start with ERROR are the ones that say what to change. Bounded in both
// count and length, because this is rendered in a sidebar panel and a
// department that has to scroll through a build log has been handed the
// admin's job.
func summarizeInstallFailure(message string) string {
	var errs []string
	for line := range strings.SplitSeq(message, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ERROR:") {
			continue
		}
		line = strings.TrimPrefix(line, "ERROR: ")
		// pip labels these ERROR but they are not the failure — they are
		// notes about versions it declined to consider along the way, and
		// they push the line that matters off the panel.
		if strings.HasPrefix(line, "Ignored the following") {
			continue
		}
		// pip appends the full list of available versions, which can be
		// hundreds of characters and helps nobody here.
		if idx := strings.Index(line, " (from versions:"); idx > 0 {
			line = line[:idx]
		}
		// Its own errors can name a path — "Permission denied:
		// '/home/git/gitea/data/…'" — so even this filtered subset is
		// scrubbed before it leaves.
		errs = append(errs, RedactServerPaths(line))
		if len(errs) == installErrorLines {
			break
		}
	}
	if len(errs) == 0 {
		return platformLocale().TrString("company.cause.check_requirements")
	}
	return strings.Join(errs, "\n") + "\n" + platformLocale().TrString("company.cause.fix_and_redeploy")
}

// installErrorLines caps how much of a failed build reaches the department.
// Two, because pip usually says the same thing twice — "could not find a
// version that satisfies" and "no matching distribution found" — and a
// sidebar panel is not a log viewer.
const installErrorLines = 2
