// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDetectRuntime(t *testing.T) {
	detect := func(files ...string) (string, string, bool) {
		return detectRuntime(func(rel string) bool { return slices.Contains(files, rel) })
	}
	rt, root, ok := detect("main.py", "index.html")
	assert.Equal(t, []any{RuntimePython, "", true}, []any{rt, root, ok}, "main.py wins")

	// A built Vite project: the root index.html only works before the build.
	rt, root, ok = detect("index.html", "dist/index.html", "package.json")
	assert.Equal(t, []any{RuntimeStatic, "dist", true}, []any{rt, root, ok})

	rt, root, ok = detect("index.html", "style.css")
	assert.Equal(t, []any{RuntimeStatic, "", true}, []any{rt, root, ok})

	_, _, ok = detect("README.md")
	assert.False(t, ok)
}

func TestStaticSiteHandler(t *testing.T) {
	h := staticSiteHandler(fstest.MapFS{
		"index.html":      {Data: []byte("home")},
		"style.css":       {Data: []byte("body{}")},
		"docs/index.html": {Data: []byte("docs")},
		".env":            {Data: []byte("SECRET=1")},
	})
	get := func(method, target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
		return w
	}
	cases := []struct {
		target string
		status int
		body   string
	}{
		{"/", http.StatusOK, "home"},
		{"/style.css", http.StatusOK, "body{}"},
		{"/docs/", http.StatusOK, "docs"},
		{"/docs", http.StatusOK, "docs"},
		{"/orders/42", http.StatusOK, "home"}, // a single-page app's route
		{"/missing.js", http.StatusNotFound, ""},
		{"/.env", http.StatusNotFound, ""},
		{"/../../etc/passwd", http.StatusOK, "home"}, // cleaned to a path inside the site
	}
	for _, c := range cases {
		w := get(http.MethodGet, c.target)
		assert.Equal(t, c.status, w.Code, c.target)
		if c.body != "" {
			assert.Equal(t, c.body, w.Body.String(), c.target)
		}
	}
	assert.Equal(t, "text/css; charset=utf-8", get(http.MethodGet, "/style.css").Header().Get("Content-Type"))
	assert.Equal(t, http.StatusMethodNotAllowed, get(http.MethodPost, "/").Code)
}

func TestProbeEveryRechecks(t *testing.T) {
	calls := 0
	probe := probeEvery(50*time.Millisecond, func() (int, error) { calls++; return calls, nil })
	first, _ := probe()
	again, _ := probe()
	assert.Equal(t, first, again, "cached within the window")
	time.Sleep(60 * time.Millisecond)
	later, _ := probe()
	assert.Equal(t, 2, later, "a tool installed since is seen")
}
