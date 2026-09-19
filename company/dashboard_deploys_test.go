// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"html/template"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The panel is on every dashboard's template but filled for administrators
// only; with nothing set it must render nothing, not fail the page — it
// once failed every department's dashboard with a 500.
func TestDashboardDeploysRendersWithoutAdminData(t *testing.T) {
	funcs := template.FuncMap{
		"ctx":       func() map[string]any { return map[string]any{"Locale": stubLocale{}} },
		"svg":       func(string, ...any) template.HTML { return "<svg/>" },
		"AppSubUrl": func() string { return "" },
		"DateUtils": func() stubDateUtils { return stubDateUtils{} },
		"Eval": func(args ...any) any {
			a, _ := args[0].(int)
			b, _ := args[2].(int)
			if args[1] == "-" {
				return a - b
			}
			return a + b
		},
	}
	tmpl, err := template.New("root").Funcs(funcs).ParseFiles("../custom/templates/company/dashboard_deploys.tmpl")
	require.NoError(t, err)

	var out strings.Builder
	require.NoError(t, tmpl.ExecuteTemplate(&out, "dashboard_deploys.tmpl", map[string]any{}))
	assert.Empty(t, strings.TrimSpace(out.String()))

	out.Reset()
	require.NoError(t, tmpl.ExecuteTemplate(&out, "dashboard_deploys.tmpl", map[string]any{
		"DashboardDeployPage": 2, "DashboardDeployHasNext": true,
		"DashboardDeploys": []dashboardDeploy{{Owner: "PO", Repo: "app", Title: "t", Link: "/x", AppLink: "/y"}},
	}))
	assert.Contains(t, out.String(), "?deploys=1")
	assert.Contains(t, out.String(), "?deploys=3")
	assert.Contains(t, out.String(), "PO/app")
}
