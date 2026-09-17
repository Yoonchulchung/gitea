// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"

	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplAppData templates.TplName = "company/app_data"

// AppData lets a department look at its own app's database.
//
// Reading only. Everything that changes data — a snapshot, a restore, a
// statement, an export — stays on the admin console, which is what was
// decided when this was planned: those are judgement calls with no way back
// except the one an administrator takes deliberately, and a restore in
// particular loses whatever was written since. See docs/company/app-data.md.
//
// Reading is not in that category. It is a department's own data, it is the
// question they ask first when an app behaves oddly, and the connection under
// it cannot write (company/appdatatool.go), so there is nothing here for the
// admin to be in the middle of.
func AppData(ctx *context.Context) {
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name

	// The read-only resolver: asking whether data exists must not take a
	// soft-deleted app off its retention clock. Same reason as the admin
	// console (company/admin_app_data.go).
	_, hasData := AppDataDirIfPresent(ctx, owner, name)

	ctx.Data["Title"] = ctx.Locale.TrString("company.data.title")
	ctx.Data["App"] = LoadAppState(owner, name)
	ctx.Data["AppLink"] = ctx.Repo.RepoLink + "/_app"
	ctx.Data["DataLink"] = ctx.Repo.RepoLink + "/_app/data"
	ctx.Data["DataEnabled"] = AppDataEnabled()
	ctx.Data["HasData"] = hasData
	if usage, ok := AppDataUsageFor(ctx, owner, name); ok {
		ctx.Data["DataUsage"] = usage
	}

	if hasData {
		table, statement := ctx.FormString("table"), ctx.FormString("sql")
		browse, err := BrowseAppData(ctx, owner, name, table, statement, ctx.FormInt("offset"))
		if browse != nil {
			ctx.Data["Browse"] = browse
		}
		if err != nil {
			// A department's own typo in a SELECT is theirs to see; anything
			// else becomes the generic sentence, because the raw text names
			// paths inside the sandbox.
			ctx.Data["BrowseError"] = DepartmentSafeErrorL(ctx.Locale, "browse app data", err)
		}
		ctx.Data["BrowseTable"] = table
		ctx.Data["BrowseSQL"] = statement
		ctx.Data["BrowsePageSize"] = browsePageSize
	}
	ctx.HTML(http.StatusOK, tplAppData)
}
