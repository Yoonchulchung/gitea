// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"net/http"
	"time"

	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplAdminAppData templates.TplName = "company/admin_app_data"

// The manual half of app data, and deliberately manual.
//
// Everything else in this platform decides for itself: a failed release rolls
// back, an app over its memory limit is stopped, expired data is swept. None
// of that works for the cases this page exists for — a schema change that
// needs unpicking, a restore that loses today's work either way. Those are
// judgement calls, so the platform's job here is to make the consequences
// visible and reversible, not to make the choice.

func adminDataLink(owner, repo string) string {
	return setting.AppSubURL + "/-/admin/company-deploys/" + owner + "/" + repo + "/data"
}

// AdminAppData is the console: what is stored, what copies exist, and the
// ways out.
func AdminAppData(ctx *context.Context) {
	st, repo, ok := adminAppContext(ctx)
	if !ok {
		return
	}
	dataDir, dirErr := appDataForStart(st.Owner, st.Repo)

	ctx.Data["Title"] = st.Owner + "/" + st.Repo
	ctx.Data["App"] = st
	ctx.Data["AppRepo"] = repo
	ctx.Data["DataEnabled"] = AppDataEnabled()
	ctx.Data["DataLink"] = adminDataLink(st.Owner, st.Repo)
	ctx.Data["AppLink"] = setting.AppSubURL + "/-/admin/company-deploys/" + st.Owner + "/" + st.Repo
	ctx.Data["SQLConsole"] = companySetting("APP_DATA_SQL_CONSOLE") == "true"
	ctx.Data["Running"] = IsAppRunning(st.Owner, st.Repo)
	available, detail := DataStatus()
	ctx.Data["DataAvailable"] = available
	ctx.Data["DataDetail"] = detail
	if dirErr != nil {
		ctx.Data["DataError"] = AdminErrorL(ctx.Locale, dirErr)
	} else if dataDir != "" {
		ctx.Data["Snapshots"] = ListSnapshots(dataDir)
	}
	if usage, ok := AppDataUsageFor(ctx, st.Owner, st.Repo); ok {
		ctx.Data["DataUsage"] = usage
	}
	ctx.Data["Archives"] = ListAppDataArchives()
	ctx.HTML(http.StatusOK, tplAdminAppData)
}

// AdminAppDataAction handles the buttons. One entry point, because every one
// of them ends the same way: a flash message and back to the console.
func AdminAppDataAction(ctx *context.Context) {
	st, _, ok := adminAppContext(ctx)
	if !ok {
		return
	}
	owner, repo, actor := st.Owner, st.Repo, ctx.Doer.Name

	var err error
	var note string
	switch ctx.PathParam("verb") {
	case "snapshot":
		var name string
		name, err = CreateSnapshot(ctx, owner, repo, "manual")
		note = ctx.Locale.TrString("company.data.flash_snapshot", name)
	case "restore":
		err = RestoreSnapshot(ctx, owner, repo, ctx.FormString("name"), actor)
		note = ctx.Locale.TrString("company.data.flash_restored", ctx.FormString("name"))
	case "sql":
		var result *AdminSQLResult
		result, err = RunAdminSQL(ctx, owner, repo, ctx.FormString("sql"), actor)
		if err == nil {
			note = ctx.Locale.TrString("company.data.flash_sql", result.Changed, result.Snapshot)
		}
	case "purge":
		err = adminPurgeArchive(ctx)
		note = ctx.Locale.TrString("company.data.flash_purged")
	default:
		ctx.HTTPError(http.StatusBadRequest, "unknown action")
		return
	}

	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	} else {
		ctx.Flash.Success(note)
	}
	ctx.Redirect(adminDataLink(owner, repo))
}

// adminPurgeArchive deletes soft-deleted data before its clock runs out.
// Only data already marked removed: this button must never be a way to
// delete a live app's database in one click.
func adminPurgeArchive(ctx *context.Context) error {
	repoID := ctx.FormInt64("repoID")
	if repoID <= 0 {
		return userKeyError("company.err.archive_unknown")
	}
	meta, err := loadAppDataMeta(appDataDirFor(repoID))
	if err != nil || meta.RepoID != repoID || meta.RemovedAt == 0 {
		return userKeyError("company.err.archive_unknown")
	}
	return PurgeAppData(repoID)
}

// AdminAppDataExport streams the bundle.
//
// Personal data can be in here, so it is streamed to the administrator who
// asked rather than written anywhere a later request could reach, and the
// response says not to store it.
func AdminAppDataExport(ctx *context.Context) {
	st, _, ok := adminAppContext(ctx)
	if !ok {
		return
	}
	filename := fmt.Sprintf("%s-%s-%s.zip", st.Owner, st.Repo, time.Now().UTC().Format("20060102-150405"))
	ctx.Resp.Header().Set("Content-Type", "application/zip")
	ctx.Resp.Header().Set("Cache-Control", "no-store")
	ctx.Resp.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	if err := WriteExportBundle(ctx, st.Owner, st.Repo, ctx.Resp); err != nil {
		// The header is already out, so this cannot become an error page. It
		// goes to the log, and the truncated zip fails to open — which is the
		// honest outcome, and better than a complete-looking partial export.
		log.Error("company: %s/%s: export failed: %v", st.Owner, st.Repo, err)
	}
}
