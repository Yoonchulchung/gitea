// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"

	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
)

// AdminBulkControl stops or starts the apps ticked on the list. Mounted at
// POST /-/admin/company-deploys/bulk. Each app is its own attempt: one that
// refuses does not keep the rest from acting, and the flash names it.
func AdminBulkControl(ctx *context.Context) {
	back := setting.AppSubURL + "/-/admin/company-deploys"
	_ = ctx.Req.ParseForm()
	verb := ctx.FormString("verb")
	apps := ctx.Req.Form["app"]
	if len(apps) == 0 {
		ctx.Flash.Info(ctx.Locale.TrString("company.admin.bulk_none"))
		ctx.Redirect(back)
		return
	}
	done := 0
	var failed []string
	for _, app := range apps {
		owner, repo, ok := strings.Cut(app, "/")
		if !ok {
			continue
		}
		var err error
		switch verb {
		case "stop":
			err = StopApp(owner, repo, ctx.Doer.Name, true)
		case "start":
			err = startAppAs(owner, repo, ctx.Doer.Name, true)
		default:
			ctx.Flash.Error("unknown action")
			ctx.Redirect(back)
			return
		}
		if err != nil {
			failed = append(failed, app+": "+AdminErrorL(ctx.Locale, err))
			continue
		}
		done++
	}
	if done > 0 {
		ctx.Flash.Success(ctx.Locale.TrString("company.admin.bulk_done", done))
	}
	if len(failed) > 0 {
		ctx.Flash.Error(strings.Join(failed, " / "))
	}
	ctx.Redirect(back)
}
