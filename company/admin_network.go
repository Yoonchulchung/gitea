// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"sort"

	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplAdminNetwork templates.TplName = "company/admin_network"

// Every app's outbound access, on one page.
//
// It was already visible — one app at a time, on that app's own admin page.
// That is the wrong shape for the question this answers. Nobody wonders
// whether *this* app can reach the internet; they wonder which ones can, and
// finding that out by opening thirty pages means it does not get checked. An
// exception nobody reviews stops being an exception, which is the same reason
// the app list already flags open network access.

// networkRow is one app's outbound policy.
type networkRow struct {
	Owner, Repo string
	Mode        string
	Allow       []AppNetworkRule
	// Enforced is whether the sandbox is actually imposing Mode. On a host
	// with no sandbox the policy is still policy and the block is simply not
	// there, and those two states must not look alike on a review screen.
	Enforced bool
}

// Exception is outbound access this app has that the default does not give
// it — what someone scanning the page is looking for.
func (r networkRow) Exception() bool { return r.Mode != NetworkNone }

// AdminNetwork lists what every department app may reach.
func AdminNetwork(ctx *context.Context) {
	enforced, detail := NetworkEnforced()

	var rows []networkRow
	appRegistry.Range(func(_, v any) bool {
		ref, ok := v.(AppRef)
		if !ok {
			return true
		}
		settings := SettingsFor(ref.Owner, ref.Repo)
		mode := settings.Network.Mode
		if mode == "" {
			mode = NetworkNone
		}
		rows = append(rows, networkRow{
			Owner: ref.Owner, Repo: ref.Repo,
			Mode: mode, Allow: settings.Network.Allow, Enforced: enforced,
		})
		return true
	})

	// Exceptions first, then by name: the rows worth reading are the ones
	// that are not the default, and a page sorted alphabetically buries them
	// among apps that can reach nothing.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Exception() != rows[j].Exception() {
			return rows[i].Exception()
		}
		if rows[i].Owner != rows[j].Owner {
			return rows[i].Owner < rows[j].Owner
		}
		return rows[i].Repo < rows[j].Repo
	})

	exceptions := 0
	for _, row := range rows {
		if row.Exception() {
			exceptions++
		}
	}

	ctx.Data["Title"] = ctx.Locale.TrString("company.adminnetwork.title")
	ctx.Data["Rows"] = rows
	ctx.Data["Total"] = len(rows)
	ctx.Data["Exceptions"] = exceptions
	ctx.Data["Enforced"] = enforced
	ctx.Data["EnforcedDetail"] = detail
	ctx.Data["AppLinkPrefix"] = setting.AppSubURL + "/-/admin/company-deploys/"
	ctx.HTML(http.StatusOK, tplAdminNetwork)
}
