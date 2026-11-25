// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSMTP answers sessions, refusing any recipient named "nobody@" and a
// sign-in by any of the refuse mechanisms.
func fakeSMTP(t *testing.T, auth string, refuse ...string) (string, *strings.Builder) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	got := &strings.Builder{}
	serve := func(conn net.Conn) {
		defer conn.Close()
		r := bufio.NewReader(conn)
		reply := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		reply("220 fake ready")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					reply("250 queued")
				} else {
					got.WriteString(line + "\n")
				}
				continue
			}
			switch cmd := strings.ToUpper(strings.Fields(line + " x")[0]); {
			case cmd == "EHLO":
				reply("250-fake")
				reply("250 AUTH " + auth)
			case cmd == "AUTH" && slices.Contains(refuse, strings.ToUpper(strings.Fields(line)[1])):
				reply("535 5.7.8 unauthorized")
			case cmd == "AUTH":
				reply("235 ok")
			case cmd == "MAIL":
				reply("250 ok")
			case cmd == "RCPT" && strings.Contains(line, "nobody@"):
				reply("550 no such user")
			case cmd == "RCPT":
				reply("250 ok")
			case cmd == "DATA":
				inData = true
				reply("354 go")
			case cmd == "QUIT":
				reply("221 bye")
				return
			default:
				reply("250 ok")
			}
		}
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			serve(conn)
		}
	}()
	return ln.Addr().String(), got
}

func TestSendSMTPTracesEachStep(t *testing.T) {
	server := MailServer{Host: "127.0.0.1", Username: "bot@corp.example", From: "bot@corp.example", Security: mailSecurityNone}
	addr, got := fakeSMTP(t, "PLAIN LOGIN")
	server.Port = portOf(t, addr)

	var steps []string
	trace := func(format string, _ ...any) { steps = append(steps, format) }
	err := sendSMTP(t.Context(), server, "pw", []string{"a@corp.example"}, buildMessage(server, []string{"a@corp.example"}, "테스트", "<p>hi</p>"), trace)
	require.NoError(t, err)
	assert.Equal(t, []string{"connected to %s (security: %s)", "server offers AUTH: %s", "AUTH %s as %s ok", "MAIL FROM %s ok", "RCPT TO %s ok", "message accepted (%d bytes)"}, steps)
	assert.Contains(t, got.String(), "<p>hi</p>")

	addr, _ = fakeSMTP(t, "PLAIN")
	server.Port = portOf(t, addr)
	err = sendSMTP(t.Context(), server, "pw", []string{"nobody@corp.example"}, []byte("x"), nil)
	assert.ErrorContains(t, err, "refused nobody@corp.example")
}

// A server that offers PLAIN and then refuses it ("535 unauthorized") is
// signed in to with the next method it offers, on a new connection.
func TestSendSMTPTriesEachOfferedSignIn(t *testing.T) {
	server := MailServer{Host: "127.0.0.1", Username: "bot@corp.example", From: "bot@corp.example", Security: mailSecurityNone}
	addr, got := fakeSMTP(t, "PLAIN LOGIN", "PLAIN")
	server.Port = portOf(t, addr)
	var steps []string
	trace := func(format string, args ...any) { steps = append(steps, fmt.Sprintf(format, args...)) }
	err := sendSMTP(t.Context(), server, "pw", []string{"a@corp.example"}, buildMessage(server, []string{"a@corp.example"}, "s", "<p>sent</p>"), trace)
	require.NoError(t, err)
	assert.Contains(t, steps, "AUTH LOGIN as bot@corp.example ok")
	assert.Contains(t, strings.Join(steps, "\n"), "AUTH PLAIN as bot@corp.example refused")
	assert.Contains(t, got.String(), "<p>sent</p>")

	addr, _ = fakeSMTP(t, "PLAIN LOGIN", "PLAIN", "LOGIN")
	server.Port = portOf(t, addr)
	err = sendSMTP(t.Context(), server, "pw", []string{"a@corp.example"}, []byte("x"), nil)
	assert.ErrorContains(t, err, "tried PLAIN, LOGIN")
}

func portOf(t *testing.T, addr string) int {
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	n, err := strconv.Atoi(port)
	require.NoError(t, err)
	return n
}
