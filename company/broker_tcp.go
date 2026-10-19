// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Raw TCP through the broker, for what is not HTTP: LDAP against the
// company's directory, a database. The app cannot dial out at all, so it
// sends CONNECT host:port down the broker socket and gets a byte pipe back;
// the platform's shim does that for every Python socket, so an LDAP library
// works unchanged (company/appshim/_company_platform.py).

// outboundTCP is the pseudo-method a rule carries when it allows a raw
// connection to its host and port.
const outboundTCP = "TCP"

// brokerTCPEnv tells the shim to tunnel the app's TCP connections.
const brokerTCPEnv = "BROKER_TCP"

const tunnelDialTimeout = 10 * time.Second

func (b *appBroker) tunnel(w http.ResponseWriter, r *http.Request) {
	host, portText, err := net.SplitHostPort(r.Host)
	port, convErr := strconv.Atoi(portText)
	if err != nil || convErr != nil || port < 1 || port > 65535 {
		b.audit(r.Method, r.Host, http.StatusBadRequest, 0, "no port to connect to")
		http.Error(w, "CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	target := net.JoinHostPort(host, portText)
	if entry := blockedBy(Blocklist(), host, port); entry != "" {
		b.audit(r.Method, target, http.StatusForbidden, 0, "blocked for every app by the platform list ("+entry+")")
		http.Error(w, "the platform blocks "+target+" for every app ("+entry+"); it cannot be approved per app", http.StatusForbidden)
		return
	}
	if !outboundRuleFor(SettingsFor(b.owner, b.repo), host, port, r.Method, "") {
		b.audit(r.Method, target, http.StatusForbidden, 0, "refused by policy")
		http.Error(w, "the platform's outbound policy does not allow a TCP connection to "+target+
			"; an administrator can approve it on the app's admin page", http.StatusForbidden)
		return
	}

	remote, err := net.DialTimeout("tcp", target, tunnelDialTimeout)
	if err != nil {
		b.audit(r.Method, target, http.StatusBadGateway, 0, err.Error())
		http.Error(w, "the destination did not answer: "+target, http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = remote.Close()
		http.Error(w, "the broker cannot open a tunnel here", http.StatusInternalServerError)
		return
	}
	local, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = remote.Close()
		return
	}
	if !b.track(local, remote) {
		return
	}
	defer b.untrack(local, remote)
	if _, err := local.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	var sent int64
	var wg sync.WaitGroup
	wg.Go(func() {
		sent, _ = io.Copy(remote, buffered.Reader) // the reader may already hold bytes sent behind the request
		closeWrite(remote)
	})
	received, _ := io.Copy(local, remote)
	closeWrite(local)
	wg.Wait()
	b.audit(r.Method, target, http.StatusOK, sent+received, "tunnel closed")
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// track registers both ends so stopBroker can cut them; false when the broker
// has already stopped.
func (b *appBroker) track(conns ...net.Conn) bool {
	b.tunnelsMu.Lock()
	defer b.tunnelsMu.Unlock()
	if b.closed {
		for _, c := range conns {
			_ = c.Close()
		}
		return false
	}
	for _, c := range conns {
		b.tunnels[c] = struct{}{}
	}
	return true
}

func (b *appBroker) untrack(conns ...net.Conn) {
	b.tunnelsMu.Lock()
	defer b.tunnelsMu.Unlock()
	for _, c := range conns {
		_ = c.Close()
		delete(b.tunnels, c)
	}
}

// closeTunnels ends every open tunnel: a stopped app keeps no connection the
// platform opened for it.
func (b *appBroker) closeTunnels() {
	b.tunnelsMu.Lock()
	defer b.tunnelsMu.Unlock()
	b.closed = true
	for c := range b.tunnels {
		_ = c.Close()
	}
	clear(b.tunnels)
}
