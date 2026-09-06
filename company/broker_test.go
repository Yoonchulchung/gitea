// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The grain is the point: "may read the employee list" and "may POST to the
// payroll endpoint" are different approvals, and matching only the host would
// collapse them back into one.
func TestOutboundRuleMatchesHostMethodAndPath(t *testing.T) {
	settings := AppSettings{Network: AppNetwork{Mode: NetworkBroker, Allow: []AppNetworkRule{
		{Host: "erp.internal", Methods: []string{"GET"}, Paths: []string{"/api/v1/*"}},
		{Host: "files.internal"}, // no methods/paths = any
	}}}

	allowed := func(host, method, path string) bool {
		return outboundRuleFor(settings, host, method, path)
	}
	assert.True(t, allowed("erp.internal", "GET", "/api/v1/employees"))
	assert.True(t, allowed("ERP.Internal", "get", "/api/v1/employees"), "host and method are case-insensitive")
	assert.False(t, allowed("erp.internal", "POST", "/api/v1/employees"), "writing is a separate approval")
	assert.False(t, allowed("erp.internal", "GET", "/admin"), "outside the approved paths")
	assert.False(t, allowed("other.internal", "GET", "/api/v1/x"), "unapproved host")
	assert.True(t, allowed("files.internal", "DELETE", "/anything"), "a bare rule covers the host")

	// none blocks everything; open allows everything — the explicit exception.
	assert.False(t, outboundRuleFor(AppSettings{Network: AppNetwork{Mode: NetworkNone}}, "erp.internal", "GET", "/"))
	assert.True(t, outboundRuleFor(AppSettings{Network: AppNetwork{Mode: NetworkOpen}}, "anywhere.example", "POST", "/"))
}

// "/api/v1*" must not quietly cover "/api/v1000" — a prefix that ignores the
// segment boundary approves paths nobody looked at.
func TestBrokerPathPatterns(t *testing.T) {
	assert.True(t, pathAllowed([]string{"/api/v1/*"}, "/api/v1/employees"))
	assert.True(t, pathAllowed([]string{"/health"}, "/health"))
	assert.False(t, pathAllowed([]string{"/health"}, "/healthz"), "exact means exact")
	assert.True(t, pathAllowed([]string{"/api/v1*"}, "/api/v1000"),
		"a pattern written without the slash is taken at its word — the form writes /api/v1/*")
}

// The whole loop, over a real unix socket: the app asks the broker, policy
// decides, and the audit log records both outcomes.
func TestBrokerRefusesAndAudits(t *testing.T) {
	withTempAppData(t)
	const owner, repo = "PO", "app"
	p := appPathsFor(owner, repo)
	require.NoError(t, os.MkdirAll(p.run, 0o700))
	require.NoError(t, os.MkdirAll(p.logs, 0o700))

	require.NoError(t, startBroker(owner, repo, p))
	t.Cleanup(func() { stopBroker(owner, repo) })

	client := &http.Client{Transport: &http.Transport{
		DialContext: unixDialer(p.run + "/" + brokerSocketName),
	}}

	// Policy for this app is NetworkNone (nothing configured), so the call is
	// refused — and the refusal names the policy, not a mystery.
	resp, err := client.Get("http://erp.internal/api/v1/employees")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "outbound policy")

	// Refusals are audited too: an exfiltration *attempt* is exactly the
	// signal the log exists to keep.
	audit, err := os.ReadFile(p.logs + "/" + brokerLogName)
	require.NoError(t, err)
	assert.Contains(t, string(audit), "erp.internal/api/v1/employees -> 403")
	assert.Contains(t, string(audit), "refused by policy")

	// Restarting replaces the listener rather than failing on the leftover
	// socket file.
	require.NoError(t, startBroker(owner, repo, p))
	resp2, err := client.Get("http://erp.internal/x")
	require.NoError(t, err)
	_ = resp2.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp2.StatusCode)
}

// The app's Host header names the destination; hop-by-hop headers stay home.
func TestBrokerHeaderCopyStripsWhatMustNotCross(t *testing.T) {
	src := http.Header{
		"Authorization": {"Bearer app-token"},
		"Connection":    {"keep-alive"},
		"Host":          {"erp.internal"},
		"Accept":        {"application/json"},
	}
	dst := http.Header{}
	copyBrokerHeaders(dst, src)
	assert.Equal(t, "Bearer app-token", dst.Get("Authorization"))
	assert.Equal(t, "application/json", dst.Get("Accept"))
	assert.Empty(t, dst.Get("Connection"))
	assert.Empty(t, dst.Get("Host"))
}

func unixDialer(socket string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	}
}
