// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An app with its own health endpoint is judged by it; one without is judged
// by answering at all, so no department has to write one.
func TestProbeHealth(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		wantOwn  bool
		wantDown bool
	}{
		{"own endpoint, healthy", http.StatusOK, true, false},
		{"own endpoint, reporting a problem", http.StatusServiceUnavailable, true, true},
		{"no endpoint", http.StatusNotFound, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/health", r.URL.Path)
				assert.Equal(t, "1", r.URL.Query().Get("platform-health-check"), "marked, so the log can leave it out")
				w.WriteHeader(c.status)
			}))
			defer srv.Close()
			client := &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
				},
			}}

			own, err := probeHealth(t.Context(), client, "/health")
			assert.Equal(t, c.wantOwn, own)
			assert.Equal(t, c.wantDown, err != nil)
		})
	}
}

// Two probes a minute would bury everything the app printed, so a passing one
// is left out — unless someone searches for it. A failing one is the story.
func TestLogsLeaveOutPassingHealthProbes(t *testing.T) {
	withTempAppData(t)
	p := appPathsFor("PO", "app")
	require.NoError(t, os.MkdirAll(p.logs, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(p.logs, appLogName), []byte(
		`INFO:      - "GET /apps/PO/app/health?platform-health-check=1 HTTP/1.1" 404 Not Found`+"\n"+
			`INFO:      - "GET /apps/PO/app/ HTTP/1.1" 200 OK`+"\n"+
			`INFO:      - "GET /apps/PO/app/health?platform-health-check=1 HTTP/1.1" 503 Service Unavailable`+"\n",
	), 0o600))

	lines, _, err := ReadAppLogs("PO", "app", LogQuery{})
	require.NoError(t, err)
	assert.Equal(t, []string{
		`INFO:      - "GET /apps/PO/app/ HTTP/1.1" 200 OK`,
		`INFO:      - "GET /apps/PO/app/health?platform-health-check=1 HTTP/1.1" 503 Service Unavailable`,
	}, texts(lines))

	lines, _, err = ReadAppLogs("PO", "app", LogQuery{Text: "health"})
	require.NoError(t, err)
	assert.Len(t, lines, 2)
}

// A restart that does not fix it, three times over, is not going to: the app
// is left down with a reason rather than cycled forever.
func TestLivenessStopsRestartingAStuckApp(t *testing.T) {
	var rec livenessRecord
	start := time.Now()
	for i := range livenessRestartLimit {
		assert.True(t, rec.allowRestart(start.Add(time.Duration(i)*time.Minute)))
	}
	assert.False(t, rec.allowRestart(start.Add(4*time.Minute)))

	assert.True(t, rec.allowRestart(start.Add(time.Hour)), "the count starts over once it has been given up on")

	var spread livenessRecord
	for range livenessRestartLimit {
		require.True(t, spread.allowRestart(start))
	}
	assert.True(t, spread.allowRestart(start.Add(livenessRestartWindow+time.Minute)),
		"restarts older than the window no longer count")
}

// Most apps failing together is the host, and restarting them all would add
// load at the worst moment and then give up on every one of them.
func TestLivenessHostTrouble(t *testing.T) {
	assert.True(t, livenessHostTrouble(3, 4))
	assert.False(t, livenessHostTrouble(3, 10), "a few stuck apps among many are the apps")
	assert.False(t, livenessHostTrouble(1, 2), "with two apps one stuck app is already half")
}
