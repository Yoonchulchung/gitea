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
	"gitea.dev/modules/timeutil"
	"gitea.dev/services/context"

	"xorm.io/builder"
)

const tplAdminActivity templates.TplName = "company/admin_activity"

// departmentOrgIDs is every department — organizations, in Gitea's model.
func departmentOrgIDs() *builder.Builder {
	return builder.Select("id").From("user").Where(
		builder.Eq{"type": user_model.UserTypeOrganization},
	)
}

// orgOwnedRepoIDs is the same shape as repo_model.AccessibleRepoIDsQuery,
// but scoped to repos owned by any organization rather than by
// permission — see docs/company/admin-activity.md for why no existing
// Gitea query does this.
func orgOwnedRepoIDs() *builder.Builder {
	return builder.Select("id").From("repository").Where(
		builder.In("owner_id", departmentOrgIDs()),
	)
}

// oneRowPerAction keeps a single row per thing that actually happened.
//
// `action` is a feed table, not an event log: services/feed/feed.go writes
// one row *per reader* — the actor, the repository's organization, and every
// watcher — all sharing act_user_id, op_type, repo_id and created_unix. A
// query filtered only by repo_id therefore shows one repository creation
// three times, which is what this feed did.
//
// The organization's copy is the one kept. It is written exactly once per
// action on an org-owned repo, and unlike the actor's copy it survives the
// actor being deleted (services/user/delete.go removes a departing user's
// whole feed, which would take their history out of an audit view with it).
func oneRowPerAction() builder.Cond {
	return builder.In("user_id", departmentOrgIDs())
}

// activityRow is one line of the feed, already resolved.
//
// Assembled here rather than in the template because the alternative is the
// forty-line if/else chain Gitea's own feed uses: that one is written for a
// feed, where each sentence repeats the repository it happened in. In a table
// that already has a repository column, the sentence would say it twice.
type activityRow struct {
	At    timeutil.TimeStamp
	Actor *user_model.User
	Repo  *repo_model.Repository
	// OpKey names what happened, translated. Always one of activityOpKeys
	// below — never a key built from a value that came out of the database,
	// because Tr hands an unknown key back verbatim as HTML and a dynamic key
	// is an escape hatch in disguise (see xss_guard_test.go).
	OpKey string
	// Detail is the branch, tag or issue number the action was about, where
	// it had one. It is what tells two commits on different branches apart.
	Detail string
}

// activityOpKeys is the vocabulary. An action type upstream adds later, or a
// row whose op_type is a number nothing maps to, is simply not in here — and
// then the key is the one constant below rather than a string assembled from
// whatever the column held.
var activityOpKeys = func() map[string]string {
	ops := []string{
		"create_repo", "rename_repo", "transfer_repo", "commit_repo", "create_branch",
		"delete_branch", "push_tag", "delete_tag", "publish_release",
		"create_issue", "comment_issue", "close_issue", "reopen_issue",
		"create_pull_request", "comment_pull", "merge_pull_request", "auto_merge_pull_request",
		"close_pull_request", "reopen_pull_request", "approve_pull_request",
		"reject_pull_request", "pull_review_dismissed", "pull_request_ready_for_review",
		"star_repo", "watch_repo", "mirror_sync_push", "mirror_sync_create", "mirror_sync_delete",
	}
	keys := make(map[string]string, len(ops))
	for _, op := range ops {
		keys[op] = "company.activity.op." + op
	}
	return keys
}()

const activityOpUnknownKey = "company.activity.op.unknown"

func describeActivity(a *activities_model.Action, repo *repo_model.Repository, actor *user_model.User) activityRow {
	op := a.OpType.String()
	row := activityRow{At: a.CreatedUnix, Actor: actor, Repo: repo, OpKey: activityOpUnknownKey}
	if key, known := activityOpKeys[op]; known {
		row.OpKey = key
	} else {
		// Shown, but as text the template escapes rather than as a key the
		// translator would hand back as markup.
		row.Detail = op
		return row
	}

	switch {
	case a.OpType.InActions("commit_repo", "create_branch", "delete_branch"):
		row.Detail = a.GetBranch()
	case a.OpType.InActions("push_tag", "delete_tag"):
		row.Detail = a.GetTag()
	case a.OpType.InActions("create_issue", "comment_issue", "close_issue", "reopen_issue",
		"create_pull_request", "comment_pull", "merge_pull_request", "close_pull_request",
		"reopen_pull_request", "approve_pull_request", "reject_pull_request",
		"auto_merge_pull_request", "pull_review_dismissed", "pull_request_ready_for_review"):
		if infos := a.GetIssueInfos(); len(infos) > 0 && infos[0] != "" {
			row.Detail = "#" + infos[0]
		}
	case a.OpType.InActions("rename_repo", "transfer_repo"):
		row.Detail = a.GetContent()
	}
	return row
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

	total, err := db.GetEngine(ctx).
		In("repo_id", orgOwnedRepoIDs()).
		Where(oneRowPerAction()).
		Count(&activities_model.Action{})
	if err != nil {
		ctx.ServerError("count cross-department activity", err)
		return
	}

	var actions []*activities_model.Action
	if err := db.GetEngine(ctx).
		In("repo_id", orgOwnedRepoIDs()).
		Where(oneRowPerAction()).
		Desc("created_unix").
		Limit(perPage, (page-1)*perPage).
		Find(&actions); err != nil {
		ctx.ServerError("list cross-department activity", err)
		return
	}

	repoIDs := make([]int64, 0, len(actions))
	userIDs := make([]int64, 0, len(actions))
	for _, a := range actions {
		repoIDs = append(repoIDs, a.RepoID)
		userIDs = append(userIDs, a.ActUserID)
	}
	repos, err := repo_model.GetRepositoriesMapByIDs(ctx, repoIDs)
	if err != nil {
		ctx.ServerError("GetRepositoriesMapByIDs", err)
		return
	}
	// The person, not just the repository. Without this the feed answered
	// "something happened here" and left "who" to be guessed — which is the
	// one thing an audit view exists to say.
	actors, err := user_model.GetUsersMapByIDs(ctx, userIDs)
	if err != nil {
		ctx.ServerError("GetUsersMapByIDs", err)
		return
	}

	rows := make([]activityRow, 0, len(actions))
	for _, a := range actions {
		rows = append(rows, describeActivity(a, repos[a.RepoID], actors[a.ActUserID]))
	}

	ctx.Data["Title"] = "Cross-department activity"
	ctx.Data["Rows"] = rows
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(perPage).CurPage(page).Build()
	ctx.HTML(http.StatusOK, tplAdminActivity)
}
