// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	organization "gitea.dev/models/organization"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	gitea_context "gitea.dev/services/context"
)

// There is no nginx to configure on the deploy server, so Gitea is the
// reverse proxy: /apps/{owner}/{repo}/* goes to the app's unix socket.
//
// That is not a workaround, it is what makes several of this platform's
// controls possible at all. Because the app has no network of its own
// (--unshare-net) and its only exit is this proxy, *every* response passes
// through here — so the download policy cannot be bypassed and request
// metrics need no log parsing. In an ordinary deployment, where the app
// binds its own port, neither would be true.
//
// Follows the shape of modules/public/vitedev.go's ReverseProxy, with the
// transport dialling a unix socket instead of a TCP address.

// appProxyPrefix is where apps are mounted. Anonymous by design: deployed
// apps are independent of Gitea's authentication unless a department opts
// in (docs/company/app-platform.md).
const appProxyPrefix = "/apps"

// appURL is where an app is opened, with the trailing slash. Without it a
// browser resolves the app's relative links — href="style.css" — against
// /apps/{owner}/, outside the app, and the page arrives unstyled.
func appURL(owner, repo string) string {
	return appProxyPrefix + "/" + owner + "/" + repo + "/"
}

// proxyIdleTimeout et al. bound a single app's ability to tie up Gitea's
// own connections.
const (
	proxyResponseHeaderTimeout = 30 * time.Second
	proxyIdleConnTimeout       = 90 * time.Second
)

// downloadAllowedTypes is what a page needs to render. Anything else — zip,
// octet-stream, spreadsheets — is what a download looks like.
//
// This is a whitelist rather than a blacklist because the set of things a
// browser will save to disk is open-ended, while the set of things a web page
// needs is small and stable.
var downloadAllowedTypes = []string{
	"text/html", "text/plain", "text/css", "text/csv",
	"application/json", "application/javascript", "text/javascript",
	"image/", "font/", "application/font", "application/manifest+json",
	"application/xml", "text/xml", "text/event-stream",
}

// forwardedHeadersToStrip are headers a client must never be able to set on
// the way in. If an app trusts X-Gitea-User to decide who someone is, then
// letting a caller supply it means letting them pick who they are.
var forwardedHeadersToStrip = []string{
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port",
	"X-Forwarded-User", "X-Forwarded-Email",
	"X-Real-Ip",
	"X-Gitea-User", "X-Gitea-User-Id", "X-Gitea-Email", "X-Gitea-Org",
}

// AppProxy serves one request to a deployed app.
func AppProxy(ctx *gitea_context.Context) {
	owner := ctx.PathParam("owner")
	repo := ctx.PathParam("repo")

	// Only apps that got here by being deployed are reachable — the URL is
	// never turned into a socket path directly (appregistry.go).
	ref, known := LookupApp(owner, repo)
	if !known {
		appUnavailable(ctx, http.StatusNotFound, AppRef{Owner: owner, Repo: repo}, "company.appgate.not_found", "", nil)
		return
	}

	settings := SettingsFor(ref.Owner, ref.Repo)
	if !settings.IsEnabled() {
		RecordAccess(ctx, ref, http.StatusNotFound, "app disabled")
		appUnavailable(ctx, http.StatusNotFound, ref, "company.appgate.not_found", "", nil)
		return
	}
	if !checkAppAccess(ctx, ref, settings) {
		return // checkAppAccess has written the response
	}
	// After the access check, so the redirect says nothing about a private
	// app to someone who may not see it.
	if location, ok := rootRedirect(requestPath(ctx.Req), ctx.Req.URL.RawQuery, appMountSegments); ok {
		ctx.Resp.Header().Set("Location", location)
		ctx.Resp.WriteHeader(http.StatusPermanentRedirect) // 308, so a POST stays a POST
		return
	}
	// After the access check, so a refusal here is never a hint about
	// whether a private app exists; before the running check, so a flood
	// does not get to learn whether it is up (company/proxy_guard.go).
	if verdict := guardRequest(ctx, ref); verdict != nil {
		if verdict.retry > 0 {
			ctx.Resp.Header().Set("Retry-After", strconv.Itoa(int(verdict.retry.Seconds())+1))
		}
		ctx.PlainText(verdict.status, verdict.body)
		RecordAccess(ctx, ref, verdict.status, verdict.reason) // the reason, not the body: the body is written to mislead a scanner
		return
	}
	if !guardBodyLimit(ctx) {
		RecordAccess(ctx, ref, http.StatusRequestEntityTooLarge, "body over limit")
		return
	}
	if !IsAppRunning(ref.Owner, ref.Repo) {
		// Deliberately plain and specific: whoever hits this needs to know it
		// is the app that is down, not the platform.
		appUnavailable(ctx, http.StatusServiceUnavailable, ref, "company.appgate.not_running", "company.appgate.not_running.detail", nil)
		return
	}

	rec := &statusRecorder{ResponseWriter: ctx.Resp, status: http.StatusOK}
	started := time.Now()
	appProxyFor(ref).ServeHTTP(rec, ctx.Req)
	RecordRequest(ref.Owner, ref.Repo, rec.status, time.Since(started), visitorKey(ctx))
	RecordAccess(ctx, ref, rec.status, "")
}

const tplAppUnavailable templates.TplName = "company/app_unavailable"

// appUnavailable is the page an app's visitor gets instead of the app: in
// their language, with somewhere to go. It was a bare English sentence, and
// the employee reading it had no way to tell whose problem it was.
func appUnavailable(ctx *gitea_context.Context, status int, ref AppRef, heading, detail string, arg any) {
	ctx.Data["Title"] = ctx.Locale.TrString(heading)
	ctx.Data["Heading"] = heading
	ctx.Data["Detail"] = detail
	ctx.Data["DetailArg"] = arg
	if ref.Owner != "" && ctx.IsSigned {
		ctx.Data["AppPageLink"] = setting.AppSubURL + "/" + url.PathEscape(ref.Owner) + "/" + url.PathEscape(ref.Repo) + "/_app"
	}
	ctx.HTML(status, tplAppUnavailable)
}

// visitorKey identifies a distinct user for the "how many people used this"
// count: the account when the app requires sign-in, the client address when
// it does not. The two are not comparable, and the admin screen says so.
func visitorKey(ctx *gitea_context.Context) string {
	if ctx.IsSigned && ctx.Doer != nil {
		return "u:" + strconv.FormatInt(ctx.Doer.ID, 10)
	}
	host, _, err := net.SplitHostPort(ctx.Req.RemoteAddr)
	if err != nil {
		host = ctx.Req.RemoteAddr
	}
	return "a:" + host
}

// checkAppAccess enforces the app's access mode, writing the response and
// returning false when it denies.
func checkAppAccess(ctx *gitea_context.Context, ref AppRef, settings AppSettings) bool {
	// The instance ceiling wins over what is written for the app: a mode
	// committed before the ceiling was lowered must not stay in force just
	// because it is still in the file (company/inbound_policy.go).
	switch mode := clampAccess(settings.Access); mode {
	case AccessLogin, AccessOrg:
		if !ctx.IsSigned {
			RecordAccess(ctx, ref, http.StatusSeeOther, "sign-in required")
			redirectToLogin(ctx)
			return false
		}
		if mode == AccessOrg && !ctx.Doer.IsAdmin {
			member, err := isOrgMember(ctx, ref.Owner, ctx.Doer.ID)
			if err != nil {
				// fail-closed: this is an access decision, and "the database was
				// briefly unavailable" must not read as "let them in".
				log.Error("company: app access check for %s/%s: %v", ref.Owner, ref.Repo, err)
				RecordAccess(ctx, ref, http.StatusServiceUnavailable, "membership could not be checked")
				appUnavailable(ctx, http.StatusServiceUnavailable, ref, "company.appgate.verify_failed", "", nil)
				return false
			}
			if !member {
				RecordAccess(ctx, ref, http.StatusForbidden, "not a member of "+ref.Owner)
				appUnavailable(ctx, http.StatusForbidden, ref, "company.appgate.members_only", "company.appgate.members_only.detail", ref.Owner)
				return false
			}
		}
		// For an app behind sign-in the platform is the authentication, and
		// a form on another site would arrive with the visitor's session and a
		// valid X-Gitea-User. Gitea's own check, applied to unsafe methods.
		if err := appCrossOrigin.Check(ctx.Req); err != nil {
			RecordAccess(ctx, ref, http.StatusForbidden, "cross-site request")
			appUnavailable(ctx, http.StatusForbidden, ref, "company.appgate.cross_site", "", nil)
			return false
		}
		return true
	case AccessPublic, "":
		return true
	default:
		// A mode nothing recognises is a corrupted record, and the one thing
		// it must not mean is "everyone".
		log.Error("company: %s/%s has an unknown access mode %q; refusing", ref.Owner, ref.Repo, mode)
		RecordAccess(ctx, ref, http.StatusForbidden, "unknown access mode")
		appUnavailable(ctx, http.StatusForbidden, ref, "company.appgate.bad_access", "company.appgate.tell_admin", nil)
		return false
	}
}

// appCrossOrigin is the same Sec-Fetch-Site based check Gitea applies to
// its own forms (routers/web/web.go).
var appCrossOrigin = http.NewCrossOriginProtection()

func redirectToLogin(ctx *gitea_context.Context) {
	ctx.Redirect(setting.AppSubURL + "/user/login?redirect_to=" + url.QueryEscape(ctx.Req.URL.RequestURI()))
}

// orgIDCache maps an organization name to its id. Membership itself is never
// cached — revoking someone's access has to take effect on their next
// request, not when a cache happens to expire. Names are stable enough to
// cache because renaming an organization changes the app's URL anyway.
var orgIDCache sync.Map // lowercased org name -> int64

func isOrgMember(ctx context.Context, orgName string, userID int64) (bool, error) {
	key := strings.ToLower(orgName)
	orgID, ok := orgIDCache.Load(key)
	if !ok {
		org, err := organization.GetOrgByName(ctx, orgName)
		if err != nil {
			return false, err
		}
		orgID = org.ID
		orgIDCache.Store(key, orgID)
	}
	id, ok := orgID.(int64)
	if !ok {
		return false, errors.New("cached organization id had the wrong type")
	}
	return organization.IsOrganizationMember(ctx, id, userID)
}

// proxyCache holds one ReverseProxy per app. Building one per request would
// discard the connection pool every time, which for a unix socket means a
// fresh connect and handshake on each hit.
var proxyCache sync.Map // appKey -> *httputil.ReverseProxy

// The cached proxy must not capture policy: an admin who tightens an app's
// access or download rules in apps.yml expects that to take effect, and a
// proxy built once at first request would keep the old rules for the life of
// the process. Every closure below re-reads SettingsFor, which is an
// in-memory lookup precisely so this is affordable per request.
func appProxyFor(ref AppRef) *httputil.ReverseProxy {
	return appProxyWith(appKey(ref.Owner, ref.Repo), ref, AppSocketPath(ref.Owner, ref.Repo),
		appProxyPrefix+"/"+ref.Owner+"/"+ref.Repo, appMountSegments)
}

// appProxyWith is the proxy for one socket mounted at one prefix. ref names
// the app whose policy applies; a preview (company/apppreview.go) passes the
// live app's, with its own socket, prefix and cache key.
func appProxyWith(key string, ref AppRef, socket, prefix string, segments int) *httputil.ReverseProxy {
	if p, ok := proxyCache.Load(key); ok {
		if proxy, ok := p.(*httputil.ReverseProxy); ok {
			return proxy
		}
	}

	proxy := &httputil.ReverseProxy{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				// The address is ignored: every connection goes to this app's
				// own socket, whose path comes from the registry rather than
				// from anything in the request.
				return dialAppSocket(ctx, socket)
			},
			ResponseHeaderTimeout: proxyResponseHeaderTimeout,
			IdleConnTimeout:       proxyIdleConnTimeout,
		},
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: internalAppHost})
			// The real Host, not the internal one. The transport ignores it —
			// every connection goes to the app's socket — but the app builds
			// absolute redirects from it, and "app" is a name this proxy
			// invented that resolves to nothing in a browser.
			r.Out.Host = r.In.Host
			// --root-path tells the app what prefix to *build* URLs with; it
			// does not remove that prefix from what arrives. Forwarding the
			// mounted path unchanged makes every app answer its own 404.
			r.Out.URL.Path = stripSegments(requestPath(r.In), segments)
			r.Out.URL.RawPath = ""
			// Strip first, then set: whatever the client sent is a claim, and
			// the app must only ever see what we assert.
			for _, h := range forwardedHeadersToStrip {
				r.Out.Header.Del(h)
			}
			// Gitea's own cookies are Path=/ and so arrive here too. Forwarded,
			// the visitor's session — an administrator's, often — was handed to
			// the department's code to replay as them. The app gets only the
			// cookies it set itself. Authorization goes for the same reason.
			r.Out.Header.Del("Authorization")
			r.Out.Header.Del("Cookie")
			for _, c := range r.In.Cookies() {
				if !isGiteaCookie(c.Name) {
					r.Out.AddCookie(c)
				}
			}
			// The app is told where it is mounted so it can build absolute
			// URLs that survive the prefix.
			r.Out.Header.Set("X-Forwarded-Prefix", prefix)
			// The scheme the visitor actually used, so an app behind TLS does
			// not redirect them back to plain http.
			r.Out.Header.Set("X-Forwarded-Proto", requestScheme(r.In))
			// Identity is only forwarded when the app actually requires
			// sign-in. Sending it to a public app would invite it to trust a
			// header that, for a public app, means nothing.
			if SettingsFor(ref.Owner, ref.Repo).Access != AccessPublic {
				if doer := doerFromRequest(r.In); doer != "" {
					r.Out.Header.Set("X-Gitea-User", doer)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			settings := SettingsFor(ref.Owner, ref.Repo)
			rewriteMountedPaths(prefix, resp)
			// Not under a Content-Security-Policy: an inline script would need
			// an exception written into a policy someone chose on purpose.
			rewriteHTMLBody(prefix, resp, !cspInForce(settings, resp), settings.Download.Policy != "allow")
			applySecurityHeaders(settings, resp)
			return applyDownloadPolicy(ref, settings, resp)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the client went away; nothing to report
			}
			if errors.Is(err, errDownloadBlocked) {
				RecordBlockedDownload(ref.Owner, ref.Repo)
				http.Error(w, "This app is not allowed to send files. Ask an administrator to approve file downloads for it.",
					http.StatusForbidden)
				return
			}
			log.Error("company: proxying to %s/%s: %v", ref.Owner, ref.Repo, err)
			http.Error(w, "This app did not respond.", http.StatusBadGateway)
		},
	}

	actual, _ := proxyCache.LoadOrStore(key, proxy)
	if cached, ok := actual.(*httputil.ReverseProxy); ok {
		return cached
	}
	return proxy
}

// appRootRedirect sends the bare app root to its slashed form, as a web
// server treats a directory (see appURL). Relative, so it holds under a
// sub-path.
func appRootRedirect(reqPath, rawQuery string) (string, bool) {
	return rootRedirect(reqPath, rawQuery, appMountSegments)
}

func rootRedirect(reqPath, rawQuery string, segments int) (string, bool) {
	if strings.HasSuffix(reqPath, "/") || stripSegments(reqPath, segments) != "/" {
		return "", false
	}
	location := path.Base(reqPath) + "/"
	if rawQuery != "" {
		location += "?" + rawQuery
	}
	return location, true
}

// requestPath is the request's path with the trailing slash the client sent.
// The router drops it before any handler runs (modules/web/router.go), but to
// an app "/items" and "/items/" are different routes: Starlette answers one
// with a redirect to the other, which then arrived without its slash again.
func requestPath(r *http.Request) string {
	p := r.URL.Path
	sent, _, _ := strings.Cut(r.RequestURI, "?")
	if strings.HasSuffix(sent, "/") && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// appRelativePath drops the "/apps/{owner}/{repo}" mount prefix.
//
// Counted in segments rather than trimmed as a string because the URL that
// matched the route may differ in case from the registered app name, and a
// failed trim would forward the whole path as if nothing were wrong.
func appRelativePath(p string) string { return stripSegments(p, appMountSegments) }

// appMountSegments is "apps", owner, repo.
const appMountSegments = 3

func stripSegments(p string, segments int) string {
	for range segments {
		p = strings.TrimPrefix(p, "/")
		i := strings.IndexByte(p, '/')
		if i < 0 {
			return "/"
		}
		p = p[i:]
	}
	if p == "" {
		return "/"
	}
	return p
}

// doerFromRequest reads the signed-in user off the inbound request's context,
// which Gitea's own middleware has already populated.
func doerFromRequest(req *http.Request) string {
	ctx := gitea_context.GetWebContext(req.Context())
	if ctx == nil || !ctx.IsSigned || ctx.Doer == nil {
		return ""
	}
	return ctx.Doer.Name
}

var errDownloadBlocked = errors.New("download blocked by policy")

// applyDownloadPolicy is the second half of "apps may not hand out files".
//
// Being honest about what this does and does not stop: it removes the
// download *button* and blocks automated bulk export. Anything visible on
// screen can still be copied, screenshotted, or saved by the browser, and
// data can be dribbled out under the size cap. See
// docs/company/app-platform.md — an admin who believes this prevents all
// exfiltration has been misled.
func applyDownloadPolicy(ref AppRef, settings AppSettings, resp *http.Response) error {
	if settings.Download.Policy == "allow" {
		return nil
	}
	// A protocol switch has no body to police: from here on the connection
	// is the app's and the browser's, a stream the size cap would only
	// break. What crosses it is not covered by this policy.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return nil
	}

	if cd := resp.Header.Get("Content-Disposition"); strings.Contains(strings.ToLower(cd), "attachment") {
		return fmt.Errorf("%w: Content-Disposition attachment", errDownloadBlocked)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" && hasBody(resp) {
		// With nosniff set, a body with no type is what a browser saves to
		// disk — the one response an app would craft to get a file out.
		// Named text, it is shown instead.
		ct = "text/plain; charset=utf-8"
		resp.Header.Set("Content-Type", ct)
	}
	if ct != "" && !isAllowedContentType(ct) {
		return fmt.Errorf("%w: content type %s", errDownloadBlocked, ct)
	}
	maxBytes := settings.Download.MaxResponseBytes
	if maxBytes <= 0 || strings.HasPrefix(strings.ToLower(ct), "text/event-stream") {
		return nil // an event stream is meant to run for hours
	}
	if resp.ContentLength > maxBytes {
		return fmt.Errorf("%w: %d bytes", errDownloadBlocked, resp.ContentLength)
	}
	// A chunked response declares no length, so the cap has to be enforced as
	// the body streams. The status line is already sent by then, so this
	// truncates rather than refusing — which still stops the bulk export,
	// just less tidily.
	resp.Body = &cappedBody{ReadCloser: resp.Body, remaining: maxBytes, ref: ref}
	return nil
}

// hasBody reports whether a response can carry one at all.
func hasBody(resp *http.Response) bool {
	if resp.ContentLength == 0 || resp.StatusCode/100 == 1 || resp.StatusCode == http.StatusNoContent ||
		resp.StatusCode == http.StatusNotModified || (resp.Request != nil && resp.Request.Method == http.MethodHead) {
		return false
	}
	return true
}

func isAllowedContentType(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	for _, allowed := range downloadAllowedTypes {
		if strings.HasSuffix(allowed, "/") {
			if strings.HasPrefix(ct, allowed) {
				return true
			}
		} else if ct == allowed {
			return true
		}
	}
	return false
}

// cappedBody stops a response once it exceeds the size limit.
type cappedBody struct {
	io.ReadCloser
	remaining int64
	ref       AppRef
	tripped   bool
}

func (c *cappedBody) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		if !c.tripped {
			c.tripped = true
			RecordBlockedDownload(c.ref.Owner, c.ref.Repo)
			log.Warn("company: %s/%s: response exceeded the size limit and was truncated", c.ref.Owner, c.ref.Repo)
		}
		return 0, io.EOF
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.ReadCloser.Read(p)
	c.remaining -= int64(n)
	return n, err
}

// statusRecorder captures the status code for metrics without buffering the
// body — the response still streams straight through.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.written {
		r.status = status
		r.written = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

// Flush and Unwrap keep streaming responses (server-sent events, long
// polling) working through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// rewriteMountedPaths puts the mount prefix back on paths the app sends out.
//
// The app is told where it is mounted (--root-path) and uses that for URLs it
// builds itself, which is why its own links work. That does not cover a path
// written literally — a RedirectResponse("/docs"), Starlette's trailing-slash
// redirect, a Location on a 201 — and those arrive as "/docs", which is not
// this app at all. Following one takes the visitor out of the app and into
// Gitea, where they get a login page or a 404 for a page that exists.
//
// The department cannot fix this: the prefix is the platform's choice, not
// something their code knows or should have to. So the proxy that imposed the
// prefix is what puts it back.
func rewriteMountedPaths(prefix string, resp *http.Response) {
	sameHost := ""
	if resp.Request != nil {
		sameHost = resp.Request.Host
	}
	if location := resp.Header.Get("Location"); location != "" {
		if rewritten := prefixDocumentURLFor(prefix, sameHost, location); rewritten != location {
			resp.Header.Set("Location", rewritten)
		}
	}

	// Cookie paths for the same reason, and one more: an app setting Path=/
	// has its cookie sent to Gitea itself and to every other department's app
	// on this host. Scoping it to the app's own prefix keeps it where it
	// belongs and out of everything else.
	cookies := resp.Header.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	rewritten := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if scoped, ok := scopeCookie(prefix, cookie); ok {
			rewritten = append(rewritten, scoped)
		}
	}
	resp.Header.Del("Set-Cookie")
	for _, cookie := range rewritten {
		resp.Header.Add("Set-Cookie", cookie)
	}
}

// scopeCookie confines a Set-Cookie to the app. Parsed and rebuilt rather
// than patched: patching the first Path= left a second one — the one a
// browser obeys — untouched, and "Path=/decoy; Path=/" reached Gitea's own
// cookie jar. A cookie with no Path gets the app's, not the directory of
// whichever page set it. Domain is dropped, which would widen it to other
// hosts; a cookie named like one of Gitea's own is dropped entirely, since
// setting it is replacing the visitor's session.
func scopeCookie(prefix, header string) (string, bool) {
	c, err := http.ParseSetCookie(header)
	if err != nil || isGiteaCookie(c.Name) {
		return "", false
	}
	c.Path = prefixDocumentURL(prefix, c.Path)
	if c.Path == "" {
		c.Path = prefix + "/"
	}
	c.Domain = ""
	return c.String(), true
}

// isGiteaCookie names the cookies that are Gitea's rather than an app's: the
// ones an app must never receive, and must never be allowed to set.
func isGiteaCookie(name string) bool {
	switch name {
	case setting.SessionConfig.CookieName, setting.CookieRememberName, // as configured
		"i_like_gitea", "gitea_incredible", // and as shipped, whatever the configuration says
		"lang", "_csrf", "redirect_to", "gitea_flash", "macaron_flash", "sudo":
		return true
	}
	return name == "" // ParseSetCookie refuses these; the Cookie header parser does not
}

// internalAppHost is the placeholder the transport is handed. Every connection
// goes to the app's unix socket regardless, so the value is arbitrary — but it
// must never reach a browser, because it resolves to nothing.
const internalAppHost = "app"

// requestScheme reports how the visitor reached Gitea.
//
// From the instance's own configured URL, not from the request's
// X-Forwarded-Proto: that header is client-supplied on the way in — it is in
// forwardedHeadersToStrip for exactly that reason — and taking it here would
// let a caller decide what scheme the app builds its URLs with. req.TLS still
// counts, since Gitea terminated that connection itself.
func requestScheme(req *http.Request) string {
	if req.TLS != nil || strings.HasPrefix(setting.AppURL, "https://") {
		return "https"
	}
	return "http"
}
