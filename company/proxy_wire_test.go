// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Drives the real proxy — the same appProxyFor the request handler uses —
// against a stub app on a unix socket. Everything between the browser and the
// app is exercised: the path the app is asked for, and the headers that come
// back.
func serveStubApp(t *testing.T, owner, repo string, handler http.HandlerFunc) {
	t.Helper()
	socket := AppSocketPath(owner, repo)
	// sun_path is 104 bytes on macOS and the temp dir a test gets is long
	// enough on its own to overrun it.
	require.Less(t, len(socket), maxUnixSocketPath, "socket path: %s", socket)
	require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0o700))

	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := &http.Server{Handler: handler} //nolint:gosec // no timeouts needed for a test stub
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		proxyCache.Delete(appKey(owner, repo))
	})
}

func TestProxyDeliversTheAppsOwnPathAndKeepsRedirectsInside(t *testing.T) {
	withTempAppData(t)
	const owner, repo = "PO", "Test_FastAPI"
	prefix := appProxyPrefix + "/" + owner + "/" + repo

	var gotPath, gotHost string
	serveStubApp(t, owner, repo, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHost = r.URL.Path, r.Host
		// What Starlette does for a trailing slash: an absolute redirect built
		// from the Host header it was given.
		w.Header().Set("Location", "http://"+r.Host+prefix+"/docs")
		w.Header().Add("Set-Cookie", "session=abc; Path=/")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})

	req := httptest.NewRequest(http.MethodGet, "http://localhost:3000"+prefix+"/docs/", nil)
	rec := httptest.NewRecorder()
	appProxyFor(AppRef{Owner: owner, Repo: repo}).ServeHTTP(rec, req)

	// The app is mounted at the prefix but serves as though it owned the root.
	assert.Equal(t, "/docs/", gotPath, "the mount prefix must be stripped on the way in")
	assert.Equal(t, "localhost:3000", gotHost, "the app must see the real host, not the internal placeholder")

	// And the redirect it produced has to stay inside the app. Reduced to a
	// path: it named this same instance, and a path holds whichever host, port
	// or scheme the visitor reached it by.
	assert.Equal(t, prefix+"/docs", rec.Header().Get("Location"))
	assert.Equal(t, []string{"session=abc; Path=" + prefix + "/"}, rec.Header().Values("Set-Cookie"))
}

// The case a department's own code produces, which --root-path does not cover.
func TestProxyPutsThePrefixBackOnALiteralRedirect(t *testing.T) {
	withTempAppData(t)
	const owner, repo = "PO", "app"
	prefix := appProxyPrefix + "/" + owner + "/" + repo

	serveStubApp(t, owner, repo, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/docs") // RedirectResponse("/docs")
		w.WriteHeader(http.StatusFound)
	})

	req := httptest.NewRequest(http.MethodGet, "http://localhost:3000"+prefix, nil)
	rec := httptest.NewRecorder()
	appProxyFor(AppRef{Owner: owner, Repo: repo}).ServeHTTP(rec, req)

	assert.Equal(t, prefix+"/docs", rec.Header().Get("Location"),
		"a literal path would send the visitor out of the app and into Gitea")
}
