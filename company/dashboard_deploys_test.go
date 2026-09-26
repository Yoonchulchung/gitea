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
		"dict": func(args ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(args); i += 2 {
				m[args[i].(string)] = args[i+1]
			}
			return m
		},
	}
	// The pager it includes is parsed with it: a sub-template Gitea resolves
	// at runtime is a parse error here, which is what this test would report
	// instead of what it is about.
	tmpl, err := template.New("root").Funcs(funcs).ParseFiles(
		"../custom/templates/company/dashboard_deploys.tmpl",
		"../custom/templates/company/pager.tmpl",
	)
	require.NoError(t, err)
	_, err = tmpl.AddParseTree("company/pager", tmpl.Lookup("pager.tmpl").Tree)
	require.NoError(t, err)

	var out strings.Builder
	require.NoError(t, tmpl.ExecuteTemplate(&out, "dashboard_deploys.tmpl", map[string]any{}))
	assert.Empty(t, strings.TrimSpace(out.String()))

	// With rows but no pager the panel still renders — the pager is a separate
	// piece of data, and a missing one must not take the list with it.
	out.Reset()
	require.NoError(t, tmpl.ExecuteTemplate(&out, "dashboard_deploys.tmpl", map[string]any{
		"DashboardDeployPage": 2,
		"DashboardDeploys":    []dashboardDeploy{{Owner: "PO", Repo: "app", Title: "t", Link: "/x", AppLink: "/y"}},
	}))
	assert.Contains(t, out.String(), "PO/app")
	assert.Contains(t, out.String(), "/x")
}
