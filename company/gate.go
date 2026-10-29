// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"net/url"
	"strings"

	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
)

// companyPrefix is where the Register Deploy flow (deploy.go) lives. Kept
// as a constant so the whitelist and RequireSignIn's redirect can't drift
// apart. There is no GET /company landing page — see routes.go.
const companyPrefix = "/company"

// RequireSignIn redirects anonymous visitors to the login page, remembering
// the exact page they were headed to (e.g. a deploy form) via Gitea's
// existing redirect_to mechanism so they land back on it after signing in
// — see docs/company/routing.md.
func RequireSignIn(ctx *context.Context) {
	if ctx.Doer == nil {
		ctx.Redirect(setting.AppSubURL + "/user/login?redirect_to=" + url.QueryEscape(ctx.Req.URL.RequestURI()))
		return
	}
	// The same cross-site check Gitea's own forms get: without it a page on
	// any other site could submit these with the visitor's session.
	if err := appCrossOrigin.Check(ctx.Req); err != nil {
		ctx.HTTPError(http.StatusForbidden, "this request came from another site")
	}
}

// uiWhitelist is the set of native-Gitea path prefixes a signed-in,
// non-admin user may still reach directly. Default-deny: anything not
// listed here (and not exempt, see isProtocolExempt) gets redirected to
// their dashboard. See docs/company/ui-gate.md and docs/company/repo-ui.md
// for the reasoning behind each entry.
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
	// AJAX endpoint the dashboard's own repo-list widget calls
	// (repo.SearchRepo, reqSignIn only) — results are filtered server-side
	// by the caller's own access, not a jargon-y page a user navigates to.
	"/repo/search",
	// Deployed apps. These are not Gitea pages at all — the request is
	// proxied to the project's own application — so the gate has nothing to
	// protect here, and the app's own access mode (public/login/org) is what
	// decides. Listed explicitly rather than left to fall through, so it is a
	// decision rather than an accident.
	"/apps/",
	// Markdown rendering for the AI chat's replies (company-ai-chat.ts). It
	// only turns the posted text into sanitised HTML; blocked, it answered
	// with a redirect to the dashboard, and the chat showed that page as a reply.
	"/-/markup",
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
// It also sets the navbar's deploy-request and new-project links for
// non-admins (custom/templates/base/head_navbar.tmpl). Admins have their own
// menu: they oversee every project from /-/admin.
func GateNonAdminUI(ctx *context.Context) {
	path := ctx.Req.URL.Path
	// An app's own requests render nothing of Gitea's — and there are many of
	// them, each of which was costing a database query for a navbar link.
	renders := !hasAnyPrefix(path, staticAssetPrefixes) && !isProtocolExempt(path) && !strings.HasPrefix(path, appProxyPrefix+"/")
	if renders {
		// The footer is rendered by a template with no handler of its own, so
		// there is no per-page place to put its links. This middleware is on
		// every request that renders anything, including the login page —
		// where someone who cannot get in is exactly who needs the address.
		SetPlatformFooterData(ctx)
		// Which repository is the platform's own, for the repository header:
		// it carries the snapshots every deploy request is made of, and a
		// deploy request *from* it would feed it to itself.
		if owner, name, err := centralDeployOwnerName(); err == nil {
			ctx.Data["CompanyCentralRepoPath"] = owner + "/" + name
		}
	}

	if ctx.Doer == nil {
		// Accounts come from the company directory; a sign-up form would only
		// make accounts nobody provisioned.
		if strings.HasPrefix(path, "/user/sign_up") {
			ctx.NotFound(nil)
		}
		return // anonymous: REQUIRE_SIGNIN_VIEW + LANDING_PAGE already force them to /user/login, no /company hop needed
	}
	if !renders {
		return // cheap checks first — never worth a DB round trip
	}

	// Before the allowlist, because this is what makes the allowlist's
	// refusal survivable rather than a trap.
	clearLocalPasswordChange(ctx)

	if ctx.Doer.IsAdmin {
		return // admins keep full native access — see docs/company/projects.md
	}
	ctx.Data["CompanyDeployRequestsLink"] = setting.AppSubURL + deployRequestsPath
	ctx.Data["CompanyNewRepoLink"] = setting.AppSubURL + newProjectPath

	if path == "/" || isRepoScopedAllow(path) || isPrefixAllowed(path) || isNotificationsInboxAllow(path) || isUserSettingsAllowed(path) {
		return
	}
	// The "+" in the dashboard's repository list still links here.
	if path == "/repo/create" {
		ctx.Redirect(setting.AppSubURL + newProjectPath)
		return
	}
	ctx.Redirect(setting.AppSubURL + "/")
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
// rather than self-served — and with accounts coming from the directory, a
// local password is not the credential anyway, so changing one would achieve
// nothing but the appearance of having done something. There is no exception:
// see clearLocalPasswordChange below for the redirect loop that tempted one. Deliberately its own function rather than plain
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

// clearLocalPasswordChange takes a department member out of Gitea's forced
// local password change.
//
// Accounts here come from the directory, so the local password is not the
// credential and changing it accomplishes nothing. The page that would do it
// is closed to non-admins by policy and stays closed — but Gitea's own
// middleware redirects anyone carrying this flag to that page on every
// request, and a closed page plus a forced redirect is a loop with no way
// through and no way out, not even to log out. An administrator who sets the
// flag from the user screen, not knowing it does not apply here, locks that
// person out of the instance.
//
// So the flag is cleared rather than the page opened. Logged, because an
// administrator who set it deliberately deserves to learn it had no effect —
// and what they actually want is a directory-side reset.
//
// Administrators keep the flag: they are the ones who may still have a local
// password worth changing, and their own screens are not gated.
func clearLocalPasswordChange(ctx *context.Context) {
	if ctx.Doer == nil || ctx.Doer.IsAdmin || !ctx.Doer.MustChangePassword {
		return
	}
	ctx.Doer.MustChangePassword = false
	if err := user_model.UpdateUserCols(ctx, ctx.Doer, "must_change_password"); err != nil {
		// Best-effort: a failure here means the loop persists for this
		// request, not that the request should fail.
		log.Error("company: clearing the forced password change for %s: %v", ctx.Doer.Name, err)
		return
	}
	log.Warn("company: %s was flagged to change their local password; the flag was cleared because passwords here come from the directory. Reset it there instead.", ctx.Doer.Name)
}
