// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package company implements the internal code-intake platform layered on
// top of stock Gitea: a non-technical-friendly entry point for uploading
// files, registering deploy requests (pull requests against a department's
// repository), and an admin view aggregating activity across every
// department (organization).
//
// This package is additive only — see docs/company/ for why, and
// docs/company/patches.md for the exact, small set of lines in Gitea's own
// source that wire it in.
package company

import (
	"gitea.dev/modules/web"
	"gitea.dev/modules/web/routing"
)

// RegisterRoutes builds the /company route tree for signed-in, non-admin
// users. Called with one line from inside routers/web/web.go's
// registerWebRoutes, so it runs on the same *web.Router that already has
// the session/auth middleware applied — see docs/company/mount-points.md.
//
// There is no GET /company landing page — a blocked user is sent straight
// to their department's native dashboard by the gate itself (gate.go,
// redirectToDepartment), not routed through an intermediate page here.
func RegisterRoutes(m *web.Router) {
	// Deployed apps. Registered without optSignIn on purpose: an app is
	// independent of Gitea's authentication unless its department opts in, and
	// optSignIn is per-group rather than global, so this stays anonymous even
	// with REQUIRE_SIGNIN_VIEW on — /api/healthz does the same. The access
	// mode is enforced inside AppProxy instead (company/proxy.go).
	//
	// MarkLongPolling keeps a slow app from being logged and treated as a slow
	// Gitea request; apps stream and hold connections open.
	m.Any("/apps/{owner}/{repo}", routing.MarkLongPolling(), AppProxy)
	m.Any("/apps/{owner}/{repo}/*", routing.MarkLongPolling(), AppProxy)

	m.Group("/company", func() {
		m.Post("/repo-description/{owner}/{repo}", UpdateDescription)
		m.Post("/deploy-request/{id}/cancel", CancelDeployRequest)
		m.Post("/deploy-request/{id}/ai-review", TriggerDeployRequestAIReview)
	}, RequireSignIn)
	// DeployRequests, DeployRequestFiles, and RepoCreateRedirect are NOT
	// registered here — all three mount inside Gitea's own org route group
	// instead (/org/{org}/dashboard/deploy-requests[/{id}], /org/{org}/repo/create),
	// to reuse its membership check. See routers/web/web.go and
	// docs/company/mount-points.md.
	// Workspace/WorkspaceSave, DeployForm/DeployPost/Submitted/DeployStatus
	// (the multi-file editor and "Deploy Request" flow) aren't registered
	// here either — they mount inside Gitea's own repo-code route group
	// instead, at /{owner}/{repo}/_edits/{branch} and /{owner}/{repo}/deploy*,
	// alongside its native _new/_edit/_upload routes, to reuse their branch
	// parsing and write-access gate. See routers/web/web.go.
}

// RegisterAdminRoutes adds the cross-department activity view inside
// Gitea's existing "/-/admin" group. Called with one line placed inside
// that group's closure in routers/web/web.go, so it inherits the group's
// adminReq middleware for free — see docs/company/mount-points.md.
func RegisterAdminRoutes(m *web.Router) {
	m.Get("/company-activity", AdminActivity)
	// App deployment management — see docs/company/app-platform.md. Kept
	// under "/-/admin" rather than a repo-scoped path on purpose: these
	// pages carry logs and failure detail, and isRepoScopedAllow
	// (company/gate.go) is *default-allow* for repo sub-paths, so a
	// /{owner}/{repo}/… route would be reachable by non-admins the moment
	// it existed.
	m.Get("/company-deploys", AdminDeploys)
	// Package policy is its own page: the dashboard is about what is
	// happening now, this is about what every app may install.
	m.Get("/company-packages", AdminPackages)
	m.Post("/company-packages/base", AdminSetBasePackages)
	m.Group("/company-deploys/{owner}/{repo}", func() {
		m.Get("", AdminApp)
		m.Get("/logs", AdminAppLogs)
		m.Get("/history", AdminAppHistory)
		// Before the {verb} catch-all below, which would otherwise swallow it.
		m.Post("/approve-packages", AdminApprovePackages)
		m.Post("/deploy-version", AdminDeployVersion)
		m.Get("/metrics", AdminAppMetrics)
		m.Post("/{verb}", AdminAppControl)
	})
}
