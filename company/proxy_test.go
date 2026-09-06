// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The registry is the reason a URL never becomes a socket path directly.
func TestAppRegistry(t *testing.T) {
	RegisterApp("PO", "MyApp")
	t.Cleanup(func() { UnregisterApp("PO", "MyApp") })

	ref, ok := LookupApp("po", "myapp")
	require.True(t, ok, "Gitea resolves owner and repo names case-insensitively, so the proxy must too")
	assert.Equal(t, AppRef{Owner: "PO", Repo: "MyApp"}, ref, "the canonical spelling is what reaches the filesystem")

	_, ok = LookupApp("PO", "not-an-app")
	assert.False(t, ok)
	_, ok = LookupApp("..", "..")
	assert.False(t, ok, "an invented name must never resolve")

	UnregisterApp("PO", "MyApp")
	_, ok = LookupApp("PO", "MyApp")
	assert.False(t, ok)
}

// Whatever the client sent is a claim. If an app trusts X-Gitea-User to
// decide who someone is, a caller who can set it picks who they are.
func TestForwardedHeadersAreStripped(t *testing.T) {
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-User", "X-Gitea-User", "X-Real-Ip"} {
		assert.Contains(t, forwardedHeadersToStrip, http.CanonicalHeaderKey(header), header)
	}
}

func TestIsAllowedContentType(t *testing.T) {
	for _, ct := range []string{
		"text/html", "text/html; charset=utf-8", "TEXT/HTML",
		"application/json", "image/png", "font/woff2", "text/csv",
	} {
		assert.True(t, isAllowedContentType(ct), ct)
	}
	// These are what a download looks like. The list is a whitelist because
	// the set of things a browser will save is open-ended, while the set of
	// things a page needs is small.
	for _, ct := range []string{
		"application/octet-stream",
		"application/zip",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/pdf",
		"application/x-tar",
	} {
		assert.False(t, isAllowedContentType(ct), ct)
	}
}

func TestApplyDownloadPolicy(t *testing.T) {
	ref := AppRef{Owner: "PO", Repo: "app"}
	blocking := AppSettings{Download: AppDownload{Policy: "block", MaxResponseBytes: 1000}}

	t.Run("attachment is refused", func(t *testing.T) {
		resp := &http.Response{Header: http.Header{
			"Content-Type":        []string{"text/html"},
			"Content-Disposition": []string{`attachment; filename="report.html"`},
		}}
		assert.ErrorIs(t, applyDownloadPolicy(ref, blocking, resp), errDownloadBlocked)
	})

	t.Run("a download content type is refused", func(t *testing.T) {
		resp := &http.Response{Header: http.Header{"Content-Type": []string{"application/zip"}}}
		assert.ErrorIs(t, applyDownloadPolicy(ref, blocking, resp), errDownloadBlocked)
	})

	t.Run("a declared oversize response is refused", func(t *testing.T) {
		resp := &http.Response{
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			ContentLength: 5000,
		}
		assert.ErrorIs(t, applyDownloadPolicy(ref, blocking, resp), errDownloadBlocked)
	})

	t.Run("a normal page passes", func(t *testing.T) {
		resp := &http.Response{
			Header:        http.Header{"Content-Type": []string{"text/html"}},
			ContentLength: 200,
			Body:          io.NopCloser(strings.NewReader("<html></html>")),
		}
		require.NoError(t, applyDownloadPolicy(ref, blocking, resp))
	})

	t.Run("policy allow disables all of it", func(t *testing.T) {
		allowing := AppSettings{Download: AppDownload{Policy: "allow"}}
		resp := &http.Response{Header: http.Header{
			"Content-Type":        []string{"application/zip"},
			"Content-Disposition": []string{"attachment"},
		}}
		assert.NoError(t, applyDownloadPolicy(ref, allowing, resp))
	})
}

// A chunked response declares no length, so the only place to enforce the
// cap is as the body streams.
func TestCappedBodyTruncates(t *testing.T) {
	withTempAppData(t)
	ref := AppRef{Owner: "PO", Repo: "app"}
	body := &cappedBody{
		ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", 5000))),
		remaining:  100,
		ref:        ref,
	}
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Len(t, got, 100, "reading stops at the cap")
}

func TestStatusRecorder(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: newDiscardWriter(), status: http.StatusOK}
	rec.WriteHeader(http.StatusNotFound)
	rec.WriteHeader(http.StatusInternalServerError) // a second call must not win
	assert.Equal(t, http.StatusNotFound, rec.status)

	// A handler that writes a body without calling WriteHeader has implicitly
	// sent 200, which the metrics must record rather than treating as unset.
	plain := &statusRecorder{ResponseWriter: newDiscardWriter(), status: http.StatusOK}
	_, _ = plain.Write([]byte("hi"))
	assert.Equal(t, http.StatusOK, plain.status)
}

type discardWriter struct{ header http.Header }

func newDiscardWriter() *discardWriter               { return &discardWriter{header: http.Header{}} }
func (d *discardWriter) Header() http.Header         { return d.header }
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}

// The app is mounted at /apps/{owner}/{repo} but serves as though it owned the
// root — uvicorn's --root-path builds URLs with the prefix, it does not strip
// it from what arrives. Forwarding the mounted path unchanged made every app
// answer its own 404 for its own index page.
func TestAppRelativePathStripsMountPrefix(t *testing.T) {
	cases := map[string]string{
		"/apps/PO/Test_FastAPI":         "/",
		"/apps/PO/Test_FastAPI/":        "/",
		"/apps/PO/Test_FastAPI/health":  "/health",
		"/apps/PO/Test_FastAPI/a/b?c=d": "/a/b?c=d", // query is not in URL.Path, but nesting is
		// Case need not match the registered app: the router matched on the
		// URL as typed, and a trim that silently failed would forward the
		// whole prefix.
		"/apps/po/test_fastapi/health": "/health",
	}
	for in, want := range cases {
		assert.Equal(t, want, appRelativePath(in), in)
	}
}

// --root-path covers the URLs an app builds for itself, which is why its own
// links work. It does not cover a path written literally, and following one of
// those takes the visitor out of the app entirely — into Gitea, where they get
// a login page or a 404 for a page that exists.
func TestLocationHeaderGetsTheMountPrefixBack(t *testing.T) {
	const prefix = "/apps/PO/Test_FastAPI"
	cases := map[string]string{
		"/docs":                         prefix + "/docs",
		"/":                             prefix + "/",
		"/x?a=b":                        prefix + "/x?a=b",
		prefix + "/docs":                prefix + "/docs", // already right: --root-path's output
		prefix:                          prefix,
		"docs":                          "docs",                     // relative; resolved under the prefix by the browser
		"https://example.com/docs":      "https://example.com/docs", // somewhere else, on purpose
		"//example.com/docs":            "//example.com/docs",       // protocol-relative, so also off-host
		"/apps/PO/Test_FastAPI_Other/x": prefix + "/apps/PO/Test_FastAPI_Other/x",
	}
	for in, want := range cases {
		assert.Equal(t, want, prefixDocumentURL(prefix, in), in)
	}
}

// A host no browser can reach is the app naming itself as it was reachable
// somewhere else. 0.0.0.0 is not an address you connect to, and a loopback
// address is the viewer's own machine — so the path is the only part that can
// ever be right.
//
// This is also why the platform is not tied to a port: the result is always a
// path, so it holds whether the instance answers on 3000, 6000 or 443.
func TestUnreachableAndSameHostURLsBecomeAppPaths(t *testing.T) {
	const prefix = "/apps/PO/app"
	cases := []struct{ in, sameHost, want string }{
		{"http://app/docs", "", prefix + "/docs"},
		{"http://0.0.0.0:3000/items", "", prefix + "/items"},
		{"http://localhost:8000/items?q=1", "", prefix + "/items?q=1"},
		{"http://127.0.0.1/x#top", "", prefix + "/x#top"},
		{"http://app", "", prefix + "/"},
		// The instance's own address, whatever port or scheme it answers on.
		{"http://gitea.internal:6000/docs", "gitea.internal:6000", prefix + "/docs"},
		{"https://gitea.internal/docs", "gitea.internal:443", prefix + "/docs"},
		{"http://gitea.internal:6000" + prefix + "/docs", "gitea.internal:6000", prefix + "/docs"},
		// A different host is a deliberate link elsewhere and is left alone.
		{"https://example.com/docs", "gitea.internal:6000", "https://example.com/docs"},
		// A real host that merely starts like the placeholder.
		{"http://appstore.example/x", "", "http://appstore.example/x"},
		{"mailto:a@b.c", "", "mailto:a@b.c"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, prefixDocumentURLFor(prefix, c.sameHost, c.in), c.in)
	}
}

// An app writes href="/docs" because that is what works when it is run
// locally. Under a mount prefix it goes to Gitea instead, and a non-developer
// has no reason to know that "/docs" and "docs" differ here.
func TestHTMLRootRelativeURLsGetThePrefix(t *testing.T) {
	const prefix = "/apps/PO/app"
	got := string(rewriteHTML(prefix, "", []byte(
		`<a href="/docs">d</a><form action="/items/"><img src="/static/x.png" srcset="/a.png 1x, /b.png 2x"></form>`)))

	assert.Contains(t, got, `href="/apps/PO/app/docs"`)
	assert.Contains(t, got, `action="/apps/PO/app/items/"`)
	assert.Contains(t, got, `src="/apps/PO/app/static/x.png"`)
	// Spacing between candidates is preserved as written.
	assert.Contains(t, got, `srcset="/apps/PO/app/a.png 1x, /apps/PO/app/b.png 2x"`)
}

// Everything the rewrite must not touch. Getting one of these wrong breaks a
// page, which is worse than the broken link it was fixing.
func TestHTMLRewriteLeavesEverythingElseAlone(t *testing.T) {
	const prefix = "/apps/PO/app"
	for _, doc := range []string{
		`<a href="docs">relative, already correct</a>`,
		`<a href="./docs">relative</a>`,
		`<a href="#top">fragment</a>`,
		`<a href="https://example.com/docs">absolute</a>`,
		`<a href="//example.com/docs">protocol-relative</a>`,
		`<a href="mailto:a@b.c">scheme</a>`,
		`<p>go to /docs for the API</p>`, // prose, not a URL
		`<script>fetch("/api/items")</script>`,
		`<a href="/apps/PO/app/docs">already mounted</a>`,
	} {
		assert.Equal(t, doc, string(rewriteHTML(prefix, "", []byte(doc))), doc)
	}
}

// A value already carrying the prefix is ambiguous and is read as
// already-mounted — see prefixDocumentURL. Pinned because the alternative
// reading would double every link a framework builds.
func TestHTMLRewriteTreatsPrefixedURLsAsAlreadyMounted(t *testing.T) {
	const prefix = "/apps/PO/app"
	assert.Equal(t, prefix+"/items", prefixDocumentURL(prefix, prefix+"/items"))
	assert.Equal(t, prefix, prefixDocumentURL(prefix, prefix))

	// A different app whose name merely starts the same is not this one, and
	// its path is prefixed like any other.
	assert.Equal(t, prefix+"/apps/PO/app-two/items", prefixDocumentURL(prefix, "/apps/PO/app-two/items"))
	// Case-sensitive: repository names differing only in case are different
	// repositories.
	assert.Equal(t, prefix+"/apps/po/app/items", prefixDocumentURL(prefix, "/apps/po/app/items"))
}

// Only HTML, only uncompressed, and the length has to follow the body — a
// stale Content-Length truncates the page in the browser.
func TestHTMLRewriteScopeAndContentLength(t *testing.T) {
	const prefix = "/apps/PO/app"
	body := `<a href="/docs">d</a>`

	resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	resp.Header.Set("Content-Type", "text/html; charset=utf-8")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	rewriteHTMLBody(prefix, resp)

	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(out), prefix+"/docs")
	assert.Equal(t, strconv.Itoa(len(out)), resp.Header.Get("Content-Length"))
	assert.Equal(t, int64(len(out)), resp.ContentLength)

	// JSON is not rewritten: "/docs" in a payload is data, not a link.
	jsonResp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"u":"/docs"}`))}
	jsonResp.Header.Set("Content-Type", "application/json")
	rewriteHTMLBody(prefix, jsonResp)
	out, err = io.ReadAll(jsonResp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"u":"/docs"}`, string(out))

	// Compressed: decoding, rewriting and re-encoding trades a link for a
	// class of bugs that corrupt whole pages.
	gz := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	gz.Header.Set("Content-Type", "text/html")
	gz.Header.Set("Content-Encoding", "gzip")
	rewriteHTMLBody(prefix, gz)
	out, err = io.ReadAll(gz.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(out))
}

// Department apps are written by people who are not web developers. The
// headers that matter here protect the platform and the other departments, so
// the proxy — the one place every response passes through — supplies them.
func TestSecurityHeadersAreSuppliedByDefault(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Server", "uvicorn")
	applySecurityHeaders(AppSettings{}, resp)

	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "SAMEORIGIN", resp.Header.Get("X-Frame-Options"))
	assert.Equal(t, "same-origin", resp.Header.Get("Referrer-Policy"))
	assert.Empty(t, resp.Header.Get("Server"), "the software and its version pick the exploit to try")
}

// An app that set the header has thought about it; overriding that would break
// a page to enforce a default.
func TestAppsOwnSecurityHeaderIsKept(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-Frame-Options", "DENY")
	applySecurityHeaders(AppSettings{}, resp)
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
}

// The half that makes a security change deployable: policy wins over both the
// default and the app, reaches every app on the next request, and needs no
// department to rebuild anything.
func TestPolicyHeadersWinAndCanRemoveADefault(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-Frame-Options", "DENY")
	applySecurityHeaders(AppSettings{Security: AppSecurity{Headers: map[string]string{
		"X-Frame-Options":         "SAMEORIGIN",
		"Content-Security-Policy": "default-src 'self'",
		"Referrer-Policy":         "", // turn a default off for an app it breaks
		"Bad\r\nName":             "injected",
	}}}, resp)

	assert.Equal(t, "SAMEORIGIN", resp.Header.Get("X-Frame-Options"))
	assert.Equal(t, "default-src 'self'", resp.Header.Get("Content-Security-Policy"))
	assert.Empty(t, resp.Header.Get("Referrer-Policy"))
	// A header name carrying a newline would let one line of policy inject a
	// second header entirely.
	assert.Empty(t, resp.Header.Get("Bad"))
	assert.False(t, validHeaderName("Bad\r\nName"))
}
