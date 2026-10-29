// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"net/http"
	"strings"

	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const (
	deployRequestsPath = companyPrefix + "/deploy-requests"
	newProjectPath     = companyPrefix + "/projects/new"

	tplProjectNew templates.TplName = "company/project_new"
)

// ProjectNew is the new-project form: a name and a description. Where it
// lives and who owns it are not questions anybody here should have to answer.
func ProjectNew(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Locale.TrString("company.project.new_title")
	ctx.HTML(http.StatusOK, tplProjectNew)
}

func ProjectNewPost(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Locale.TrString("company.project.new_title")
	name := strings.TrimSpace(ctx.FormString("repo_name"))
	description := strings.TrimSpace(ctx.FormString("description"))
	ctx.Data["repo_name"], ctx.Data["description"] = name, description

	if len(name) > 100 {
		ctx.Data["Err_RepoName"] = true
		ctx.RenderWithErrDeprecated(ctx.Tr("company.project.name_invalid"), tplProjectNew, nil)
		return
	}
	repo, err := CreateProject(ctx, ctx.Doer, name, description)
	if err == nil {
		ctx.Redirect(repo.Link())
		return
	}
	var reserved db.ErrNameReserved
	var pattern db.ErrNamePatternNotAllowed
	var chars db.ErrNameCharsNotAllowed
	switch {
	case repo_model.IsErrRepoAlreadyExist(err), repo_model.IsErrRepoFilesAlreadyExist(err):
		ctx.Data["Err_RepoName"] = true
		ctx.RenderWithErrDeprecated(ctx.Tr("company.project.name_taken"), tplProjectNew, nil)
	case errors.As(err, &reserved), errors.As(err, &pattern), errors.As(err, &chars):
		ctx.Data["Err_RepoName"] = true
		ctx.RenderWithErrDeprecated(ctx.Tr("company.project.name_invalid"), tplProjectNew, nil)
	default:
		log.Error("company: %s creating project %q: %v", ctx.Doer.Name, name, err)
		ctx.RenderWithErrDeprecated(DepartmentSafeErrorL(ctx.Locale, "creating a project", err), tplProjectNew, nil)
	}
}

// ProjectPanel is the owner and members box on a project's page.
type ProjectPanel struct {
	Owner *user_model.User
	// OwnerGone is an owner whose account was deleted or switched off: the
	// project is still theirs on paper, and somebody has to be named.
	OwnerGone bool
	Members   []*user_model.User
	CanManage bool
	Action    string
}

func setProjectPanelData(ctx *context.Context) {
	repo := ctx.Repo.Repository
	if !IsProjectRepo(ctx, repo) {
		return
	}
	owner, err := ProjectOwnerOf(ctx, repo.ID)
	if err != nil {
		log.Error("company: project owner of %s: %v", repo.FullName(), err)
		return
	}
	members, err := ProjectMembers(ctx, repo)
	if err != nil {
		log.Error("company: project members of %s: %v", repo.FullName(), err)
		return
	}
	ctx.Data["CompanyProject"] = &ProjectPanel{
		Owner:     owner,
		OwnerGone: owner == nil || !owner.IsActive || owner.ProhibitLogin,
		Members:   members,
		CanManage: CanManageProject(ctx, ctx.Doer, repo),
		Action:    setting.AppSubURL + companyPrefix + "/project/" + repo.OwnerName + "/" + repo.Name + "/members",
	}
}

// ProjectMembersPost adds or removes a member. The owner decides who works on
// a project — from any department — and an administrator can always step in.
func ProjectMembersPost(ctx *context.Context) {
	repo, err := repo_model.GetRepositoryByOwnerAndName(ctx, ctx.PathParam("owner"), ctx.PathParam("repo"))
	if err != nil || !IsProjectRepo(ctx, repo) || !CanManageProject(ctx, ctx.Doer, repo) {
		ctx.NotFound(nil)
		return
	}
	back := repo.Link()

	name := strings.TrimSpace(ctx.FormString("user"))
	u, err := user_model.GetUserByName(ctx, name)
	if err != nil {
		ctx.Flash.Error(ctx.Locale.TrString("company.project.no_such_user", name))
		ctx.Redirect(back)
		return
	}
	ownerID, err := ProjectOwnerID(ctx, repo.ID)
	if err != nil {
		ctx.ServerError("ProjectOwnerID", err)
		return
	}

	if ctx.FormString("action") == "remove" {
		if u.ID == ownerID {
			ctx.Flash.Error(ctx.Locale.TrString("company.project.cannot_remove_owner"))
			ctx.Redirect(back)
			return
		}
		err = removeProjectMember(ctx, repo, u)
		if err == nil {
			ctx.Flash.Success(ctx.Locale.TrString("company.project.member_removed", u.Name))
		}
	} else {
		err = addProjectMember(ctx, repo, u)
		if err == nil {
			ctx.Flash.Success(ctx.Locale.TrString("company.project.member_added", u.Name))
		}
	}
	if errors.Is(err, errNotRealUser) {
		ctx.Flash.Error(ctx.Locale.TrString("company.project.no_such_user", name))
	} else if err != nil {
		log.Error("company: %s changing the members of %s: %v", ctx.Doer.Name, repo.FullName(), err)
		ctx.Flash.Error(DepartmentSafeErrorL(ctx.Locale, "changing project members", err))
	}
	ctx.Redirect(back)
}
