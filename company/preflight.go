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
		fail("서버 파이썬", "서버에 파이썬이 준비되어 있지 않습니다. 부서에서 고칠 수 있는 문제가 아니니 관리자에게 알려 주세요.")
		result.Summary = "서버 문제로 지금은 배포할 수 없습니다."
		return result
	}
	ok("서버 파이썬", "Python "+info.Version+" 에서 실행됩니다.")

	// The start command runs `main:app`, so a missing main.py is the one part
	// of the app contract that can be checked without running anything.
	if readRepoFile(ctx, repo, "main.py") == "" {
		fail("main.py", "저장소 최상위에 main.py 가 없습니다. 앱은 main.py 안의 app 을 실행합니다.")
	} else {
		ok("main.py", "찾았습니다.")
	}

	requirements := readRepoFile(ctx, repo, "requirements.txt")
	if requirements == "" {
		ok("requirements.txt", "없습니다 — 플랫폼이 제공하는 패키지만 사용합니다: "+joinOrDash(settings.BasePackages))
		result.Summary = summarize(result.Deployable, nil)
		return result
	}

	if _, errs := ParseRequirements(requirements); len(errs) > 0 {
		fail("requirements.txt 형식", formatRequirementErrors(errs, settings.BasePackages))
		result.Summary = summarize(result.Deployable, nil)
		return result
	}
	ok("requirements.txt 형식", "각 줄이 올바르게 적혀 있습니다.")

	// The expensive one, and the reason this exists: pip resolves the whole
	// tree against this host's Python and says whether the versions asked for
	// can exist together.
	resolved, err := resolveDependencies(ctx, requirements, settings.BasePackages)
	if err != nil {
		if resolveErr, isResolve := err.(*resolveError); isResolve && !resolveErr.unreachable {
			fail("패키지 버전", "요청한 버전들을 함께 설치할 수 없습니다.\n"+resolveFailureSummary(resolveErr.output))
		} else {
			// The platform's problem, not theirs. Reported as unchecked rather
			// than as a failure, because telling someone their code is broken
			// when the index was briefly down is worse than saying nothing.
			warn("패키지 버전", "지금은 확인할 수 없습니다 (패키지 저장소에 연결하지 못했습니다). 배포 요청은 그대로 제출할 수 있습니다.")
		}
		result.Summary = summarize(result.Deployable, nil)
		return result
	}

	for _, item := range packageRequests(resolved, settings.AllowedPackages()) {
		result.NeedsApproval = append(result.NeedsApproval, item.Value)
	}
	if len(result.NeedsApproval) > 0 {
		// Not a failure: this is exactly what a deploy request is for. But the
		// answer depends on someone else, and that is worth knowing before
		// submitting rather than after waiting.
		warn("패키지 승인", "설치는 가능하지만 아래 패키지는 관리자 승인이 필요합니다: "+joinOrDash(result.NeedsApproval))
	} else {
		ok("패키지 승인", "필요한 패키지가 모두 승인되어 있습니다.")
	}
	ok("패키지 버전", "요청한 패키지를 이 서버의 파이썬에 설치할 수 있습니다.")

	result.Summary = summarize(result.Deployable, result.NeedsApproval)
	return result
}

func summarize(deployable bool, needsApproval []string) string {
	switch {
	case !deployable:
		return "지금 배포하면 실패합니다. 아래 항목을 고친 뒤 요청해 주세요."
	case len(needsApproval) > 0:
		return "코드는 문제없습니다. 관리자가 패키지를 승인하면 배포됩니다."
	default:
		return "배포할 수 있는 상태입니다."
	}
}

func joinOrDash(items []string) string {
	if len(items) == 0 {
		return "없음"
	}
	return strings.Join(items, ", ")
}
