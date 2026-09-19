// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"

	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/unit"
	"gitea.dev/services/context"
)

// The repository home page is where a non-developer actually goes, so it is
// where the answer to "what is my app allowed to do?" has to be. Anything
// that requires navigating somewhere else will not be found.
//
// Design rules for this panel, from docs/company/app-platform.md:
//
//   - Denied and pending items matter most. Showing only what was approved
//     leaves "why doesn't my app work?" unanswered on the very screen the
//     person is looking at.
//   - A denial without its reason still sends them to an administrator.
//   - Resource use is shown next to the limit, because "512MB" alone gives
//     nobody grounds to decide whether to ask for more.
//   - When nothing is wrong, the panel stays quiet. A panel that always has
//     something to say is a panel nobody reads on the day it matters. The
//     cause box appears only on a real failure, and that plus the status
//     badge is the whole of the emphasis — a third marker on the same block
//     read as noise rather than urgency.

// PermissionState is how one permission is rendered.
type PermissionState string

const (
	PermAllowed PermissionState = "allowed"
	PermDenied  PermissionState = "denied"
	PermPending PermissionState = "pending"
)

// PermissionRow is one line of the sidebar panel.
//
// Label, Value and Reason are locale keys; ValueText is verbatim data — a
// host, a package list — that no locale file can know. Same split as
// AppCause, for the same reason: the fixed sentences follow the reader's
// language, and the data follows itself.
type PermissionRow struct {
	Label string
	// Value is a locale key; empty when ValueText carries the value instead.
	Value     string
	ValueText string
	State     PermissionState
	// Reason (a locale key) is why it was denied — the part that saves a
	// support request. ReasonText carries verbatim additions.
	Reason     string
	ReasonText string
	// More is how many items were left off ValueText for room — the sidebar
	// shows the first few packages and sends the reader to the app page.
	More int
}

// sidebarPackageLimit is how many packages the repository sidebar names.
const sidebarPackageLimit = 5

// trimPackagesRow shortens the packages row to its first few names. The
// list is the platform's whole stack plus every approval, and on a sidebar
// it pushed everything below it out of sight.
func trimPackagesRow(rows []PermissionRow, keep int) []PermissionRow {
	for i := range rows {
		if rows[i].Label != "company.perm.packages" || rows[i].ValueText == "" {
			continue
		}
		names := strings.Split(rows[i].ValueText, ", ")
		if len(names) > keep {
			rows[i].ValueText = strings.Join(names[:keep], ", ")
			rows[i].More = len(names) - keep
		}
	}
	return rows
}

// AppSidebarData is everything the panel renders.
type AppSidebarData struct {
	// ShowState is whether the header and panel show the app's own state.
	// Hidden only while the deploy badge beside the file list already says
	// "running": that badge reports the latest deploy request, and after a
	// rejected one it says "rejected" over a perfectly healthy app.
	ShowState   bool
	Deployed    bool
	StatusLabel string
	Status      string // the raw state, for styling
	Cause       *AppCause
	Rows        []PermissionRow
	MemoryUsed  int // MB, most recent sample
	MemoryLimit int
	// MemoryMeasured is false where this host cannot sample usage at all. The
	// figure above is then absent rather than zero, and "192MB 중 0MB 사용"
	// would read as an idle app rather than as no data
	// (company/appsample.go).
	MemoryMeasured bool
	AppURL         string
	AppLink        string
	DeployLink     string
	// CanControl is write access to the repository. Starting and stopping an
	// app is the department's own decision, so it follows the same permission
	// as changing the code.
	CanControl bool
	// Running drives which of start/stop is offered. Suspended is neither:
	// an admin stopped it and the department cannot undo that.
	Running   bool
	Suspended bool
	// CanStart is whether pressing start could actually work. An app with
	// nothing built yet is not offered the button at all — the cause panel
	// tells them to deploy instead, which is the thing that would help.
	CanStart bool
	// CanRedeploy is whether rebuilding the recorded commit is worth
	// offering: something has to have been deployed, and nothing may be in
	// flight already.
	CanRedeploy bool
}

// SetAppPermissionData attaches the sidebar panel's data to the repo home
// page.
//
// Registered as one more middleware on the existing repo-home chain, the
// same way this fork already injects RedirectToWorkspaceIfEmpty and
// SetDeployRequestPageData (docs/company/patches.md).
//
// Everything it needs is already in memory — app state, parsed policy, the
// latest memory reading — so this adds neither a database query nor a file
// read to the most-visited page in the instance.
func SetAppPermissionData(ctx *context.Context) {
	if ctx.Repo == nil || ctx.Repo.Repository == nil || ctx.Repo.Owner == nil || !ctx.IsSigned {
		// On a public repository an anonymous visitor would otherwise read the
		// app's failure cause, its memory use and the internal hosts it may
		// reach — none of which is theirs to act on.
		return
	}
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name

	if _, known := LookupApp(owner, name); !known {
		// Never deployed. Not nothing, though: the settings a department can
		// reach through this panel — access mode, environment variables, the
		// permission request — all work before a first deploy, and are most
		// useful then. Hiding the panel entirely meant the only way to find
		// the app screen was to already know its URL.
		//
		// Department repositories only: on a personal repository the platform
		// has no role, and the panel really would be noise.
		if ctx.Repo.Owner.IsOrganization() {
			ctx.Data["CompanyApp"] = &AppSidebarData{
				CanControl: ctx.Repo.Permission.CanWrite(unit.TypeCode),
				AppLink:    ctx.Repo.RepoLink + "/_app",
				DeployLink: ctx.Repo.RepoLink + "/deploy",
			}
		}
		return
	}

	st := LoadAppState(owner, name)
	settings := SettingsFor(owner, name)
	// Whether the usage figure below is a measurement at all: a host with no
	// /proc cannot read it, and "192MB 중 0MB 사용" reads as an idle app
	// rather than as no data (company/appsample.go).
	memoryMeasured, _ := ResourceSamplingAvailable()

	data := &AppSidebarData{
		CanControl:     ctx.Repo.Permission.CanWrite(unit.TypeCode),
		Running:        st.Actual == AppStateRunning,
		Suspended:      st.Actual == AppStateSuspended,
		CanStart:       st.HasRelease && st.Actual != AppStateRunning && st.Actual != AppStateSuspended,
		CanRedeploy:    st.SHA != "" && !st.IsBusy(),
		ShowState:      st.Actual != AppStateRunning || !deployBadgeSaysRunning(ctx, st),
		Deployed:       true,
		StatusLabel:    departmentStatusLabel(st),
		Status:         st.Actual,
		Cause:          DepartmentCause(st),
		Rows:           trimPackagesRow(permissionRows(settings, st), sidebarPackageLimit),
		MemoryLimit:    settings.Limits.MemoryMB,
		MemoryUsed:     CurrentMemoryMB(owner, name),
		MemoryMeasured: memoryMeasured,
		AppURL:         appURL(owner, name),
		AppLink:        ctx.Repo.RepoLink + "/_app",
		DeployLink:     ctx.Repo.RepoLink + "/deploy",
	}
	ctx.Data["CompanyApp"] = data
}

func permissionRows(settings AppSettings, st *AppState) []PermissionRow {
	rows := []PermissionRow{{
		Label: "company.perm.access",
		Value: accessLabel(settings.Access),
		State: PermAllowed,
	}}

	// These rows are department-facing, so they state permission and never
	// enforcement.
	//
	// The distinction matters twice over. Saying "blocked" would promise a
	// technical block this host may not be applying, and saying that it is
	// *not* being applied would hand every person with repository access a
	// working description of a hole only an administrator can close. So the
	// wording is about what the app is allowed to do, which is true either
	// way; whether the platform is currently able to impose it is on the
	// admin screen, where someone can act on it (company/admin_app.go).
	switch settings.Network.Mode {
	case NetworkOpen:
		rows = append(rows, PermissionRow{Label: "company.perm.outbound", Value: "company.perm.outbound_open", State: PermAllowed})
	case NetworkBroker:
		for _, rule := range settings.Network.Allow {
			value := rule.Host
			if len(rule.Methods) > 0 {
				value += " (" + strings.Join(rule.Methods, ", ") + ")"
			}
			rows = append(rows, PermissionRow{Label: "company.perm.outbound", ValueText: value, State: PermAllowed})
		}
	default:
		rows = append(rows, PermissionRow{
			Label: "company.perm.outbound", Value: "company.perm.outbound_none", State: PermDenied,
			Reason: "company.perm.outbound_none.reason",
		})
	}

	if packages := settings.AllowedPackages(); len(packages) > 0 {
		rows = append(rows, PermissionRow{Label: "company.perm.packages", ValueText: strings.Join(packages, ", "), State: PermAllowed})
	} else {
		rows = append(rows, PermissionRow{
			Label: "company.perm.packages", Value: "company.perm.packages_none", State: PermDenied,
			Reason: "company.perm.packages_none.reason",
		})
	}

	// A package the app needs and did not get is the single most useful line
	// here — it is the exact reason a deploy failed.
	//
	// Driven by MissingPackages rather than by st.Reason: the reason code is
	// cleared the moment another deploy is queued, so the one fact that
	// explains why nothing will install disappeared from this panel while
	// still being true. MissingPackages survives until a build succeeds.
	//
	// Names only. st.Message is the build's own prose, written for an admin.
	if len(st.MissingPackages) > 0 {
		rows = append(rows, PermissionRow{
			Label: "company.perm.packages", ValueText: strings.Join(st.MissingPackages, ", "), State: PermPending,
			Reason: "company.perm.packages_missing.reason",
		})
	}

	if settings.Download.Policy == "allow" {
		rows = append(rows, PermissionRow{Label: "company.perm.download", Value: "company.perm.allowed", State: PermAllowed})
	} else {
		rows = append(rows, PermissionRow{
			Label: "company.perm.download", Value: "company.perm.blocked", State: PermDenied,
			Reason: "company.perm.download_blocked.reason",
		})
	}
	return rows
}

func accessLabel(access string) string {
	switch access {
	case AccessLogin:
		return "company.perm.access_login"
	case AccessOrg:
		return "company.perm.access_org"
	default:
		return "company.perm.access_public"
	}
}

// PendingRequest is one item a department has submitted and an admin has not
// decided yet.
type PendingRequest struct {
	Kind    string
	Label   string
	Detail  string
	Reason  string
	Waiting bool // still open, as opposed to decided
}

// deployBadgeSaysRunning reports whether the deploy-request badge on the
// repository home will read "running" for this app (company/deploystatus.go).
func deployBadgeSaysRunning(ctx *context.Context, st *AppState) bool {
	pr, err := latestDeployRequest(ctx, st.Owner, st.Repo)
	if err != nil || pr == nil || !pr.HasMerged {
		return false
	}
	return st.PRID == pr.ID && st.LastOutcomeOf(pr.MergedCommitID) != AppStateFailed
}

// pendingPermissionRequests reports what this app has asked for and not yet
// received.
//
// "Why is my app still blocked?" has two different answers — nobody asked, or
// somebody asked and it is sitting with an admin — and a screen that cannot
// tell them apart sends the department to ask an administrator either way.
//
// Best-effort: the request file is a record, and a screen that cannot read it
// should show less rather than fail.
func pendingPermissionRequests(ctx *context.Context, owner, repo string) []PendingRequest {
	set := LoadPermissionRequestSet(owner, repo)
	if set == nil || len(set.Requests) == 0 {
		return nil
	}
	// Merged means the permissions were applied, so the items are history
	// rather than something to wait for.
	pr, err := issues_model.GetPullRequestByID(ctx, set.PRID)
	if err != nil || pr.HasMerged {
		return nil
	}

	out := make([]PendingRequest, 0, len(set.Requests))
	for _, r := range set.Requests {
		out = append(out, PendingRequest{Kind: r.Kind, Label: r.Label, Detail: r.Detail, Reason: r.Reason, Waiting: true})
	}
	return out
}
