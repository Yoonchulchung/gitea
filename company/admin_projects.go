// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	gitea_context "gitea.dev/services/context"
)

const tplAdminProjects templates.TplName = "company/admin_projects"

// AdminProjectRow is one project on the administrator's list.
type AdminProjectRow struct {
	Repo  *repo_model.Repository
	Owner *user_model.User
	// OwnerGone is an owner who left: deleted, deactivated or barred from
	// signing in. Their projects are the ones that need somebody named.
	OwnerGone bool
	Members   int
}

// AdminProjects lists every project with its owner, leads with the ones whose
// owner is gone, and shows what moving the old department repositories in
// would do.
func AdminProjects(ctx *gitea_context.Context) {
	org, err := ProjectOrg(ctx)
	if err != nil {
		ctx.ServerError("ProjectOrg", err)
		return
	}
	var repos []*repo_model.Repository
	if err := db.GetEngine(ctx).Where("owner_id = ?", org.ID).Asc("lower_name").Find(&repos); err != nil {
		ctx.ServerError("list projects", err)
		return
	}
	var owners []*ProjectOwner
	if err := db.GetEngine(ctx).Find(&owners); err != nil {
		ctx.ServerError("list project owners", err)
		return
	}
	ownerOf := make(map[int64]int64, len(owners))
	ownerIDs := make([]int64, 0, len(owners))
	for _, o := range owners {
		ownerOf[o.RepoID] = o.OwnerID
		ownerIDs = append(ownerIDs, o.OwnerID)
	}
	users, err := user_model.GetUsersMapByIDs(ctx, ownerIDs)
	if err != nil {
		ctx.ServerError("GetUsersMapByIDs", err)
		return
	}

	needsOwnerOnly := ctx.FormBool("needs_owner")
	rows := make([]*AdminProjectRow, 0, len(repos))
	needsOwner := 0
	for _, repo := range repos {
		if isCentralRepo(repo) {
			continue
		}
		owner := users[ownerOf[repo.ID]]
		row := &AdminProjectRow{
			Repo:      repo,
			Owner:     owner,
			OwnerGone: owner == nil || !owner.IsActive || owner.ProhibitLogin,
		}
		if row.OwnerGone {
			needsOwner++
		} else if needsOwnerOnly {
			continue
		}
		if n, err := db.GetEngine(ctx).Where("repo_id = ?", repo.ID).Count(new(repo_model.Collaboration)); err == nil {
			row.Members = int(n)
		}
		rows = append(rows, row)
	}

	plan, err := PlanProjectMigration(ctx)
	if err != nil {
		ctx.ServerError("PlanProjectMigration", err)
		return
	}
	movable := 0
	for _, item := range plan {
		if item.Blocked == "" {
			movable++
		}
	}

	ctx.Data["Title"] = ctx.Locale.TrString("company.projects.title")
	ctx.Data["PageIsAdminCompanyProjects"] = true
	ctx.Data["ProjectOrg"] = org
	ctx.Data["Projects"] = rows
	ctx.Data["NeedsOwner"] = needsOwner
	ctx.Data["NeedsOwnerOnly"] = needsOwnerOnly
	ctx.Data["Migration"] = plan
	ctx.Data["MigrationMovable"] = movable
	ctx.HTML(http.StatusOK, tplAdminProjects)
}

const adminProjectsPath = "/-/admin/company-projects"

// AdminProjectOwnerPost names a project's owner — any time, whether or not
// the current one still works here.
func AdminProjectOwnerPost(ctx *gitea_context.Context) {
	back := setting.AppSubURL + adminProjectsPath
	repo, err := repo_model.GetRepositoryByID(ctx, ctx.FormInt64("repo_id"))
	if err != nil || !IsProjectRepo(ctx, repo) {
		ctx.NotFound(nil)
		return
	}
	name := strings.TrimSpace(ctx.FormString("owner"))
	owner, err := user_model.GetUserByName(ctx, name)
	if err != nil {
		ctx.Flash.Error(ctx.Locale.TrString("company.project.no_such_user", name))
		ctx.Redirect(back)
		return
	}
	if err := SetProjectOwner(ctx, repo, owner, ctx.FormBool("drop_previous")); err != nil {
		ctx.Flash.Error(ownerChangeError(ctx, name, err))
		ctx.Redirect(back)
		return
	}
	log.Info("company: %s made %s the owner of %s", ctx.Doer.Name, owner.Name, repo.FullName())
	ctx.Flash.Success(ctx.Locale.TrString("company.projects.owner_changed", repo.Name, owner.Name))
	ctx.Redirect(back)
}

// AdminProjectHandoverPost hands every project one person owns to another —
// what somebody leaving needs, in one step.
func AdminProjectHandoverPost(ctx *gitea_context.Context) {
	fromID := ctx.FormInt64("from_id")
	back := setting.AppSubURL + "/-/admin/users/" + strconv.FormatInt(fromID, 10) + "/edit"
	name := strings.TrimSpace(ctx.FormString("to"))
	to, err := user_model.GetUserByName(ctx, name)
	if err != nil {
		ctx.Flash.Error(ctx.Locale.TrString("company.project.no_such_user", name))
		ctx.Redirect(back)
		return
	}
	projects, err := projectsOwnedBy(ctx, fromID)
	if err != nil {
		ctx.ServerError("projectsOwnedBy", err)
		return
	}
	for i, repo := range projects {
		if err := SetProjectOwner(ctx, repo, to, ctx.FormBool("drop_previous")); err != nil {
			ctx.Flash.Error(ownerChangeError(ctx, name, err))
			if i > 0 {
				ctx.Flash.Info(ctx.Locale.TrString("company.projects.handover_partial", i, len(projects)))
			}
			ctx.Redirect(back)
			return
		}
	}
	log.Info("company: %s handed %d projects of user %d to %s", ctx.Doer.Name, len(projects), fromID, to.Name)
	ctx.Flash.Success(ctx.Locale.TrString("company.projects.handover_done", len(projects), to.Name))
	ctx.Redirect(back)
}

func ownerChangeError(ctx *gitea_context.Context, name string, err error) string {
	if errors.Is(err, errNotRealUser) {
		return ctx.Locale.TrString("company.project.no_such_user", name)
	}
	return AdminErrorL(ctx.Locale, err)
}

// AdminProjectMigratePost moves every department repository that can move.
func AdminProjectMigratePost(ctx *gitea_context.Context) {
	moved, skipped, err := RunProjectMigration(ctx, ctx.Doer)
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	}
	if moved > 0 || err == nil {
		ctx.Flash.Success(ctx.Locale.TrString("company.projects.migrate_done", moved, skipped))
	}
	ctx.Redirect(setting.AppSubURL + adminProjectsPath)
}
