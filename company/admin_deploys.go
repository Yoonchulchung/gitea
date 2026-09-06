// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"sort"

	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplAdminDeploys templates.TplName = "company/admin_deploys"

// adminDeployRow is one line of the admin app list: an app's live state
// joined with the repository it came from.
//
// Repo may be nil — a department repo can be renamed or deleted while its
// state file (and quite possibly its still-running app) remains. Hiding
// those rows would make exactly the situation that most needs attention
// invisible, so they are shown with the raw owner/repo instead of a link.
type adminDeployRow struct {
	State *AppState
	Repo  *repo_model.Repository // nil if the repo no longer exists

	// NeedsAttention drives the "조치 필요" section. Computed here rather
	// than in the template so the definition of "needs attention" lives in
	// one place as it grows (pending approvals, stuck queues, …).
	NeedsAttention bool
}

// AdminDeploys lists every department app and its current state.
//
// The list is the union of "has state on disk" and "is an org-owned repo",
// not just the former: a department that has never deployed still gets a
// row saying so. Otherwise a repo nobody ever deployed is indistinguishable
// from one that doesn't exist, and "why is my app not here?" becomes a
// support question instead of something the screen answers.
//
// Mounted inside the existing "/-/admin" group, so it inherits adminReq;
// no separate access check belongs here. Same as AdminActivity.
func AdminDeploys(ctx *context.Context) {
	states := ListAppStates()
	byKey := make(map[string]*AppState, len(states))
	for _, st := range states {
		byKey[st.Owner+"/"+st.Repo] = st
	}

	var repos []*repo_model.Repository
	if err := db.GetEngine(ctx).In("id", orgOwnedRepoIDs()).Find(&repos); err != nil {
		ctx.ServerError("list org-owned repos", err)
		return
	}

	rows := make([]*adminDeployRow, 0, len(repos)+len(states))
	seen := make(map[string]bool, len(repos))
	for _, repo := range repos {
		key := repo.OwnerName + "/" + repo.Name
		seen[key] = true
		st := byKey[key]
		if st == nil {
			st = &AppState{Owner: repo.OwnerName, Repo: repo.Name, Desired: AppStateStopped, Actual: AppStateStopped}
		}
		rows = append(rows, &adminDeployRow{State: st, Repo: repo, NeedsAttention: needsAttention(st)})
	}
	// State whose repo is gone (renamed or deleted). deployPathPrefix is
	// name-based, so a rename orphans the old app and leaves it running
	// under the old URL — surfacing it here is how that gets noticed.
	for _, st := range states {
		if !seen[st.Owner+"/"+st.Repo] {
			rows = append(rows, &adminDeployRow{State: st, NeedsAttention: true})
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].State.Owner != rows[j].State.Owner {
			return rows[i].State.Owner < rows[j].State.Owner
		}
		return rows[i].State.Repo < rows[j].State.Repo
	})

	attention := make([]*adminDeployRow, 0, len(rows))
	var running, stopped, failed int
	for _, r := range rows {
		if r.NeedsAttention {
			attention = append(attention, r)
		}
		switch r.State.Actual {
		case AppStateRunning:
			running++
		case AppStateFailed:
			failed++
		default:
			stopped++
		}
	}

	ctx.Data["Title"] = "App deployments"
	ctx.Data["Rows"] = rows
	ctx.Data["Attention"] = attention
	ctx.Data["CountTotal"] = len(rows)
	ctx.Data["CountRunning"] = running
	ctx.Data["CountStopped"] = stopped
	ctx.Data["CountFailed"] = failed
	ctx.HTML(http.StatusOK, tplAdminDeploys)
}

// needsAttention reports whether an app is in a state a human has to do
// something about. Deliberately narrow: the "조치 필요" section is only
// useful if it is empty most of the time — if it always has rows in it,
// nobody reads it when it matters.
func needsAttention(st *AppState) bool {
	switch st.Actual {
	case AppStateFailed, AppStateSuspended:
		return true
	}
	// Sandboxing is fail-closed at start time, so an app that is running
	// unsandboxed got there through an explicit admin opt-in — worth
	// keeping visible rather than letting it fade into the list.
	return st.Actual == AppStateRunning && !st.Sandboxed
}
