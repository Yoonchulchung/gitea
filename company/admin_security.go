// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"time"

	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplAdminSecurity templates.TplName = "company/admin_security"

// AdminSecurity answers "were we attacked": what was tried against the
// platform in a period, from whom, and against what (company/secevents.go).
func AdminSecurity(ctx *context.Context) {
	ranges := map[string]struct {
		span, bucket time.Duration
		label        string
	}{
		"24h": {24 * time.Hour, time.Hour, "15시"},
		"7d":  {7 * 24 * time.Hour, 24 * time.Hour, "01-02"},
		"30d": {30 * 24 * time.Hour, 24 * time.Hour, "01-02"},
	}
	name := ctx.FormString("range")
	r, ok := ranges[name]
	if !ok {
		name, r = "24h", ranges["24h"]
	}
	now := time.Now()
	since := now.Add(-r.span).Truncate(r.bucket)
	summary := summarizeSecurity(ReadSecurityEvents(since), since, now, r.bucket, r.label)

	ctx.Data["Title"] = ctx.Locale.TrString("company.security.title")
	ctx.Data["Range"] = name
	ctx.Data["Summary"] = summary
	ctx.Data["Kinds"] = SecurityKinds
	ctx.Data["AlertReady"] = MailEventReady(ctx, MailEventAttack)
	ctx.Data["AlertWindow"] = int(securityAlertWindow.Minutes())
	ctx.Data["AlertTotal"] = securityAlertTotal
	ctx.Data["AlertLogins"] = securityAlertLogins
	ctx.Data["MailLink"] = setting.AppSubURL + "/-/admin/company-mail"
	ctx.HTML(http.StatusOK, tplAdminSecurity)
}
