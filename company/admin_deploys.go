// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

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

	// NetworkOpen flags the explicit exception: this app may connect
	// anywhere, which resurrects the exfiltration path the sandbox closes.
	// Flagged on the list because an exception nobody sees stops being an
	// exception.
	NetworkOpen bool

	// Sparkline is the last 24 hours of request counts, one point per bucket.
	// A number on its own — "1.2k requests" — cannot say whether that is
	// normal for this app; a shape can, which is the whole reason it is here
	// rather than another column of digits.
	// Sparkline is the polyline's "x,y …" attribute, computed here rather
	// than with template arithmetic: the maths is testable in Go and a
	// division by zero in a template is a blank page at render time.
	Sparkline string
	Requests  int
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

	// One window for the whole page, so every sparkline and the tiles above
	// them describe the same period.
	since := time.Now().Add(-24 * time.Hour)

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
		row := &adminDeployRow{
			State: st, Repo: repo, NeedsAttention: needsAttention(st),
			NetworkOpen: SettingsFor(st.Owner, st.Repo).Network.Mode == NetworkOpen,
		}
		// Only for apps that exist: LoadMetrics on a repository that was never
		// deployed reads a file that is not there, once per row.
		if byKey[key] != nil {
			row.Sparkline, row.Requests = requestSparkline(st.Owner, st.Repo, since)
		}
		rows = append(rows, row)
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

	// The build environment, on the page an admin already opens — a missing
	// interpreter or a broken base package list stops every deploy, and
	// finding that out through a failed deploy is finding out too late.
	pythonOK, pythonDetail := PythonStatus()
	sandboxOK, sandboxDetail := SandboxStatus()
	_, _, configErr := AppsConfigSnapshot()

	ctx.Data["PythonOK"] = pythonOK
	ctx.Data["PythonDetail"] = pythonDetail
	ctx.Data["SandboxOK"] = sandboxOK
	ctx.Data["SandboxDetail"] = sandboxDetail
	ctx.Data["ConfigError"] = configErr

	ctx.Data["Title"] = "App deployments"
	ctx.Data["Rows"] = rows
	ctx.Data["Attention"] = attention
	ctx.Data["CountTotal"] = len(rows)
	ctx.Data["Fleet"] = summarizeFleet(rows, since)
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

// fleetSummary is the headline for every app at once.
//
// The list already answered "is this app all right"; nobody could answer "is
// the platform all right" without reading every row. These are the four
// numbers that do, and an error rate or a p95 that is only visible per app is
// one nobody looks at until someone complains.
type fleetSummary struct {
	Requests  int
	Errors    int
	ErrorRate float64
	P95       int
	// Measured is false where this host cannot sample; the memory figure is
	// then absent rather than zero (company/appsample.go).
	Measured bool
	MemMaxMB int
}

// requestSparkline returns one app's request shape and total.
func requestSparkline(owner, repo string, since time.Time) (string, int) {
	buckets := LoadMetrics(owner, repo, since)
	points := make([]int, 0, len(buckets))
	total := 0
	for _, b := range buckets {
		points = append(points, b.Req.Total)
		total += b.Req.Total
	}
	return sparklinePoints(points), total
}

// sparklinePoints maps counts onto the 100x20 viewBox the template draws in.
//
// A flat or single-point series still returns a baseline: a row with no line
// at all reads as broken rather than as quiet, and "this app had no traffic"
// is information.
func sparklinePoints(counts []int) string {
	if len(counts) == 0 {
		return ""
	}
	peak := 0
	for _, c := range counts {
		peak = max(peak, c)
	}
	if len(counts) == 1 {
		counts = []int{counts[0], counts[0]}
	}

	var b strings.Builder
	for i, c := range counts {
		x := float64(i) * 100 / float64(len(counts)-1)
		y := 19.0 // the baseline, which is where every point sits when peak is 0
		if peak > 0 {
			y = 19 - float64(c)*18/float64(peak)
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	return b.String()
}

// summarizeFleet folds every app's buckets into the tiles.
func summarizeFleet(rows []*adminDeployRow, since time.Time) fleetSummary {
	var out fleetSummary
	var p95Sum, p95Count int
	for _, row := range rows {
		if row.State == nil {
			continue
		}
		summary := SummarizeMetrics(LoadMetrics(row.State.Owner, row.State.Repo, since))
		out.Requests += summary.Requests
		// ErrorRate is a percentage of that app's own requests; the fleet's
		// rate has to come from counts, or a quiet app with one failure would
		// drag the whole number as hard as a busy one with a hundred.
		out.Errors += int(summary.ErrorRate * float64(summary.Requests) / 100)
		if summary.P95 > 0 {
			p95Sum += summary.P95
			p95Count++
		}
		if summary.ResourcesMeasured {
			out.Measured = true
			out.MemMaxMB = max(out.MemMaxMB, summary.MemMaxMB)
		}
	}
	if out.Requests > 0 {
		out.ErrorRate = float64(out.Errors) * 100 / float64(out.Requests)
	}
	if p95Count > 0 {
		out.P95 = p95Sum / p95Count
	}
	return out
}
