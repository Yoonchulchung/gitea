// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"io"
	"net/http"
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
