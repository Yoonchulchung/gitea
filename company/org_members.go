// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/url"
	"strings"

	org_model "gitea.dev/models/organization"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
	org_service "gitea.dev/services/org"
)

// OrgMemberAdd adds a person to an organization from its members page. Teams
// are not on screen here, so a member joins the owners team: everyone in a
// group runs it together. The checks are the ones Gitea's own team page makes.
func OrgMemberAdd(ctx *context.Context) {
	org, err := org_model.GetOrgByName(ctx, ctx.PathParam("org"))
	if err != nil {
		ctx.NotFound()
		return
	}
	isOwner, err := org.IsOwnedBy(ctx, ctx.Doer.ID)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	if !isOwner && !ctx.Doer.IsAdmin {
		ctx.NotFound()
		return
	}
	back := setting.AppSubURL + "/org/" + url.PathEscape(org.Name) + "/members"

	name := strings.TrimSpace(ctx.FormString("user"))
	u, err := user_model.GetUserByName(ctx, name)
	if err != nil {
		if !user_model.IsErrUserNotExist(err) {
			ctx.ServerErrorAuto(err)
			return
		}
		ctx.Flash.Error(ctx.Tr("form.user_not_exist"))
		ctx.Redirect(back)
		return
	}
	if u.IsOrganization() {
		ctx.Flash.Error(ctx.Tr("form.cannot_add_org_to_team"))
		ctx.Redirect(back)
		return
	}
	team, err := org.GetOwnerTeam(ctx)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	if team.IsMember(ctx, u.ID) {
		ctx.Flash.Error(ctx.Locale.TrString("company.org.already_member", u.Name))
		ctx.Redirect(back)
		return
	}
	if err := org_service.AddTeamMember(ctx, team, u); err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	ctx.Flash.Success(ctx.Locale.TrString("company.org.member_added", u.Name))
	ctx.Redirect(back)
}
