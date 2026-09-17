// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"gitea.dev/models/db"
	org_model "gitea.dev/models/organization"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	gitea_context "gitea.dev/services/context"
	org_service "gitea.dev/services/org"
)

// Putting a person in a department, from the page an administrator is
// already on.
//
// Membership normally arrives with the group sync at login
// (docs/company/departments.md), which is the right mechanism and the wrong
// one for the first week of somebody's job: the directory has not caught up,
// nobody can add them, and the person sits on the no-department page. Gitea
// can of course do this — through the organization's own members screen,
// which means knowing which organization first, and that is exactly what the
// administrator looking at this user does not know yet.

// AdminUserPanel fills in the company half of the admin's user page.
//
// Called with one line from routers/web/admin/users.go (docs/company/patches.md);
// everything it needs is already in ctx.Data, so it adds no parameters to a
// core signature.
func AdminUserPanel(ctx *gitea_context.Context) {
	u, ok := ctx.Data["User"].(*user_model.User)
	if !ok || u == nil {
		return
	}

	departments, err := org_model.GetUserOrganizations(ctx, u.ID)
	if err != nil {
		log.Error("company: listing departments for %s: %v", u.Name, err)
		return
	}
	// Which of them this person owns, because "member" and "owner" are
	// different answers to "why can they do that?".
	owned := make(map[int64]bool, len(departments))
	for _, org := range departments {
		isOwner, err := org_model.IsOrganizationOwner(ctx, org.ID, u.ID)
		if err == nil && isOwner {
			owned[org.ID] = true
		}
	}

	var all []*repo_model.Repository
	if err := db.GetEngine(ctx).In("id", orgOwnedRepoIDs()).Find(&all); err != nil {
		log.Error("company: listing department repositories: %v", err)
	}

	ctx.Data["CompanyDepartments"] = departments
	ctx.Data["CompanyOwnedDepartments"] = owned
	ctx.Data["CompanyAllDepartments"] = allDepartments(ctx)
	ctx.Data["CompanyUserStats"] = userStorage(ctx, u, departments)
}

func allDepartments(ctx *gitea_context.Context) []*user_model.User {
	var orgs []*user_model.User
	if err := db.GetEngine(ctx).
		Where("type = ?", user_model.UserTypeOrganization).
		Asc("lower_name").Find(&orgs); err != nil {
		log.Error("company: listing departments: %v", err)
		return nil
	}
	return orgs
}

// UserStorage is the storage half of the page — what this person's work
// actually occupies, which is the question an administrator sizing a disk is
// asking and had no screen to ask it on.
type UserStorage struct {
	Repos      int
	RepoBytes  int64
	AppBytes   int64
	AppQuota   int64
	AppPercent int
}

func userStorage(ctx *gitea_context.Context, u *user_model.User, departments []*org_model.Organization) UserStorage {
	var stats UserStorage

	// Their own repositories. Summed in the database rather than by walking
	// rows: an administrator opening a user page should not pay for it.
	type sizeRow struct {
		Count int
		Bytes int64
	}
	var row sizeRow
	if _, err := db.GetEngine(ctx).Table("repository").
		Select("COUNT(*) AS count, COALESCE(SUM(size), 0) AS bytes").
		Where("owner_id = ?", u.ID).Get(&row); err != nil {
		log.Error("company: sizing repositories for %s: %v", u.Name, err)
	}
	stats.Repos, stats.RepoBytes = row.Count, row.Bytes

	if !AppDataEnabled() {
		return stats
	}
	// App data for every app in the departments this person belongs to. It is
	// shared, not theirs alone, and the screen says so — but it is the number
	// that decides whether the volume is about to fill up.
	for _, org := range departments {
		var repos []*repo_model.Repository
		if err := db.GetEngine(ctx).Where("owner_id = ?", org.ID).Find(&repos); err != nil {
			continue
		}
		for _, repo := range repos {
			usage, ok := AppDataUsageForRepoID(org.Name, repo.Name, repo.ID)
			if !ok {
				continue
			}
			stats.AppBytes += usage.Bytes
			stats.AppQuota += usage.QuotaBytes
		}
	}
	if stats.AppQuota > 0 {
		stats.AppPercent = int(stats.AppBytes * 100 / stats.AppQuota)
	}
	return stats
}

// AdminAssignDepartment adds a user to a department, or removes them.
//
// Owner is the default because that is what a department needs to run its own
// repositories here, and a member who cannot is the commoner mistake. It is
// still a choice: an assistant who should see the work without being able to
// reshape it is a real case, and defaulting is not the same as deciding.
func AdminAssignDepartment(ctx *gitea_context.Context) {
	userID := ctx.FormInt64("user_id")
	orgID := ctx.FormInt64("org_id")
	back := setting.AppSubURL + "/-/admin/users/" + ctx.FormString("user_id") + "/edit"

	u, err := user_model.GetUserByID(ctx, userID)
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		ctx.Redirect(back)
		return
	}
	org, err := org_model.GetOrgByID(ctx, orgID)
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		ctx.Redirect(back)
		return
	}

	if ctx.FormString("action") == "remove" {
		err = org_service.RemoveOrgUser(ctx, org, u)
		if err == nil {
			ctx.Flash.Success(ctx.Locale.TrString("company.adminuser.removed", u.Name, org.Name))
		}
	} else {
		err = assignToDepartment(ctx, org, u, ctx.FormString("role") == "owner")
		if err == nil {
			ctx.Flash.Success(ctx.Locale.TrString("company.adminuser.assigned", u.Name, org.Name))
		}
	}
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	}
	ctx.Redirect(back)
}

// assignToDepartment puts the user in the owners team, or plain membership.
//
// Owner membership goes through the team rather than through org_user
// directly: the owners team *is* what ownership means in Gitea's model, and a
// row added beside it would make someone a member the permission checks never
// treat as an owner.
func assignToDepartment(ctx *gitea_context.Context, org *org_model.Organization, u *user_model.User, asOwner bool) error {
	if !asOwner {
		return org_model.AddOrgUser(ctx, org.ID, u.ID)
	}
	team, err := org_model.GetOwnerTeam(ctx, org.ID)
	if err != nil {
		return err
	}
	return org_service.AddTeamMember(ctx, team, u)
}
