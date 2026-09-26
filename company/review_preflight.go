// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"sync"

	issues_model "gitea.dev/models/issues"
	"gitea.dev/modules/git"
	gitea_context "gitea.dev/services/context"
)

// The submission form refuses a request that cannot deploy (company/preflight.go).
// That answer was given to the requester, by whatever version of the platform
// was running then: a request raised before a check existed never met it, and
// one left open while its department pushed something else is judged on what
// it froze. The approval is a second decision, made later, by somebody else —
// so it gets the same checks, run again, against the snapshot approving it
// would build.
//
// The same checks, not a second list of them: both call runPreflightOn and
// differ only in which files they hand it. A check added to the preflight is
// a check this screen gains, which is the only way a promise like "everything
// that could go wrong is visible before approving" survives the next feature.

// reviewPreflightCache keeps one report per snapshot.
//
// The expensive part resolves the dependency tree with pip, which takes
// seconds — too slow for a page an administrator reloads while deciding. The
// key is the head commit, and a request's head commit does not change without
// becoming a different snapshot, so a cached report is never stale: it either
// describes these exact files or it is not used.
var reviewPreflight sync.Map // head commit id → PreflightResult

// PreflightForRequest is the report for the version this request carries.
func PreflightForRequest(ctx *gitea_context.Context, gitRepo *git.Repository, pr *issues_model.PullRequest, deptOwner, deptName string) PreflightResult {
	head, err := gitRepo.GetBranchCommit(ctx, pr.HeadBranch)
	if err != nil {
		// The branch is gone — the request is decided, and there is nothing
		// left to check.
		return PreflightResult{}
	}
	key := head.ID.String()
	if cached, found := reviewPreflight.Load(key); found {
		return cached.(PreflightResult)
	}
	prefix := deployPathPrefix(deptOwner, deptName)
	result := runPreflightOn(ctx, deptOwner, deptName, func(path string) string {
		return deployRequestFile(ctx, gitRepo, pr.HeadBranch, prefix, path)
	})
	reviewPreflight.Store(key, result)
	return result
}
