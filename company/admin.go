// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"

	activities_model "gitea.dev/models/activities"
	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"

	"xorm.io/builder"
)

const tplAdminActivity templates.TplName = "company/admin_activity"

// orgOwnedRepoIDs is the same shape as repo_model.AccessibleRepoIDsQuery,
// but scoped to repos owned by any organization rather than by
// permission — see docs/company/admin-activity.md for why no existing
// Gitea query does this.
func orgOwnedRepoIDs() *builder.Builder {
	return builder.Select("id").From("repository").Where(
		builder.In("owner_id", builder.Select("id").From("user").Where(
			builder.Eq{"type": user_model.UserTypeOrganization},
		)),
	)
}

// AdminActivity lists recent activity across every department's repos in
// one feed — the aggregation Gitea's own dashboard doesn't do (it's always
// scoped to a single org, see docs/company/admin-activity.md). Mounted
// inside the existing "/-/admin" group, so it inherits that group's
// adminReq middleware; no separate access check is needed here.
func AdminActivity(ctx *context.Context) {
	// Paged rather than a fixed tail. This feed covers every department at
	// once, so on a busy instance the interesting entry is as likely to be
	// two hundred rows down as at the top, and a page that only ever shows
	// the newest hundred cannot reach it at all.
	page := max(ctx.FormInt("page"), 1)
	const perPage = 50

	total, err := db.GetEngine(ctx).In("repo_id", orgOwnedRepoIDs()).Count(&activities_model.Action{})
	if err != nil {
		ctx.ServerError("count cross-department activity", err)
		return
	}

	var actions []*activities_model.Action
	if err := db.GetEngine(ctx).
		In("repo_id", orgOwnedRepoIDs()).
		Desc("created_unix").
		Limit(perPage, (page-1)*perPage).
		Find(&actions); err != nil {
		ctx.ServerError("list cross-department activity", err)
		return
	}

	repoIDs := make([]int64, 0, len(actions))
	for _, a := range actions {
		repoIDs = append(repoIDs, a.RepoID)
	}
	repos, err := repo_model.GetRepositoriesMapByIDs(ctx, repoIDs)
	if err != nil {
		ctx.ServerError("GetRepositoriesMapByIDs", err)
		return
	}
	for _, a := range actions {
		a.Repo = repos[a.RepoID]
	}

	ctx.Data["Title"] = "Cross-department activity"
	ctx.Data["Actions"] = actions
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(perPage).CurPage(page).Build()
	ctx.HTML(http.StatusOK, tplAdminActivity)
}
