// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"net/url"
	"strings"

	"gitea.dev/models/organization"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

// companyPrefix is where the Register Deploy flow (deploy.go) lives. Kept
// as a constant so the whitelist and RequireSignIn's redirect can't drift
// apart. There is no GET /company landing page — see routes.go.
const companyPrefix = "/company"

const tplNoDepartment templates.TplName = "company/no_department"

// RequireSignIn redirects anonymous visitors to the login page, remembering
// the exact page they were headed to (e.g. a deploy form) via Gitea's
// existing redirect_to mechanism so they land back on it after signing in
// — see docs/company/routing.md.
func RequireSignIn(ctx *context.Context) {
	if ctx.Doer != nil {
		return
	}
	ctx.Redirect(setting.AppSubURL + "/user/login?redirect_to=" + url.QueryEscape(ctx.Req.URL.RequestURI()))
}

// uiWhitelist is the set of native-Gitea path prefixes a signed-in,
// non-admin user may still reach directly. Default-deny: anything not
// listed here (and not exempt, see isProtocolExempt) gets redirected to
// /company. See docs/company/ui-gate.md and docs/company/repo-ui.md for the
// reasoning behind each entry.
var uiWhitelist = []string{
	companyPrefix,
	"/user/login",
	"/user/logout",
	"/user/forgot_password",
	"/user/recover_account",
	// Saves the repo-code sidebar's own collapsed/expanded state
	// (web_src/js/features/repo-view-file-tree.ts) — fired from any repo
	// code page a non-admin is already allowed to browse, nothing to do
	// with the settings menu below despite the URL. Exact path, no
	// sub-paths to worry about.
	"/user/settings/update_preferences",
	// AJAX endpoint the org dashboard's own repo-list widget calls
	// (repo.SearchRepo, reqSignIn only) — results are filtered server-side
	// by the caller's own access, not a jargon-y page a user navigates to.
	"/repo/search",
	// Deployed department apps. These are not Gitea pages at all — the
	// request is proxied to the department's own application — so the gate
	// has nothing to protect here, and the app's own access mode
	// (public/login/org) is what decides. Listed explicitly rather than left
	// to fall through, so it is a decision rather than an accident.
	"/apps/",
	// footer theme switcher (list/apply) — cosmetic per-user preference,
	// no git/admin concepts involved. optSignIn only, same as anonymous
	// visitors already get.
	"/-/web-theme",
}

// staticAssetPrefixes are checked before anything else, and before any DB
// lookup — every page pulls a dozen+ of these (CSS/JS/images), so this path
// must stay cheap.
var staticAssetPrefixes = []string{
	"/assets/",
	"/avatars/",
	"/attachments/",
	"/manifest.json",
	"/favicon.ico",
}

// GateNonAdminUI is appended once to the AfterRouting middleware chain in
// routers/web/web.go's Routes(), right after the session/auth middleware
// runs — so ctx.Doer is always resolved by the time this executes. It
// covers every route registered on the same *web.Router (all of
// registerWebRoutes), which is the entire native web UI in one place.
//
// It also sets ctx.Data["CompanyDeployRequestsLink"] for non-admin signed-in
// users with exactly one department, so
// custom/templates/base/head_navbar.tmpl (rendered on every page, not just
// the org dashboard) can link to it — see docs/company/architecture.md.
// Admins never get this: they oversee every department (the whole point of
// the separate /-/admin/company-activity view), so a link personalized to
// "your one department" would misrepresent their role — even if, like any
// other user, they also happen to hold real membership in one org.
func GateNonAdminUI(ctx *context.Context) {
	if ctx.Doer == nil {
		return // anonymous: REQUIRE_SIGNIN_VIEW + LANDING_PAGE already force them to /user/login, no /company hop needed
	}

	path := ctx.Req.URL.Path
	if hasAnyPrefix(path, staticAssetPrefixes) || isProtocolExempt(path) {
		return // cheap checks first — never worth a DB round trip
	}

	var orgs []*organization.MinimalOrg
	if !ctx.Doer.IsAdmin {
		var err error
		orgs, err = organization.GetUserOrgsList(ctx, ctx.Doer)
		if err != nil {
			ctx.ServerError("GetUserOrgsList", err)
			return
		}
		if len(orgs) == 1 {
			ctx.Data["CompanyDeployRequestsLink"] = setting.AppSubURL + "/org/" + orgs[0].Name + "/dashboard/deploy-requests"
			ctx.Data["CompanyNewRepoLink"] = setting.AppSubURL + "/org/" + orgs[0].Name + "/repo/create"
		}
	}

	if ctx.Doer.IsAdmin {
		return // admins keep full native access — see docs/company/departments.md
	}

	if isRepoScopedAllow(path) || isOrgScopedAllow(path) || isPrefixAllowed(path) || isNotificationsInboxAllow(path) || isUserSettingsAllowed(path) {
		return
	}

	// bare /repo/create (the "+" button in the repo-list sidebar widget,
	// web_src/js/components/DashboardRepoList.vue, still links here — not
	// worth a Vue edit for) — send them into the same simplified,
	// department-scoped create page a direct /org/{org}/repo/create visit
	// would reach, not just the generic department bounce below.
	if path == "/repo/create" && len(orgs) == 1 {
		ctx.Redirect(setting.AppSubURL + "/org/" + orgs[0].Name + "/repo/create")
		return
	}

	redirectToDepartment(ctx, orgs)
}

// redirectToDepartment sends a blocked user straight to their department's
// native dashboard — there is no intermediate /company landing page (see
// routes.go). orgs is whatever GateNonAdminUI already fetched; department
// membership comes from the same org/team assignment LDAP/OAuth2 group sync
// applies at login (docs/company/departments.md), not anything in the URL.
// The zero/multiple-orgs cases render a normal page (base/head+footer),
// not ctx.PlainText — that left someone in either state on a bare
// text response with no navbar, so no way to even log out, until an
// admin fixed their org membership. See custom/templates/company/no_department.tmpl.
func redirectToDepartment(ctx *context.Context, orgs []*organization.MinimalOrg) {
	switch len(orgs) {
	case 1:
		ctx.Redirect(setting.AppSubURL + "/org/" + orgs[0].Name + "/dashboard")
	case 0:
		ctx.Data["Title"] = string(ctx.Locale.Tr("company.gate.no_department_title"))
		ctx.Data["Message"] = string(ctx.Locale.Tr("company.gate.no_department"))
		ctx.HTML(http.StatusOK, tplNoDepartment)
	default:
		ctx.Data["Title"] = string(ctx.Locale.Tr("company.gate.multiple_departments_title"))
		ctx.Data["Message"] = string(ctx.Locale.Tr("company.gate.multiple_departments"))
		ctx.Data["Orgs"] = orgs
		ctx.HTML(http.StatusOK, tplNoDepartment)
	}
}

func hasAnyPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func isPrefixAllowed(path string) bool {
	for _, p := range uiWhitelist {
		if path == p || strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// isUserSettingsAllowed is the settings-menu gate: a non-admin may reach
// their own profile page (bare /user/settings — GET and its own POST) plus
// the Appearance and AI tabs (including their own sub-actions, e.g.
// /appearance/theme) — everything else under /user/settings/ (account,
// change_password, avatar, notifications, security, applications, keys,
// packages, actions, organization, repos, hooks, blocked_users) stays
// blocked. Account is blocked deliberately: it owns email changes, password
// changes and account deletion, which are administered centrally here
// rather than self-served. Deliberately its own function rather than plain
// uiWhitelist entries: uiWhitelist's isPrefixAllowed treats every entry as
// a prefix, and "/user/settings" as a bare prefix would swallow all of the
// above right back in.
func isUserSettingsAllowed(path string) bool {
	if path == "/user/settings" {
		return true
	}
	return strings.HasPrefix(path, "/user/settings/appearance") ||
		strings.HasPrefix(path, "/user/settings/ai") // everyone's own AI key/model — see company/settings_ai.go
}

// reservedFirstSegments mirrors models/user.reservedUsernames — these are
// never a real owner name, so a path starting with one of them is a Gitea
// system route, not "/{owner}/{repo}". Without this check, isRepoScopedAllow
// would wrongly treat e.g. /org/create or /notifications/subscriptions as an
// allowed 2-segment repo path. Not imported directly since that var is
// unexported; kept in sync manually — it changes rarely.
var reservedFirstSegments = map[string]bool{
	"org": true, "repo": true, "user": true, "admin": true,
	"explore": true, "issues": true, "pulls": true, "milestones": true,
	"notifications": true, "api": true, "metrics": true, "assets": true,
	"attachments": true, "avatars": true, "avatar": true, "captcha": true,
	"login": true, "manifest.json": true, "robots.txt": true, "favicon.ico": true,
	"-": true, "company": true,
}

// isRepoScopedAllow allows browsing an individual repo's Code tab
// (/{owner}/{repo}, /{owner}/{repo}/src/..., etc.) while still blocking its
// Issues/Pulls/Actions/Releases/Wiki/Projects/Activity sub-paths, which are
// handled by disabling the unit (repo-ui.md) or, for the ones that must
// stay enabled, by the explicit block list below.
func isRepoScopedAllow(path string) bool {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) < 2 || segs[0] == "" || segs[1] == "" || reservedFirstSegments[segs[0]] {
		return false
	}
	if len(segs) == 2 {
		return true // /{owner}/{repo}
	}
	switch segs[2] {
	case "pulls", "actions", "releases", "activity", "issues", "wiki", "projects", "packages", "milestones":
		return false
	default:
		return true // e.g. /src, /raw, /commits, /branches — code browsing
	}
}

// notificationsInboxAllow is the notification bell inbox and the endpoints
// its own page needs to function (unread status, purge, poll-for-new) —
// deliberately not a blanket "/notifications" prefix, which would also let
// through /notifications/subscriptions and /notifications/watching
// ("구독"/"watching" — explicitly blocked, see docs/company/ui-gate.md).
var notificationsInboxAllow = map[string]bool{
	"/notifications":        true,
	"/notifications/status": true,
	"/notifications/purge":  true,
	"/notifications/new":    true,
}

func isNotificationsInboxAllow(path string) bool {
	return notificationsInboxAllow[path]
}

// isOrgScopedAllow allows the department-scoped dashboard/pulls/milestones
// pages, which Gitea's own org-membership check already restricts to that
// org's members (services/context/org.go, RequireMember: true) — see
// docs/company/ui-gate.md. "repo" is /org/{org}/repo/create
// (company.RepoCreateForOrg/RepoCreateForOrgPost, routers/web/web.go),
// rendered directly at this URL — no redirect involved.
//
// Every one of dashboard/pulls/milestones/issues also has a native
// /{...}/{team} variant (routers/web/web.go) that renders the same page
// filtered to one team — deliberately excluded here (except our own
// /dashboard/deploy-requests), since the team name in the URL is exactly
// the kind of detail the hidden Teams dropdown (custom/templates/user/dashboard/navbar.tmpl)
// was hidden to avoid non-admins seeing.
func isOrgScopedAllow(path string) bool {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) < 3 || segs[0] != "org" {
		return false
	}
	switch segs[2] {
	case "dashboard":
		if len(segs) == 3 {
			return true // bare /org/{org}/dashboard
		}
		if len(segs) == 4 && segs[3] == "deploy-requests" {
			return true // company.DeployRequests
		}
		if len(segs) == 5 && segs[3] == "deploy-requests" {
			return true // company.DeployRequestFiles — /org/{org}/dashboard/deploy-requests/{id}
		}
		if len(segs) == 5 && segs[3] == "-" && segs[4] == "heatmap" {
			return true // the activity heatmap widget, fetched client-side from the dashboard itself
		}
		return false
	case "repo":
		return len(segs) == 4 && segs[3] == "create"
	case "pulls", "milestones", "issues", "members":
		return len(segs) == 3
	default:
		return false
	}
}

// isProtocolExempt matches git smart-HTTP and LFS endpoints, which must
// always bypass the gate regardless of role — an HTML redirect response
// here just breaks the git client. See docs/company/ui-gate.md.
func isProtocolExempt(path string) bool {
	for _, suffix := range []string{"/git-upload-pack", "/git-receive-pack", "/info/refs"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return strings.Contains(path, "/info/lfs/")
}
