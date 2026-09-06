// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/services/context"
)

// Until now the first thing that told a department their app would not build
// was a failed deploy — after they had written a request, after an admin had
// read it, and after that admin had approved something that then did not
// work. The approval was spent on an answer nobody had yet.
//
// So the same checks the build performs are offered before the request is
// made. Every one of them is a fact the platform already has: whether the
// host has an interpreter, whether the file the start command needs exists,
// whether the requirements are well-formed, and whether the versions asked
// for can be installed together on this Python. That last one is the
// "버전 문제" this exists for, and it is the only one that costs anything to
// answer — pip has to resolve the tree, which takes seconds.
//
// Nothing is installed and nothing runs. This resolves and inspects; the
// build is still what proves it.

// preflightStatus is how one check came out.
const (
	preflightOK    = "ok"
	preflightWarn  = "warn"  // cannot be checked from here, not necessarily wrong
	preflightError = "error" // this will fail the deploy
)

// PreflightCheck is one line of the report.
type PreflightCheck struct {
	Label  string `json:"label"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// PreflightResult is the whole report.
type PreflightResult struct {
	Deployable bool             `json:"deployable"`
	Summary    string           `json:"summary"`
	Checks     []PreflightCheck `json:"checks"`
	// NeedsApproval names packages that resolve but are not allowed yet. Not
	// a failure — it is what the request is for — but someone should know
	// before submitting that the answer depends on an admin.
	NeedsApproval []string `json:"needsApproval"`
}

func runPreflight(ctx *context.Context, repo *repo_model.Repository) PreflightResult {
	// Translated here rather than carried as keys: this report is built inside
	// a request and shown once, so the reader's language is already known and
	// there is nothing to store.
	tr := ctx.Locale.TrString
	settings := SettingsFor(repo.OwnerName, repo.Name)
	result := PreflightResult{Deployable: true}

	fail := func(label, detail string) {
		result.Checks = append(result.Checks, PreflightCheck{Label: label, Status: preflightError, Detail: detail})
		result.Deployable = false
	}
	ok := func(label, detail string) {
		result.Checks = append(result.Checks, PreflightCheck{Label: label, Status: preflightOK, Detail: detail})
	}
	warn := func(label, detail string) {
		result.Checks = append(result.Checks, PreflightCheck{Label: label, Status: preflightWarn, Detail: detail})
	}

	// The interpreter first: without it nothing else matters, and it is the
	// one failure a department cannot do anything about.
	info, pythonErr := pythonProbe()
	if pythonErr != nil {
		fail(tr("company.preflight.python"), tr("company.preflight.python_missing"))
		result.Summary = tr("company.preflight.server_problem")
		return result
	}
	ok(tr("company.preflight.python"), tr("company.preflight.python_ok", info.Version))

	// The start command runs `main:app`, so a missing main.py is the one part
	// of the app contract that can be checked without running anything.
	if readRepoFile(ctx, repo, "main.py") == "" {
		fail(tr("company.preflight.main_py"), tr("company.preflight.main_py_missing"))
	} else {
		ok(tr("company.preflight.main_py"), tr("company.preflight.main_py_ok"))
	}

	requirements := readRepoFile(ctx, repo, "requirements.txt")
	if requirements == "" {
		ok(tr("company.preflight.requirements"), tr("company.preflight.requirements_none", joinOrDash(ctx, settings.BasePackages)))
		result.Summary = summarize(ctx, result.Deployable, nil)
		return result
	}

	if _, errs := ParseRequirements(requirements); len(errs) > 0 {
		fail(tr("company.preflight.requirements_shape"), formatRequirementErrors(ctx.Locale, errs, settings.BasePackages))
		result.Summary = summarize(ctx, result.Deployable, nil)
		return result
	}
	ok(tr("company.preflight.requirements_shape"), tr("company.preflight.requirements_shape_ok"))

	// The expensive one, and the reason this exists: pip resolves the whole
	// tree against this host's Python and says whether the versions asked for
	// can exist together.
	resolved, err := resolveDependencies(ctx, requirements, settings.BasePackages)
	if err != nil {
		if resolveErr, isResolve := err.(*resolveError); isResolve && !resolveErr.unreachable {
			fail(tr("company.preflight.versions"), tr("company.preflight.versions_conflict")+"\n"+resolveFailureSummary(resolveErr.output))
		} else {
			// The platform's problem, not theirs. Reported as unchecked rather
			// than as a failure, because telling someone their code is broken
			// when the index was briefly down is worse than saying nothing.
			warn(tr("company.preflight.versions"), tr("company.preflight.versions_unchecked"))
		}
		result.Summary = summarize(ctx, result.Deployable, nil)
		return result
	}

	for _, item := range packageRequests(resolved, settings.AllowedPackages()) {
		result.NeedsApproval = append(result.NeedsApproval, item.Value)
	}
	if len(result.NeedsApproval) > 0 {
		// Not a failure: this is exactly what a deploy request is for. But the
		// answer depends on someone else, and that is worth knowing before
		// submitting rather than after waiting.
		warn(tr("company.preflight.approval"), tr("company.preflight.approval_needed", joinOrDash(ctx, result.NeedsApproval)))
	} else {
		ok(tr("company.preflight.approval"), tr("company.preflight.approval_ok"))
	}
	ok(tr("company.preflight.versions"), tr("company.preflight.versions_ok"))

	result.Summary = summarize(ctx, result.Deployable, result.NeedsApproval)
	return result
}

func summarize(ctx *context.Context, deployable bool, needsApproval []string) string {
	switch {
	case !deployable:
		return ctx.Locale.TrString("company.preflight.summary_fail")
	case len(needsApproval) > 0:
		return ctx.Locale.TrString("company.preflight.summary_approval")
	default:
		return ctx.Locale.TrString("company.preflight.summary_ok")
	}
}

func joinOrDash(ctx *context.Context, items []string) string {
	if len(items) == 0 {
		return ctx.Locale.TrString("company.preflight.none")
	}
	return strings.Join(items, ", ")
}
