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
	"strconv"
	"strings"
	"sync"
	"time"

	organization "gitea.dev/models/organization"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
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
	"application/xml", "text/xml",
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
		ctx.PlainText(http.StatusNotFound, "This app does not exist.")
		return
	}

	settings := SettingsFor(ref.Owner, ref.Repo)
	if !settings.IsEnabled() {
		ctx.PlainText(http.StatusNotFound, "This app does not exist.")
		return
	}
	if !checkAppAccess(ctx, ref, settings) {
		return // checkAppAccess has written the response
	}
	if !IsAppRunning(ref.Owner, ref.Repo) {
		// Deliberately plain and specific: whoever hits this needs to know it
		// is the app that is down, not the platform.
		ctx.PlainText(http.StatusServiceUnavailable, "This app is not running at the moment.")
		return
	}

	rec := &statusRecorder{ResponseWriter: ctx.Resp, status: http.StatusOK}
	started := time.Now()
	appProxyFor(ref).ServeHTTP(rec, ctx.Req)
	RecordRequest(ref.Owner, ref.Repo, rec.status, time.Since(started), visitorKey(ctx))
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
	switch settings.Access {
	case AccessLogin:
		if !ctx.IsSigned {
			redirectToLogin(ctx)
			return false
		}
		return true
	case AccessOrg:
		if !ctx.IsSigned {
			redirectToLogin(ctx)
			return false
		}
		if ctx.Doer.IsAdmin {
			return true
		}
		member, err := isOrgMember(ctx, ref.Owner, ctx.Doer.ID)
		if err != nil {
			// fail-closed: this is an access decision, and "the database was
			// briefly unavailable" must not read as "let them in".
			log.Error("company: app access check for %s/%s: %v", ref.Owner, ref.Repo, err)
			ctx.PlainText(http.StatusServiceUnavailable, "Access could not be verified. Please try again.")
			return false
		}
		if !member {
			ctx.PlainText(http.StatusForbidden, "This app is restricted to members of "+ref.Owner+".")
			return false
		}
		return true
	default: // AccessPublic
		return true
	}
}

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
	key := appKey(ref.Owner, ref.Repo)
	if p, ok := proxyCache.Load(key); ok {
		if proxy, ok := p.(*httputil.ReverseProxy); ok {
			return proxy
		}
	}

	socket := AppSocketPath(ref.Owner, ref.Repo)
	prefix := appProxyPrefix + "/" + ref.Owner + "/" + ref.Repo

	proxy := &httputil.ReverseProxy{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				// The address is ignored: every connection goes to this app's
				// own socket, whose path comes from the registry rather than
				// from anything in the request.
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
			ResponseHeaderTimeout: proxyResponseHeaderTimeout,
			IdleConnTimeout:       proxyIdleConnTimeout,
		},
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: "app"})
			r.Out.Host = "app"
			// --root-path tells the app what prefix to *build* URLs with; it
			// does not remove that prefix from what arrives. Forwarding the
			// mounted path unchanged makes every app answer its own 404.
			r.Out.URL.Path = appRelativePath(r.In.URL.Path)
			r.Out.URL.RawPath = ""
			// Strip first, then set: whatever the client sent is a claim, and
			// the app must only ever see what we assert.
			for _, h := range forwardedHeadersToStrip {
				r.Out.Header.Del(h)
			}
			// The app is told where it is mounted so it can build absolute
			// URLs that survive the prefix.
			r.Out.Header.Set("X-Forwarded-Prefix", prefix)
			r.Out.Header.Set("X-Forwarded-Proto", "http")
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
			return applyDownloadPolicy(ref, SettingsFor(ref.Owner, ref.Repo), resp)
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

// appRelativePath drops the "/apps/{owner}/{repo}" mount prefix.
//
// Counted in segments rather than trimmed as a string because the URL that
// matched the route may differ in case from the registered app name, and a
// failed trim would forward the whole path as if nothing were wrong.
func appRelativePath(p string) string {
	for range 3 { // "apps", owner, repo
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

	if cd := resp.Header.Get("Content-Disposition"); strings.Contains(strings.ToLower(cd), "attachment") {
		return fmt.Errorf("%w: Content-Disposition attachment", errDownloadBlocked)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !isAllowedContentType(ct) {
		return fmt.Errorf("%w: content type %s", errDownloadBlocked, ct)
	}
	maxBytes := settings.Download.MaxResponseBytes
	if maxBytes <= 0 {
		return nil
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
