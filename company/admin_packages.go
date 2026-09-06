// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"slices"
	"sort"
	"strings"

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
		ctx.Flash.Error(AdminError(err))
	} else {
		// Says what actually happened: an environment is built once and
		// reused, so this does not reach apps that are already running.
		ctx.Flash.Success("기본 패키지를 저장했습니다. 새로 배포되는 앱부터 적용됩니다.")
	}
	ctx.Redirect(setting.AppSubURL + "/-/admin/company-packages")
}
