// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

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

	// Data is how much of its storage allowance this app has spent. Shown on
	// the list because a department that has filled it cannot deploy, and an
	// administrator finding that out one app at a time finds it out late.
	Data    AppDataUsage
	HasData bool

	// Memory: what it is allowed, what it holds now, the most it held in
	// the window — beside each other, so a limit reads against evidence.
	MemLimitMB int
	MemNowMB   int
	MemPeakMB  int
}

// Kind is how the list groups an app: what it is doing, or that it has
// never been deployed — a repository waiting, which "stopped" misdescribes.
func (r *adminDeployRow) Kind() string {
	switch {
	case r.State.Actual == AppStateRunning:
		return "running"
	case r.State.Actual == AppStateFailed:
		return "failed"
	case r.State.IsBusy():
		return "running"
	case !r.State.HasRelease && r.State.UpdatedAt == 0:
		return "undeployed"
	default:
		return "stopped"
	}
}

// MemPct is the bar under the memory figure, capped at the limit.
func (r *adminDeployRow) MemPct() int {
	if r.MemLimitMB <= 0 || r.MemNowMB <= 0 {
		return 0
	}
	return min(r.MemNowMB*100/r.MemLimitMB, 100)
}

// Odd is a repository name that follows no naming rule — no letter or
// digit in it — which is what a test left behind looks like.
func (r *adminDeployRow) Odd() bool {
	return !strings.ContainsFunc(r.State.Repo, func(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) })
}

const adminDeploysPageSize = 10

// filterDeployRows keeps the rows a state tab and an owner select ask for.
func filterDeployRows(rows []*adminDeployRow, kind, owner string) []*adminDeployRow {
	out := rows[:0:0]
	for _, r := range rows {
		if kind != "" && kind != "all" && r.Kind() != kind {
			continue
		}
		if owner != "" && !strings.EqualFold(r.State.Owner, owner) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// sortDeployRows orders by what the select says: the most recently
// deployed first, by name, or by memory in use.
func sortDeployRows(rows []*adminDeployRow, by string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch by {
		case "name":
			if a.State.Owner != b.State.Owner {
				return a.State.Owner < b.State.Owner
			}
			return a.State.Repo < b.State.Repo
		case "memory":
			return a.MemNowMB > b.MemNowMB
		default:
			return a.State.UpdatedAt > b.State.UpdatedAt
		}
	})
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
		st.Health = CurrentHealth(st)
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
			row.Data, row.HasData = AppDataUsageForRepoID(st.Owner, st.Repo, repo.ID)
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
	var running, stopped, failed, undeployed int
	owners := map[string]bool{}
	for _, r := range rows {
		if r.NeedsAttention {
			attention = append(attention, r)
		}
		owners[r.State.Owner] = true
		switch r.Kind() {
		case "running":
			running++
		case "failed":
			failed++
		case "undeployed":
			undeployed++
		default:
			stopped++
		}
	}
	ownerNames := make([]string, 0, len(owners))
	for o := range owners {
		ownerNames = append(ownerNames, o)
	}
	sort.Strings(ownerNames)

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
	cgroupOK, cgroupDetail := CgroupStatus()
	ctx.Data["CgroupOK"] = cgroupOK
	ctx.Data["CgroupDetail"] = cgroupDetail
	ctx.Data["ConfigError"] = configErr

	ctx.Data["DataEnabled"] = AppDataEnabled()
	ctx.Data["Title"] = ctx.Locale.TrString("company.admin.nav_deploys")
	ctx.Data["Attention"] = attention
	ctx.Data["CountTotal"] = len(rows)
	ctx.Data["Fleet"] = summarizeFleet(rows, since) // fills every row's memory figures first
	ctx.Data["CountRunning"] = running
	ctx.Data["CountUndeployed"] = undeployed
	ctx.Data["Owners"] = ownerNames

	// The list itself: a state tab, an owner, an order, a page of ten.
	kind, owner, sortBy := ctx.FormString("state"), ctx.FormString("owner"), ctx.FormString("sort")
	shown := filterDeployRows(rows, kind, owner)
	sortDeployRows(shown, sortBy)
	page := max(ctx.FormInt("page"), 1)
	pages := max((len(shown)+adminDeploysPageSize-1)/adminDeploysPageSize, 1)
	page = min(page, pages)
	from := (page - 1) * adminDeploysPageSize
	to := min(from+adminDeploysPageSize, len(shown))
	ctx.Data["Rows"] = shown[from:to]
	ctx.Data["ListState"] = kind
	ctx.Data["ListOwner"] = owner
	ctx.Data["ListSort"] = sortBy
	ctx.Data["ListPage"] = page
	ctx.Data["ListPages"] = pages
	ctx.Data["ListFrom"] = from + 1
	ctx.Data["ListTo"] = to
	ctx.Data["ListTotal"] = len(shown)
	ctx.Data["ListQuery"] = "state=" + url.QueryEscape(kind) + "&owner=" + url.QueryEscape(owner) + "&sort=" + url.QueryEscape(sortBy)
	ctx.Data["CountStopped"] = stopped
	ctx.Data["CountFailed"] = failed
	ctx.HTML(http.StatusOK, tplAdminDeploys)
}

// staleDeployAfter is how long a deploy may sit in one busy state before it
// counts as stuck. A build with a large dependency set takes minutes; nothing
// takes this long without something having gone wrong.
const staleDeployAfter = 30 * time.Minute

// needsAttention reports whether an app is in a state a human has to do
// something about. Deliberately narrow: the "조치 필요" section is only
// useful if it is empty most of the time — if it always has rows in it,
// nobody reads it when it matters.
func needsAttention(st *AppState) bool {
	switch st.Actual {
	case AppStateFailed, AppStateSuspended:
		return true
	}
	// A deploy that has been "on its way" for this long has stopped: the
	// record of its failure could not be written — a full disk, usually —
	// and every button is refused while it stands.
	if st.IsBusy() && time.Since(time.Unix(st.UpdatedAt, 0)) > staleDeployAfter {
		return true
	}
	// Running is a process, not an answer: one that stopped answering is on
	// its way to an automatic restart, and someone should know it happened.
	if st.Actual == AppStateRunning && st.Health.State == "down" {
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

	// CommittedMB is every running app's memory limit added up, against the
	// host: the number that says whether the limits can all be honoured at
	// once. HostKnown is whether this host reports its size at all.
	CommittedMB int
	// RoomApps is how many more apps, at the default memory limit, fit
	// before the committed total reaches the warning share of the host.
	RoomApps      int
	RoomMB        int
	DefaultMemMB  int
	HostTotalMB   int
	HostAvailMB   int
	HostKnown     bool
	HostAvailable bool
	CommitPct     int
}

// CommitWarn and CommitOver are the two colours of the committed-memory card.
func (f fleetSummary) CommitWarn() bool { return f.HostKnown && f.CommitPct >= memoryCommitWarnPct }
func (f fleetSummary) CommitOver() bool { return f.HostKnown && f.CommitPct >= 100 }

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
			row.MemPeakMB = summary.MemMaxMB
			row.MemNowMB = CurrentMemoryMB(row.State.Owner, row.State.Repo)
		}
		row.MemLimitMB = SettingsFor(row.State.Owner, row.State.Repo).Limits.MemoryMB
		if row.State.Actual == AppStateRunning {
			out.CommittedMB += row.MemLimitMB
		}
	}
	if host, ok := hostMemory(); ok {
		out.HostKnown = true
		out.HostTotalMB = int(host.TotalBytes >> 20)
		out.HostAvailable = host.HasAvailable
		out.HostAvailMB = int(host.AvailableBytes >> 20)
		if out.HostTotalMB > 0 {
			out.CommitPct = out.CommittedMB * 100 / out.HostTotalMB
			out.DefaultMemMB = builtinDefaults().Limits.MemoryMB
			out.RoomMB = max(out.HostTotalMB*memoryCommitWarnPct/100-out.CommittedMB, 0)
			out.RoomApps = out.RoomMB / out.DefaultMemMB
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
