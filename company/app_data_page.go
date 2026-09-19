// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"

	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplAppData templates.TplName = "company/app_data"

// AppData is a department's own view of its app's database: what is in it,
// and the means to change it without writing SQL (company/appdataedit.go).
// Reached only with write access to the repository, the same people who can
// change the code that writes this data.
func AppData(ctx *context.Context) {
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name

	// The read-only resolver: asking whether data exists must not take a
	// soft-deleted app off its retention clock. Same reason as the admin
	// console (company/admin_app_data.go).
	dataDir, hasData := AppDataDirIfPresent(ctx, owner, name)

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
		setDataEditData(ctx, owner, name, table, ListSnapshots(dataDir))
	}
	ctx.HTML(http.StatusOK, tplAppData)
}
