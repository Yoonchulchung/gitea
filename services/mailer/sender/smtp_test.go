// Copyright 2025 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package sender

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"

	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeEmailAddress(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		hasError bool
	}{
		{"abc@gitea.com", "abc@gitea.com", false},
		{"<abc@gitea.com>", "abc@gitea.com", false},
		{"ssss.com", "", true},
		{"<invalid-email>", "", true},
	}

	for _, tt := range tests {
		result, err := sanitizeEmailAddress(tt.input)
		if (err != nil) != tt.hasError {
			t.Errorf("sanitizeEmailAddress(%q) unexpected error status: got %v, want error: %v", tt.input, err != nil, tt.hasError)
			continue
		}
		if result != tt.expected {
			t.Errorf("sanitizeEmailAddress(%q) = %q; want %q", tt.input, result, tt.expected)
		}
	}
}

func TestSMTPSenderAuthFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	var mu sync.Mutex
	var received []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(conn)
			reply := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
			reply("220 localhost ESMTP")
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					break
				}
				line = strings.TrimSpace(line)
				mu.Lock()
				received = append(received, line)
				mu.Unlock()
				switch {
				case strings.HasPrefix(line, "EHLO"):
					reply("250-localhost\r\n250 AUTH CRAM-MD5 PLAIN LOGIN")
				case line == "AUTH CRAM-MD5":
					reply("504 5.7.4 Unrecognized authentication type")
				case strings.HasPrefix(line, "AUTH PLAIN"):
					reply("235 2.7.0 Authentication successful")
				case line == "DATA":
					reply("354 Go ahead")
				case line == ".":
					reply("250 OK")
				case line == "QUIT":
					reply("221 Bye")
				default:
					reply("250 OK")
				}
			}
			conn.Close()
		}
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	defer test.MockVariableValue(&setting.MailService, &setting.Mailer{
		Protocol: "smtp",
		SMTPAddr: host,
		SMTPPort: port,
		User:     "user",
		Passwd:   "pass",
	})()

	err = (&SMTPSender{}).Send("from@gitea.com", []string{"to@gitea.com"}, strings.NewReader("Subject: test\r\n\r\nbody\r\n"))
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, received, "AUTH CRAM-MD5")
	assert.Contains(t, received, "MAIL FROM:<from@gitea.com>")
}
