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
func AdminSetLimits(ctx *context.Context) {
	st, _, ok := adminAppContext(ctx)
	if !ok {
		return
	}
	back := setting.AppSubURL + "/-/admin/company-deploys/" + st.Owner + "/" + st.Repo

	// Parsed rather than taken from FormInt, which reads anything unparsable
	// as 0 — and 0 here means "follow the instance default". A typo would have
	// silently reset the limit instead of being refused.
	raw := strings.TrimSpace(ctx.FormString("data_mb"))
	mb := 0
	if raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			ctx.Flash.Error(ctx.Locale.TrString("company.err.bad_quota", maxDataQuotaMB))
			ctx.Redirect(back)
			return
		}
		mb = parsed
	}
	if mb < 0 || mb > maxDataQuotaMB {
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
