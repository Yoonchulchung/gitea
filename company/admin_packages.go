// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"slices"
	"sort"
	"strings"

	issues_model "gitea.dev/models/issues"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplAdminPackages templates.TplName = "company/admin_packages"

// Package policy has its own page rather than a corner of the deploy
// dashboard, because it answers a different question. The dashboard is about
// what is happening right now; this is about what every app is permitted to
// install, which changes rarely and is read when someone is deciding whether
// to approve something.
//
// Both halves are the same file — `.company/apps.yml` in the central deploy
// repository — so what is shown here and what is in force cannot drift apart.
// See company/permissions_commit.go on why that file, and not a table.

// appPackages is one app's approved additions, for the read-only half.
type appPackages struct {
	Owner    string
	Repo     string
	Packages []string
}

// AdminPackages shows the platform's own stack and every department's
// approved additions.
func AdminPackages(ctx *context.Context) {
	cfg, loadedAt, configErr := AppsConfigSnapshot()
	defaults := cfg.EffectiveSettings("", "")

	// Per-app additions, sorted so the page reads the same way twice. Only
	// what an admin actually approved is listed: the shared base is above,
	// and repeating it under every app would bury the differences that
	// matter.
	rows := make([]appPackages, 0, len(cfg.Apps))
	for key, s := range cfg.Apps {
		if s == nil || len(s.Dependencies.AllowExtra) == 0 {
			continue
		}
		owner, repo, _ := strings.Cut(key, "/")
		extra := slices.Clone(s.Dependencies.AllowExtra)
		sort.Strings(extra)
		rows = append(rows, appPackages{Owner: owner, Repo: repo, Packages: extra})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Owner != rows[j].Owner {
			return rows[i].Owner < rows[j].Owner
		}
		return rows[i].Repo < rows[j].Repo
	})

	pythonOK, pythonDetail := PythonStatus()
	centralOwner, centralName, _ := centralDeployOwnerName()

	ctx.Data["Title"] = "패키지 관리"
	ctx.Data["BasePackages"] = strings.Join(defaults.BasePackages, "\n")
	ctx.Data["SharedAllow"] = defaults.Dependencies.Allow
	ctx.Data["AppPackages"] = rows
	// What is waiting on an admin, oldest first. See pendingApprovals.
	ctx.Data["PendingApprovals"] = pendingApprovals(ctx)
	ctx.Data["PythonOK"] = pythonOK
	ctx.Data["PythonDetail"] = pythonDetail
	ctx.Data["ConfigError"] = configErr
	ctx.Data["ConfigLoadedAt"] = loadedAt.Unix()
	// The commit history of apps.yml *is* the audit record, so the page links
	// to it rather than keeping a second one of its own.
	if centralOwner != "" {
		ctx.Data["PolicyHistoryLink"] = setting.AppSubURL + "/" + centralOwner + "/" + centralName +
			"/commits/branch/" + "main" + "/" + appsConfigPath
	}
	ctx.HTML(http.StatusOK, tplAdminPackages)
}

// AdminSetBasePackages replaces the packages every app gets.
//
// Committed to apps.yml rather than stored anywhere else, so the change is a
// commit authored by the admin who made it.
func AdminSetBasePackages(ctx *context.Context) {
	packages, problems := ParseBasePackages(ctx.FormString("packages"))
	if len(problems) > 0 {
		ctx.Flash.Error(strings.Join(problems, " / "))
		ctx.Redirect(setting.AppSubURL + "/-/admin/company-packages")
		return
	}
	if err := CommitBasePackages(ctx, ctx.Doer, packages); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	} else {
		// Says what actually happened: an environment is built once and
		// reused, so this does not reach apps that are already running.
		ctx.Flash.Success(ctx.Locale.TrString("company.flash.base_saved"))
	}
	ctx.Redirect(setting.AppSubURL + "/-/admin/company-packages")
}

// pendingApproval is one open request an admin has not decided yet, for the
// policy page.
type pendingApproval struct {
	Owner, Repo string
	PRID        int64
	Index       int64 // the PR's visible number, for the review link
	At          int64
	Requests    []PermissionRequest
}

// pendingApprovals collects every open deploy request that is asking for
// something.
//
// The policy page answers "what may apps install"; the natural next question
// is "and what is waiting on me". Until now that answer lived only on each
// org's own dashboard, so a request could sit unnoticed for as long as nobody
// happened to open the right org.
func pendingApprovals(ctx *context.Context) []pendingApproval {
	var out []pendingApproval
	for _, st := range ListAppStates() {
		set := LoadPermissionRequestSet(st.Owner, st.Repo)
		if set == nil || len(set.Requests) == 0 {
			continue
		}
		pr, err := issues_model.GetPullRequestByID(ctx, set.PRID)
		if err != nil || pr.HasMerged {
			continue
		}
		if err := pr.LoadIssue(ctx); err != nil || pr.Issue.IsClosed {
			continue
		}
		out = append(out, pendingApproval{
			Owner: st.Owner, Repo: st.Repo,
			PRID: set.PRID, Index: pr.Index, At: set.At,
			Requests: set.Requests,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At }) // oldest first: it has waited longest
	return out
}
