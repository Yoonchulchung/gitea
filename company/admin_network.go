// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	// Access is the exposure actually in force — the app's setting after the
	// instance ceiling has had its say — and Written is what the file says,
	// shown beside it when the two differ so an admin sees the clamp working.
	Access, Written string
	// Blocked is how many of the recent requests the guard refused, and
	// Recent how many it has on record at all. The numbers a review scans for.
	Blocked, Recent int
	Running         bool
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
		written := settings.Access
		if written == "" {
			written = AccessPublic
		}
		recent, blocked := RecentAccess(ref.Owner, ref.Repo, 0)
		rows = append(rows, networkRow{
			Owner: ref.Owner, Repo: ref.Repo,
			Mode: mode, Allow: settings.Network.Allow, Enforced: enforced,
			Access: clampAccess(written), Written: written,
			Recent: len(recent), Blocked: blocked,
			Running: IsAppRunning(ref.Owner, ref.Repo),
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
	ctx.Data["NetworkLink"] = setting.AppSubURL + "/-/admin/company-network"
	ctx.Data["Inbound"] = CurrentInboundPolicy()
	ctx.Data["Blocked"] = Blocklist()
	ctx.Data["Banned"] = ListBannedVisitors()
	ctx.HTML(http.StatusOK, tplAdminNetwork)
}

// AdminNetworkApp is the row-level view for one app: who came in, request by
// request, and what went out through the broker. This is the screen for the
// question after an incident — "who was that at 02:14" — which no count can
// answer.
func AdminNetworkApp(ctx *context.Context) {
	owner, repo := ctx.PathParam("owner"), ctx.PathParam("repo")
	ref, known := LookupApp(owner, repo)
	if !known {
		ctx.NotFound(nil)
		return
	}
	recent, blocked := RecentAccess(ref.Owner, ref.Repo, accessRingSize)
	ctx.Data["Title"] = ref.Owner + "/" + ref.Repo
	ctx.Data["Owner"], ctx.Data["Repo"] = ref.Owner, ref.Repo
	ctx.Data["Access"] = recent
	ctx.Data["AccessBlocked"] = blocked
	ctx.Data["Outbound"] = tailBrokerLog(ref, 200)
	ctx.Data["AppLink"] = setting.AppSubURL + "/-/admin/company-deploys/" + ref.Owner + "/" + ref.Repo
	ctx.Data["NetworkLink"] = setting.AppSubURL + "/-/admin/company-network"
	ctx.HTML(http.StatusOK, tplAdminNetworkApp)
}

// AdminNetworkUnban lifts one ban by hand.
// AdminNetworkBlock adds a destination to the list every app is refused;
// AdminNetworkUnblock removes one. Both are commits to apps.yml.
func AdminNetworkBlock(ctx *context.Context) {
	back := setting.AppSubURL + "/-/admin/company-network"
	entry, err := BlockDestination(ctx, ctx.Doer, ctx.FormString("entry"))
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	} else {
		ctx.Flash.Success(ctx.Locale.TrString("company.adminnetwork.blocked_added", entry))
	}
	ctx.Redirect(back)
}

func AdminNetworkUnblock(ctx *context.Context) {
	back := setting.AppSubURL + "/-/admin/company-network"
	entry := ctx.FormString("entry")
	if err := UnblockDestination(ctx, ctx.Doer, entry); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	} else {
		ctx.Flash.Success(ctx.Locale.TrString("company.adminnetwork.blocked_removed", entry))
	}
	ctx.Redirect(back)
}

func AdminNetworkUnban(ctx *context.Context) {
	key := ctx.FormString("key")
	if UnbanVisitor(key, ctx.Doer.Name) {
		ctx.Flash.Success(ctx.Locale.TrString("company.adminnetwork.unbanned", key))
	} else {
		ctx.Flash.Info(ctx.Locale.TrString("company.adminnetwork.not_banned", key))
	}
	ctx.Redirect(setting.AppSubURL + "/-/admin/company-network")
}

// tailBrokerLog returns the last n lines the broker wrote for an app — the
// outbound half of the audit, in the format broker.go writes it. Read raw
// rather than parsed: it is an operator's log, and a parser that dropped a
// line it did not understand would be dropping evidence.
func tailBrokerLog(ref AppRef, n int) []string {
	body, err := os.ReadFile(filepath.Join(appPathsFor(ref.Owner, ref.Repo).logs, brokerLogName))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	// Newest first, like the inbound table beside it.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines
}

const tplAdminNetworkApp templates.TplName = "company/admin_network_app"
