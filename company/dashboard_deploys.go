// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"
	"gitea.dev/services/context"

	"xorm.io/xorm"
)

// An administrator's dashboard feed is Gitea's: every branch the platform
// created and deleted, every snapshot it pushed, every pull request it
// opened on its own account. What the administrator wants to know is who
// asked to deploy what and how it went, which none of those lines say.
//
// So the recent deploy requests are listed as themselves above the feed,
// and the platform's own actions on the central repository are left out
// of the feed below (custom/templates/user/dashboard/feeds.tmpl).

const dashboardDeployRows = 8

// dashboardDeploy is one request, as the dashboard lists it.
type dashboardDeploy struct {
	Link      string // the request's review page
	AppLink   string
	Owner     string
	Repo      string
	Title     string
	Requester *user_model.User
	Approver  *user_model.User // who merged, for an approved one
	Status    *deployRequestStatus
	At        timeutil.TimeStamp // when it was last touched: asked, decided, or deployed
}

// SetActivityLogPage marks the dashboard rendered at /activity: the feed of
// every push and branch, which an administrator's own dashboard no longer
// carries inline — the deploy activity is what they open it for, and the
// feed buried it. Mounted ahead of Home on /activity (routers/web/web.go).
func SetActivityLogPage(ctx *context.Context) {
	if ctx.Doer == nil || !ctx.Doer.IsAdmin {
		ctx.NotFound(nil)
		return
	}
	ctx.Data["CompanyActivityLog"] = true
}

// RedirectPullsToCentral sends an administrator's "Deploy approvals" menu
// entry (/pulls) to the central repository's own pull request list. The
// global list only shows repositories a person is a member of, and an
// administrator is not a member of the central repository — every deploy
// request was missing from the one page named after them. Mounted ahead
// of user.Pulls on /pulls (routers/web/web.go).
func RedirectPullsToCentral(ctx *context.Context) {
	if ctx.Doer == nil || !ctx.Doer.IsAdmin {
		return
	}
	central, err := centralDeployRepo(ctx)
	if err != nil {
		return // no central repository yet: the plain list is all there is
	}
	ctx.Redirect(central.Link() + "/pulls")
}

// SetDashboardDeploys attaches the recent deploy requests to an
// administrator's dashboard. Mounted ahead of Home on "/" (routers/web/web.go).
// Best-effort, like the apps panel: the dashboard renders without it.
func SetDashboardDeploys(ctx *context.Context) {
	if ctx.Doer == nil || !ctx.Doer.IsAdmin {
		return
	}
	central, err := centralDeployRepo(ctx)
	if err != nil {
		return // no central repository configured yet: nothing to list, and the feed has nothing to hide
	}
	ctx.Data["CompanyCentralRepoPath"] = central.FullName()

	// Its own page parameter, so the feed's paging and this never move
	// each other.
	page := max(ctx.FormInt("deploys"), 1)
	session := func() *xorm.Session {
		return db.GetEngine(ctx).
			Join("INNER", "issue", "issue.id = pull_request.issue_id").
			Where("pull_request.base_repo_id = ?", central.ID).
			And("pull_request.head_repo_id = ?", central.ID).
			And("pull_request.head_branch LIKE ?", "deploy/%")
	}
	total, err := session().Count(new(issues_model.PullRequest))
	if err != nil {
		log.Error("company: dashboard deploys: %v", err)
		return
	}
	var prs []*issues_model.PullRequest
	if err := session().Desc("issue.updated_unix").Limit(dashboardDeployRows, (page-1)*dashboardDeployRows).Find(&prs); err != nil {
		log.Error("company: dashboard deploys: %v", err)
		return
	}
	rows := make([]dashboardDeploy, 0, len(prs))
	{
		for _, pr := range prs {
			if err := pr.LoadIssue(ctx); err != nil {
				continue
			}
			owner, repo, requesterID, ok := parseDeployBranchName(pr.HeadBranch)
			if !ok {
				continue
			}
			row := dashboardDeploy{
				Link:    fmt.Sprintf("%s/pulls/%d", central.Link(), pr.Issue.Index),
				AppLink: fmt.Sprintf("%s/-/admin/company-deploys/%s/%s", setting.AppSubURL, owner, repo),
				Owner:   owner,
				Repo:    repo,
				Title:   pr.Issue.Title,
				At:      pr.Issue.UpdatedUnix,
			}
			// A deleted requester still asked: the row stays, without a name.
			row.Requester, _ = user_model.GetUserByID(ctx, requesterID)
			if pr.HasMerged && pr.MergerID > 0 {
				row.Approver, _ = user_model.GetUserByID(ctx, pr.MergerID)
			}
			if status, err := deployStatusFor(ctx, pr); err == nil {
				row.Status = status
				if status.Date > row.At {
					row.At = status.Date
				}
			}
			rows = append(rows, row)
		}
	}
	ctx.Data["DashboardDeploys"] = rows
	ctx.Data["DashboardDeployPage"] = page
	ctx.Data["DashboardDeployHasNext"] = int64(page*dashboardDeployRows) < total
}
