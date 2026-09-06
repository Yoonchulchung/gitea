// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"sort"

	organization "gitea.dev/models/organization"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
)

// The dashboard lists a department's repositories, and a repository is not an
// app: half of them never become one, and the ones that do say nothing there
// about whether they are actually up. So "is our thing running?" — the
// question the people using this platform ask most — could only be answered by
// opening repositories one at a time until you found the right one.
//
// This adds the short answer beside the repository list: every app the viewer
// can see, and what it is doing right now.

// DashboardApp is one row of that panel.
type DashboardApp struct {
	Owner, Repo string
	Status      string // the raw state, for styling
	StatusLabel string // locale key
	Link        string
	AppURL      string
	Running     bool
}

// SetDashboardApps attaches the viewer's apps to the dashboard.
//
// Scoped to what they can already see: an admin gets every app, and everyone
// else gets the apps of organizations they belong to. Membership is the same
// boundary the repository list uses, so this shows nothing new — it just
// answers a different question about it.
//
// Best-effort: the dashboard is the first page most people land on, and a
// panel that could not be built is not a reason to fail it.
func SetDashboardApps(ctx *context.Context) {
	if ctx.Doer == nil {
		return
	}
	states := ListAppStates()
	if len(states) == 0 {
		return
	}

	// One membership lookup per organization rather than per app: a
	// department with a dozen apps is one org.
	allowed := map[string]bool{}
	apps := make([]DashboardApp, 0, len(states))
	for _, st := range states {
		if !st.HasRelease && st.Actual == AppStateStopped {
			continue // never deployed: the repository list already covers it
		}
		visible, known := allowed[st.Owner]
		if !known {
			visible = ctx.Doer.IsAdmin
			if !visible {
				if org, err := organization.GetOrgByName(ctx, st.Owner); err == nil {
					visible, _ = organization.IsOrganizationMember(ctx, org.ID, ctx.Doer.ID)
				}
			}
			allowed[st.Owner] = visible
		}
		if !visible {
			continue
		}
		apps = append(apps, DashboardApp{
			Owner: st.Owner, Repo: st.Repo,
			Status:      st.Actual,
			StatusLabel: departmentStatusLabel(st),
			Link:        setting.AppSubURL + "/" + st.Owner + "/" + st.Repo + "/_app",
			AppURL:      setting.AppSubURL + appProxyPrefix + "/" + st.Owner + "/" + st.Repo,
			Running:     st.Actual == AppStateRunning,
		})
	}
	if len(apps) == 0 {
		return
	}
	// Anything not running first: this panel exists to surface the app that
	// stopped, and a healthy fleet should read as a quiet list rather than
	// hide the one row that matters at the bottom of it.
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Running != apps[j].Running {
			return !apps[i].Running
		}
		if apps[i].Owner != apps[j].Owner {
			return apps[i].Owner < apps[j].Owner
		}
		return apps[i].Repo < apps[j].Repo
	})
	ctx.Data["CompanyDashboardApps"] = apps
}
