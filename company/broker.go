// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/log"
)

// The egress broker: how an app with no network of its own reaches the few
// hosts an admin has approved.
//
// A sandboxed app cannot open a connection to anywhere — that is the point of
// the sandbox — but its run/ directory is bind-mounted read-write, and a unix
// socket is a file, not a network. So the platform listens on a socket inside
// that directory and forwards approved requests on the app's behalf. This is
// the same trick that makes the app reachable at all (the proxy dials
// run/app.sock from outside), pointed the other way.
//
// What this buys over a firewall, and why it exists instead of one (no root
// here anyway): policy by method and path, not just host and port; a complete
// audit log of every outbound request, because every one passes through this
// function; and the app never holds the credentials for the systems it talks
// to — it cannot leak what it never had.
//
// The department-side convention is three lines, documented in
// docs/company/app-platform.md:
//
//	import httpx, os
//	web = httpx.Client(transport=httpx.HTTPTransport(uds=os.environ["BROKER_SOCKET"]))
//	r = web.get("http://erp.internal.company.com/api/v1/employees")
//
// The URL names the real host and stays http:// — inside the socket there is
// no network to protect, and an https URL would make the client attempt TLS
// against the broker itself. The broker reads the Host header, checks the
// allowlist, and makes the real request over https.

// brokerSocketName lives beside app.sock in the one directory the sandbox
// can write.
const brokerSocketName = "broker.sock"

// brokerRequestTimeout bounds one outbound call. Generous, because reports
// against slow internal systems are the normal case here — but bounded,
// because an app must not be able to hold broker goroutines open forever.
const brokerRequestTimeout = 60 * time.Second

// appBroker is one app's listener.
type appBroker struct {
	owner, repo string
	server      *http.Server
	listener    net.Listener
}

var (
	brokersMu sync.Mutex
	brokers   = map[string]*appBroker{} // appKey
)

// startBroker begins listening for one app, replacing any previous listener.
// Called from the app start path when its network mode is broker; a failure
// is returned rather than logged because an app approved for outbound access
// whose broker silently failed to start would see every call refused with no
// hint that the platform, not the policy, is what broke.
func startBroker(owner, repo string, p appPaths) error {
	stopBroker(owner, repo)

	socket := filepath.Join(p.run, brokerSocketName)
	_ = os.Remove(socket) // ours alone; the app only ever dials it
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("the outbound broker could not listen: %w", err)
	}
	// The app runs as the same account, but the socket should still not be
	// wider than it has to be.
	_ = os.Chmod(socket, 0o600)

	b := &appBroker{owner: owner, repo: repo}
	b.server = &http.Server{
		Handler:           http.HandlerFunc(b.serve),
		ReadHeaderTimeout: 10 * time.Second,
	}
	b.listener = listener

	brokersMu.Lock()
	brokers[appKey(owner, repo)] = b
	brokersMu.Unlock()

	go func() {
		if err := b.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Error("company: broker for %s/%s: %v", owner, repo, err)
		}
	}()
	return nil
}

// stopBroker shuts one app's listener down with its app.
func stopBroker(owner, repo string) {
	brokersMu.Lock()
	b := brokers[appKey(owner, repo)]
	delete(brokers, appKey(owner, repo))
	brokersMu.Unlock()
	if b != nil {
		_ = b.server.Close()
	}
}

// hopByHopHeaders never cross a proxy; RFC 9110 §7.6.1.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// serve handles one outbound request from the app.
func (b *appBroker) serve(w http.ResponseWriter, r *http.Request) {
	host := hostWithoutPort(r.Host)
	// Policy is read per request, not captured at listener start, so an
	// admin's change applies to the next call rather than the next restart.
	if !outboundRuleFor(SettingsFor(b.owner, b.repo), host, r.Method, r.URL.Path) {
		b.audit(r, host, http.StatusForbidden, 0, "refused by policy")
		http.Error(w, "the platform's outbound policy does not allow "+r.Method+" "+host+r.URL.Path+
			"; an administrator can approve it on the app's admin page", http.StatusForbidden)
		return
	}

	// Always https out. The traffic crosses a real network from here; an app
	// that genuinely needs plaintext http to an internal legacy box is a
	// decision an admin should have to make knowingly, not a default.
	outURL := "https://" + host + r.URL.RequestURI()
	out, err := http.NewRequestWithContext(r.Context(), r.Method, outURL, r.Body)
	if err != nil {
		b.audit(r, host, http.StatusBadGateway, 0, "bad request: "+err.Error())
		http.Error(w, "the broker could not build that request", http.StatusBadGateway)
		return
	}
	copyBrokerHeaders(out.Header, r.Header)

	client := &http.Client{Timeout: brokerRequestTimeout}
	resp, err := client.Do(out)
	if err != nil {
		b.audit(r, host, http.StatusBadGateway, 0, err.Error())
		http.Error(w, "the destination did not respond: "+host, http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	header := w.Header()
	for name, values := range resp.Header {
		for _, v := range values {
			header.Add(name, v)
		}
	}
	for _, h := range hopByHopHeaders {
		header.Del(h)
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	b.audit(r, host, resp.StatusCode, n, "")
}

// copyBrokerHeaders forwards the app's headers minus what must not cross.
func copyBrokerHeaders(dst, src http.Header) {
	for name, values := range src {
		if strings.EqualFold(name, "Host") {
			continue
		}
		skip := false
		for _, h := range hopByHopHeaders {
			if strings.EqualFold(name, h) {
				skip = true
				break
			}
		}
		if !skip {
			dst[name] = values
		}
	}
}

// outboundRuleFor reports whether policy allows this call.
//
// Everything is matched, not just the host: "may read the employee list" and
// "may POST to the payroll endpoint" are different approvals, and the
// per-method, per-path grain is the thing a firewall cannot do and this can.
func outboundRuleFor(settings AppSettings, host, method, path string) bool {
	if settings.Network.Mode == NetworkOpen {
		return true
	}
	if settings.Network.Mode != NetworkBroker {
		return false
	}
	for _, rule := range settings.Network.Allow {
		if !strings.EqualFold(rule.Host, host) {
			continue
		}
		if len(rule.Methods) > 0 && !containsFold(rule.Methods, method) {
			continue
		}
		if len(rule.Paths) > 0 && !pathAllowed(rule.Paths, path) {
			continue
		}
		return true
	}
	return false
}

func containsFold(list []string, want string) bool {
	for _, item := range list {
		if strings.EqualFold(item, want) {
			return true
		}
	}
	return false
}

// pathAllowed matches a path against the rule's patterns. A trailing "*"
// makes a prefix; anything else must match exactly. Matching is by segment
// boundary so "/api/v1*" does not quietly cover "/api/v1000".
func pathAllowed(patterns []string, path string) bool {
	for _, pattern := range patterns {
		if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
			if strings.HasPrefix(path, prefix) {
				return true
			}
			continue
		}
		if path == pattern {
			return true
		}
	}
	return false
}

// audit records one outbound call, allowed or not.
//
// Every call, because "did data leave, and where to" is the question this
// whole mode exists to answer after the fact — an audit trail with holes in
// it answers nothing. Written to the app's own log directory so it rotates
// with everything else and is read on the same admin screen.
func (b *appBroker) audit(r *http.Request, host string, status int, bytes int64, note string) {
	p := appPathsFor(b.owner, b.repo)
	f, err := openRotatingLog(p.logs, brokerLogName)
	if err != nil {
		log.Error("company: broker audit for %s/%s: %v", b.owner, b.repo, err)
		return
	}
	defer func() { _ = f.Close() }()
	line := fmt.Sprintf("%s %s %s%s -> %d (%d bytes)", time.Now().Format(logTimeLayout),
		r.Method, host, r.URL.RequestURI(), status, bytes)
	if note != "" {
		line += " " + note
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		log.Error("company: broker audit for %s/%s: %v", b.owner, b.repo, err)
	}
}

// brokerLogName sits beside app.log and build.log.
const brokerLogName = "broker.log"
