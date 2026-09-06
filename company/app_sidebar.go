// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"

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
type PermissionRow struct {
	Label  string
	Value  string
	State  PermissionState
	Reason string // why it was denied — the part that saves a support request
}

// AppSidebarData is everything the panel renders.
type AppSidebarData struct {
	Deployed    bool
	StatusLabel string
	Status      string // the raw state, for styling
	Cause       *AppCause
	Rows        []PermissionRow
	MemoryUsed  int // MB, most recent sample
	MemoryLimit int // MB
	AppURL      string
	AppLink     string
	DeployLink  string
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
	// StartedAt is when the running process came up. "Running" alone does not
	// say whether it has been up for a week or restarted a minute ago, which
	// is the first thing anyone wants to know when something looks wrong.
	StartedAt int64
}

// SetAppPermissionData attaches the sidebar panel's data to the repo home
// page.
//
// Registered as one more middleware on the existing repo-home chain, the
// same way this fork already injects RedirectToWorkspaceIfEmpty and
// SetDeployRequestAIReviewData (docs/company/patches.md).
//
// Everything it needs is already in memory — app state, parsed policy, the
// latest memory reading — so this adds neither a database query nor a file
// read to the most-visited page in the instance.
func SetAppPermissionData(ctx *context.Context) {
	if ctx.Repo == nil || ctx.Repo.Repository == nil || ctx.Repo.Owner == nil {
		return
	}
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name

	if _, known := LookupApp(owner, name); !known {
		return // never deployed: the panel would be noise on a plain repository
	}

	st := LoadAppState(owner, name)
	settings := SettingsFor(owner, name)
	data := &AppSidebarData{
		CanControl:  ctx.Repo.Permission.CanWrite(unit.TypeCode),
		Running:     st.Actual == AppStateRunning,
		Suspended:   st.Actual == AppStateSuspended,
		CanStart:    st.HasRelease && st.Actual != AppStateRunning && st.Actual != AppStateSuspended,
		CanRedeploy: st.SHA != "" && !st.IsBusy(),
		StartedAt:   st.StartedAt,
		Deployed:    true,
		StatusLabel: departmentStatusLabel(st),
		Status:      st.Actual,
		Cause:       DepartmentCause(st),
		Rows:        permissionRows(settings, st),
		MemoryLimit: settings.Limits.MemoryMB,
		MemoryUsed:  CurrentMemoryMB(owner, name),
		AppURL:      appProxyPrefix + "/" + owner + "/" + name,
		AppLink:     ctx.Repo.RepoLink + "/_app",
		DeployLink:  ctx.Repo.RepoLink + "/deploy",
	}
	ctx.Data["CompanyApp"] = data
}

func permissionRows(settings AppSettings, st *AppState) []PermissionRow {
	rows := []PermissionRow{{
		Label: "접근",
		Value: accessLabel(settings.Access),
		State: PermAllowed,
	}}

	switch settings.Network.Mode {
	case NetworkOpen:
		rows = append(rows, PermissionRow{Label: "외부 통신", Value: "제한 없음", State: PermAllowed})
	case NetworkBroker:
		for _, rule := range settings.Network.Allow {
			value := rule.Host
			if len(rule.Methods) > 0 {
				value += " (" + strings.Join(rule.Methods, ", ") + ")"
			}
			rows = append(rows, PermissionRow{Label: "외부 통신", Value: value, State: PermAllowed})
		}
	default:
		rows = append(rows, PermissionRow{
			Label: "외부 통신", Value: "차단됨", State: PermDenied,
			Reason: "이 앱은 외부 인터넷·다른 서버로 연결하지 않습니다. 필요하면 배포 요청에서 신청할 수 있습니다.",
		})
	}

	if packages := settings.AllowedPackages(); len(packages) > 0 {
		rows = append(rows, PermissionRow{Label: "패키지", Value: strings.Join(packages, ", "), State: PermAllowed})
	} else {
		rows = append(rows, PermissionRow{
			Label: "패키지", Value: "승인된 패키지 없음", State: PermDenied,
			Reason: "requirements.txt 에 적은 패키지는 관리자 승인 뒤에 설치됩니다.",
		})
	}

	// A package the app asked for and did not get is the single most useful
	// line here — it is the exact reason a deploy failed.
	if st.Reason == ReasonPackageDenied && st.Message != "" {
		rows = append(rows, PermissionRow{
			Label: "패키지", Value: st.Message, State: PermPending,
			Reason: "관리자 승인을 기다리고 있습니다.",
		})
	}

	if settings.Download.Policy == "allow" {
		rows = append(rows, PermissionRow{Label: "파일 다운로드", Value: "허용됨", State: PermAllowed})
	} else {
		rows = append(rows, PermissionRow{
			Label: "파일 다운로드", Value: "차단됨", State: PermDenied,
			Reason: "앱이 파일을 내려주는 것은 기본적으로 막혀 있습니다. 필요하면 배포 요청에서 신청할 수 있습니다.",
		})
	}
	return rows
}

func accessLabel(access string) string {
	switch access {
	case AccessLogin:
		return "로그인한 사람만"
	case AccessOrg:
		return "우리 부서만"
	default:
		return "사내 누구나"
	}
}
