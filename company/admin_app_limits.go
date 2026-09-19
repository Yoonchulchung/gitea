// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strconv"
	"strings"

	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
)

// AdminSetLimits changes what one app may consume.
//
// Only the data quota so far, because that is the limit a department actually
// runs into: memory is sized from the recorded peak and shown next to it on
// the same page, while storage grows quietly until a deploy is refused.
//
// Raising it is deliberately per-app. The instance default is what every app
// gets, and moving that to suit one department would move it for thirty.
// maxCPUPercent is the most a card accepts: thirty-two cores' worth.
const maxCPUPercent = 3200

// parseLimitMB reads a limit typed into a card: empty is the instance
// default (0), anything else a whole number of MB up to maxMB. Parsed
// rather than taken from FormInt, which reads a typo as 0 — and 0 here
// means "back to the default", which a typo must not do silently.
func parseLimitMB(raw string, maxMB int) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true
	}
	mb, err := strconv.Atoi(raw)
	if err != nil || mb < 0 || mb > maxMB {
		return 0, false
	}
	return mb, true
}

// AdminSetLimits saves a limit typed into one of the app page's cards —
// memory_mb or data_mb, whichever the card sent. Mounted at POST
// /-/admin/company-deploys/{owner}/{repo}/limits.
func AdminSetLimits(ctx *context.Context) {
	st, _, ok := adminAppContext(ctx)
	if !ok {
		return
	}
	back := setting.AppSubURL + "/-/admin/company-deploys/" + st.Owner + "/" + st.Repo

	// ParseForm first: the map is nil until something asks for a field
	// (the same trap AdminApprovePackages notes), and an unparsed form
	// made every card's Enter read as the data quota.
	_ = ctx.Req.ParseForm()
	if _, sent := ctx.Req.Form["memory_mb"]; sent {
		mb, ok := parseLimitMB(ctx.FormString("memory_mb"), maxMemoryRequestMB)
		if !ok {
			ctx.Flash.Error(ctx.Locale.TrString("company.err.bad_memory_limit", maxMemoryRequestMB))
			ctx.Redirect(back)
			return
		}
		subject := "set the memory limit to the default"
		if mb > 0 {
			subject = "set the memory limit"
		}
		if err := SetAppLimits(ctx, ctx.Doer, st.Owner, st.Repo, subject, func(s *AppSettings) { s.Limits.MemoryMB = mb }); err != nil {
			ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		} else {
			// The limit is set on the process as it starts (company/appsandbox.go),
			// so a running app keeps its old one until it is restarted.
			ctx.Flash.Success(ctx.Locale.TrString("company.flash.memory_changed"))
		}
		ctx.Redirect(back)
		return
	}

	if _, sent := ctx.Req.Form["cpu_percent"]; sent {
		pct, ok := parseLimitMB(ctx.FormString("cpu_percent"), maxCPUPercent) // a whole number too, in percent
		if !ok {
			ctx.Flash.Error(ctx.Locale.TrString("company.err.bad_cpu_limit", maxCPUPercent))
			ctx.Redirect(back)
			return
		}
		subject := "set the CPU limit to the default"
		if pct > 0 {
			subject = "set the CPU limit"
		}
		if err := SetAppLimits(ctx, ctx.Doer, st.Owner, st.Repo, subject, func(s *AppSettings) { s.Limits.CPUPercent = pct }); err != nil {
			ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		} else {
			ctx.Flash.Success(ctx.Locale.TrString("company.flash.cpu_changed"))
		}
		ctx.Redirect(back)
		return
	}

	mb, ok := parseLimitMB(ctx.FormString("data_mb"), maxDataQuotaMB)
	if !ok {
		ctx.Flash.Error(ctx.Locale.TrString("company.err.bad_quota", maxDataQuotaMB))
		ctx.Redirect(back)
		return
	}
	subject := "set the data quota to unlimited"
	if mb > 0 {
		subject = "set the data quota"
	}
	if err := SetAppLimits(ctx, ctx.Doer, st.Owner, st.Repo, subject, func(s *AppSettings) {
		// Zero is not "no storage": it means this app has no override and
		// follows [company] APP_DATA_QUOTA_MB, which is how clearing the
		// field puts an app back on the instance default.
		s.Limits.DataMB = mb
	}); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		ctx.Redirect(back)
		return
	}

	note := ""
	// Lowering below what is already stored is allowed — a full quota stops
	// new deploys and never stops the app — but it should not be a surprise.
	if usage, ok := AppDataUsageFor(ctx, st.Owner, st.Repo); ok && mb > 0 && usage.Bytes > int64(mb)<<20 {
		note = " " + ctx.Locale.TrString("company.flash.quota_below_usage")
	}
	ctx.Flash.Success(ctx.Locale.TrString("company.flash.quota_changed") + note)
	ctx.Redirect(back)
}
