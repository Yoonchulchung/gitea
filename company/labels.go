// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sync"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/util"
	"gitea.dev/services/context"
	issue_service "gitea.dev/services/issue"
)

// deployDeptLabelName and deployProjectLabelName are the two labels every
// Deploy Request PR carries once it lands on the central deploy repo — one
// per department (org), one per the specific project (repo) it came from.
// Central-deploy aggregates every department's requests as same-repo PRs
// (deployBranchName, company/deploy.go) with no other structural way to
// tell them apart at a glance; these make the department and project
// filterable/visible directly in Gitea's own PR list and label sidebar,
// same as any other label.
//
// Deliberately repo-scoped labels on central-deploy itself, not
// organization-scoped labels on each department's own org: Gitea only
// offers an issue the labels belonging to its own repo plus (if any) that
// repo's *own* owning organization (routers/web/repo/issue_page_meta.go),
// and central-deploy is not owned by any department org — a "PO" label
// created on the PO organization would simply never be assignable to an
// issue living on central-deploy. Creating the label directly on
// central-deploy sidesteps that entirely.
func deployDeptLabelName(deptOwner string) string {
	return deptOwner
}

func deployProjectLabelName(deptOwner, deptName string) string {
	return deptOwner + "/" + deptName
}

// labelColorFor derives a stable, readable hex color from name — the same
// department/project always gets the same color across separate deploy
// requests without maintaining a color table by hand. Fixed
// saturation/lightness keep every generated color legible with the
// (typically dark) label text Gitea renders on top.
func labelColorFor(name string) string {
	sum := sha256.Sum256([]byte(name))
	hue := float64(sum[0]) / 255 * 360
	return hslToHex(hue, 0.55, 0.55)
}

func hslToHex(h, s, l float64) string {
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	to255 := func(v float64) int {
		return min(255, max(0, int(v*255+0.5)))
	}
	return fmt.Sprintf("#%02x%02x%02x", to255(r+m), to255(g+m), to255(b+m))
}

// ensureRepoLabelLocks serializes concurrent ensureRepoLabel calls for the
// same (repo, name) — two Deploy Requests for the same never-before-seen
// project landing close enough together would otherwise both run
// "look up by name → not found → insert" before either insert commits,
// creating two identically-named labels (Label has no DB-level unique
// constraint on (repo_id, name) to fall back on). Same pattern as
// workspaceTmpLocks (company/workspace_tmp.go); entries are never removed,
// bounded by how many distinct (repo, name) pairs this instance has ever
// touched — negligible at this scale.
var (
	ensureRepoLabelLocksMu sync.Mutex
	ensureRepoLabelLocks   = map[string]*sync.Mutex{}
)

func ensureRepoLabelLockFor(key string) *sync.Mutex {
	ensureRepoLabelLocksMu.Lock()
	defer ensureRepoLabelLocksMu.Unlock()
	l, ok := ensureRepoLabelLocks[key]
	if !ok {
		l = &sync.Mutex{}
		ensureRepoLabelLocks[key] = l
	}
	return l
}

// ensureRepoLabel gets or creates a repo-scoped label named exactly name on
// repo — idempotent, so every Deploy Request for the same department/
// project reuses the one label created the first time rather than piling
// up duplicates. description is only used the first time the label is
// created; an existing label's own description/color is left alone (an
// admin may have deliberately edited either after the fact).
func ensureRepoLabel(ctx *context.Context, repo *repo_model.Repository, name, description string) (*issues_model.Label, error) {
	lock := ensureRepoLabelLockFor(fmt.Sprintf("%d:%s", repo.ID, name))
	lock.Lock()
	defer lock.Unlock()

	l, err := issues_model.GetLabelInRepoByName(ctx, repo.ID, name)
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, util.ErrNotExist) {
		return nil, err
	}
	l = &issues_model.Label{
		RepoID:      repo.ID,
		Name:        name,
		Description: description,
		Color:       labelColorFor(name),
	}
	if err := issues_model.NewLabel(ctx, l); err != nil {
		return nil, err
	}
	return l, nil
}

// applyDeployLabels tags pullIssue (a just-opened Deploy Request PR on
// central) with its department and project labels — see
// deployDeptLabelName/deployProjectLabelName above. Best-effort: labeling
// is purely an admin-convenience filter on top of the PR that already
// carries the real deploy content, so it must never fail or block the
// submission itself the way the diff snapshot/PR creation above must —
// same reasoning as postAIReviewComment (company/deploy.go).
func applyDeployLabels(ctx *context.Context, central, deptRepo *repo_model.Repository, pullIssue *issues_model.Issue, doer *user_model.User) {
	deptLabel, err := ensureRepoLabel(ctx, central, deployDeptLabelName(deptRepo.OwnerName),
		fmt.Sprintf("Deploy Requests from the %s department", deptRepo.OwnerName))
	if err != nil {
		log.Error("company: applyDeployLabels: ensure department label: %v", err)
		return
	}
	projectLabel, err := ensureRepoLabel(ctx, central, deployProjectLabelName(deptRepo.OwnerName, deptRepo.Name),
		fmt.Sprintf("Deploy Requests from %s", deptRepo.FullName()))
	if err != nil {
		log.Error("company: applyDeployLabels: ensure project label: %v", err)
		return
	}
	if err := issue_service.AddLabels(ctx, pullIssue, doer, []*issues_model.Label{deptLabel, projectLabel}); err != nil {
		log.Error("company: applyDeployLabels: AddLabels: %v", err)
	}
}
