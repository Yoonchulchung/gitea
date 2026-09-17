// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"slices"

	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplAdminServerLogs templates.TplName = "company/admin_server_logs"

// AdminServerLogs shows the instance's own log — what a terminal would have
// shown if this instance logged to one.
//
// Registered inside Gitea's "/-/admin" group, so the group's admin check
// applies; nothing here does its own. See company/routes.go.
func AdminServerLogs(ctx *context.Context) {
	query := ServerLogQuery{
		Text:     ctx.FormString("q"),
		Regexp:   ctx.FormBool("regexp"),
		File:     ctx.FormString("file"),
		MinLevel: ctx.FormString("level"),
	}
	// An unknown level means no level filter rather than an empty page: a
	// hand-edited query string should not look like an instance with no logs.
	if !slices.Contains(serverLogLevels, query.MinLevel) {
		query.MinLevel = ""
	}

	lines, truncated, err := ReadServerLogs(query)

	ctx.Data["Title"] = ctx.Locale.TrString("company.serverlogs.title")
	ctx.Data["Lines"] = lines
	ctx.Data["Truncated"] = truncated
	ctx.Data["Query"] = query.Text
	ctx.Data["UseRegexp"] = query.Regexp
	ctx.Data["Files"] = ServerLogFiles()
	ctx.Data["File"] = query.File
	ctx.Data["Levels"] = serverLogLevels
	ctx.Data["Level"] = query.MinLevel
	if err != nil {
		// A bad regexp is the administrator's typo, not a server fault.
		ctx.Data["SearchError"] = err.Error()
	}

	// Where the lines come from, shown next to them. With MODE=console there
	// is no file to read and the page would otherwise be an unexplained blank
	// — the logs exist, they are just going somewhere this screen cannot see.
	ctx.Data["LogMode"] = setting.Log.Mode
	ctx.Data["LogLevel"] = setting.Log.Level.String()
	ctx.Data["LogRootPath"] = setting.Log.RootPath

	ctx.HTML(http.StatusOK, tplAdminServerLogs)
}
