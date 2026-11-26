// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"maps"
	"net/http"
	"slices"
	"sort"
	"time"

	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplAdminProxy templates.TplName = "company/admin_proxy"

type proxyHeader struct{ Name, Value string }

// proxyAppRow is how the proxy treats one app, where that differs by app.
type proxyAppRow struct {
	Owner, Repo string
	Access      string // in force, after the instance ceiling
	Written     string // as set for the app
	Download    bool
	Headers     []proxyHeader // the app's own additions to the security headers
	Running     bool
}

// AdminProxy shows what the app proxy applies, in the order a request meets
// it. The values are the ones in force, read from the same settings and
// constants the proxy uses.
func AdminProxy(ctx *context.Context) {
	now := time.Now()

	var apps []proxyAppRow
	accessCounts := map[string]int{}
	appRegistry.Range(func(_, v any) bool {
		ref, ok := v.(AppRef)
		if !ok {
			return true
		}
		settings := SettingsFor(ref.Owner, ref.Repo)
		row := proxyAppRow{
			Owner: ref.Owner, Repo: ref.Repo,
			Access: clampAccess(settings.Access), Written: settings.Access,
			Download: settings.Download.Policy == "allow",
			Running:  IsAppRunning(ref.Owner, ref.Repo),
		}
		if row.Written == "" {
			row.Written = AccessPublic
		}
		for _, name := range slices.Sorted(maps.Keys(settings.Security.Headers)) {
			row.Headers = append(row.Headers, proxyHeader{name, settings.Security.Headers[name]})
		}
		accessCounts[row.Access]++
		apps = append(apps, row)
		return true
	})
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Owner != apps[j].Owner {
			return apps[i].Owner < apps[j].Owner
		}
		return apps[i].Repo < apps[j].Repo
	})

	security := make([]proxyHeader, 0, len(defaultSecurityHeaders))
	for _, name := range slices.Sorted(maps.Keys(defaultSecurityHeaders)) {
		security = append(security, proxyHeader{name, defaultSecurityHeaders[name]})
	}

	ctx.Data["Title"] = ctx.Locale.TrString("company.adminproxy.title")
	ctx.Data["Mount"] = setting.AppSubURL + appProxyPrefix
	ctx.Data["MaxAccess"] = MaxAppAccess()
	ctx.Data["AccessCounts"] = accessCounts
	ctx.Data["GuardOn"] = proxyGuardEnabled()
	ctx.Data["RatePerMin"] = companySettingPositiveInt("APP_PROXY_RATE_PER_MIN", guardRatePerMinDefault)
	ctx.Data["Burst"] = companySettingPositiveInt("APP_PROXY_BURST", guardBurstDefault)
	ctx.Data["BanStrikes"] = companySettingPositiveInt("APP_PROXY_BAN_STRIKES", guardBanStrikesDefault)
	ctx.Data["BanMinutes"] = companySettingPositiveInt("APP_PROXY_BAN_MINUTES", guardBanMinutesDefault)
	ctx.Data["MaxBodyMB"] = companySettingPositiveInt("APP_PROXY_MAX_BODY_MB", guardMaxBodyMBDefault)
	ctx.Data["BotAgents"] = guardBotAgents
	ctx.Data["ProbePaths"] = guardProbePaths
	ctx.Data["Banned"] = ListBannedVisitors()
	ctx.Data["Now"] = now
	ctx.Data["StrippedRequestHeaders"] = forwardedHeadersToStrip
	ctx.Data["HeaderTimeout"] = int(proxyResponseHeaderTimeout.Seconds())
	ctx.Data["IdleTimeout"] = int(proxyIdleConnTimeout.Seconds())
	ctx.Data["SecurityHeaders"] = security
	ctx.Data["StrippedResponseHeaders"] = strippedResponseHeaders
	ctx.Data["DownloadTypes"] = downloadAllowedTypes
	ctx.Data["Apps"] = apps
	ctx.Data["NetworkLink"] = setting.AppSubURL + "/-/admin/company-network"
	ctx.HTML(http.StatusOK, tplAdminProxy)
}
