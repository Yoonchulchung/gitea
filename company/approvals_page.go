// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"net/http"

	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/container"
	"gitea.dev/modules/log"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplApprovals templates.TplName = "company/approvals"

const (
	approvalsPageSize  = 30
	dashboardApprovals = 5
)

// Approvals is the administrator's one place for everything waiting on them.
func Approvals(ctx *context.Context) {
	if !ctx.Doer.IsAdmin {
		ctx.NotFound()
		return
	}
	done := ctx.FormString("state") == "done"
	kind := ctx.FormString("kind")
	if kind != ApprovalKindProject && kind != ApprovalKindDeploy {
		kind = ""
	}
	page := max(ctx.FormInt("page"), 1)
	reqs, total, err := FindApprovals(ctx, !done, kind, approvalsPageSize, (page-1)*approvalsPageSize)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	pending, err := CountPendingApprovals(ctx)
	if err != nil {
		ctx.ServerErrorAuto(err)
		return
	}
	pager := context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(approvalsPageSize).CurPage(page).Build()
	pager.RemoveParam(container.SetOf("page"))

	ctx.Data["Title"] = ctx.Locale.TrString("company.approvals.title")
	ctx.Data["PageIsCompanyApprovals"] = true
	ctx.Data["Approvals"] = reqs
	ctx.Data["ApprovalsDone"] = done
	ctx.Data["ApprovalsKind"] = kind
	ctx.Data["ApprovalsPending"] = pending
	ctx.Data["ApprovalsLink"] = approvalsLink()
	ctx.Data["ApprovalsPager"] = pager
	ctx.HTML(http.StatusOK, tplApprovals)
}

// ApprovalApprove and ApprovalReject decide a project. A deploy is decided on
// its own review page, where its permission items are.
func ApprovalApprove(ctx *context.Context) {
	if !ctx.Doer.IsAdmin {
		ctx.NotFound()
		return
	}
	req, repo, err := ApproveProjectRequest(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		ctx.Flash.Error(approvalDecisionError(ctx, err))
	} else {
		log.Info("company: %s approved project request %d, creating %s", ctx.Doer.Name, req.ID, repo.FullName())
		ctx.Flash.Success(ctx.Locale.TrString("company.approvals.project_approved", repo.Name))
	}
	ctx.Redirect(approvalsLink())
}

func ApprovalReject(ctx *context.Context) {
	if !ctx.Doer.IsAdmin {
		ctx.NotFound()
		return
	}
	if err := RejectProjectRequest(ctx, ctx.Doer, ctx.PathParamInt64("id")); err != nil {
		ctx.Flash.Error(approvalDecisionError(ctx, err))
	} else {
		ctx.Flash.Success(ctx.Locale.TrString("company.approvals.rejected"))
	}
	ctx.Redirect(approvalsLink())
}

func approvalDecisionError(ctx *context.Context, err error) string {
	switch {
	case errors.Is(err, errProjectRequestNotPending):
		return ctx.Locale.TrString("company.project_request.not_pending")
	case repo_model.IsErrRepoAlreadyExist(err), repo_model.IsErrRepoFilesAlreadyExist(err):
		return ctx.Locale.TrString("company.project.name_taken")
	case user_model.IsErrUserNotExist(err):
		return ctx.Locale.TrString("company.approvals.requester_gone")
	}
	log.Error("company: %s deciding an approval request: %v", ctx.Doer.Name, err)
	return AdminErrorL(ctx.Locale, err)
}

// SetDashboardApprovals puts what is waiting on an administrator's dashboard.
// Best-effort: the dashboard renders without it.
func SetDashboardApprovals(ctx *context.Context) {
	if ctx.Doer == nil || !ctx.Doer.IsAdmin {
		return
	}
	reqs, total, err := FindApprovals(ctx, true, "", dashboardApprovals, 0)
	if err != nil {
		log.Error("company: dashboard approvals: %v", err)
		return
	}
	ctx.Data["DashboardApprovals"] = reqs
	ctx.Data["DashboardApprovalsTotal"] = total
	ctx.Data["ApprovalsLink"] = approvalsLink()
}
