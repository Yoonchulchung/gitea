// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"strings"
)

// Department apps are written by people who are not web developers, with an
// assistant's help. They will not set security headers, and asking them to is
// asking the wrong people: the headers that matter here protect the platform
// and the other departments, not the app that omitted them.
//
// Every response passes through this proxy — the app has no network of its
// own and no port of its own — so this is the one place a control can be
// applied to every app at once, including apps deployed before the control
// existed. That property is the point: when a header has to change, it
// changes here and in apps.yml, and no department redeploys anything.
//
// Applied only where the app said nothing. An app that sets its own
// Content-Security-Policy has thought about it, and overriding that would
// break a page to enforce a default. An admin who needs the platform to win
// anyway sets the value in apps.yml, which is authoritative.

// defaultSecurityHeaders are applied to every app response unless the app set
// the header itself or an admin overrode it.
//
// Chosen for being safe on an app that knows nothing about them. A
// Content-Security-Policy is deliberately absent from this list: a useful one
// is specific to what a page loads, and a default strict enough to be worth
// having breaks FastAPI's own /docs, which pulls Swagger UI from a CDN. It is
// available per app in apps.yml for those that want it.
var defaultSecurityHeaders = map[string]string{
	// The department's app decides its own Content-Type, and a browser
	// guessing otherwise is how an uploaded file becomes a script.
	"X-Content-Type-Options": "nosniff",
	// These are internal tools. Nothing outside should be framing one, and a
	// framed internal app is a click away from acting as the person viewing
	// it.
	"X-Frame-Options": "SAMEORIGIN",
	// Internal URLs describe internal structure — a department name, an app
	// name, an object id. Not something to hand to whatever a link points at.
	"Referrer-Policy": "same-origin",
	// An app that never asked for the camera should not be able to, and one
	// that does can say so in apps.yml.
	"Permissions-Policy": "camera=(), microphone=(), geolocation=(), payment=()",
}

// strippedResponseHeaders name the software and its version to anyone who
// asks. Removing them does not stop an attacker who tries the exploit anyway,
// but it stops the scan that decides which exploit to try.
var strippedResponseHeaders = []string{"Server", "X-Powered-By", "X-AspNet-Version"}

// applySecurityHeaders sets the platform's headers on one response.
func applySecurityHeaders(settings AppSettings, resp *http.Response) {
	for _, name := range strippedResponseHeaders {
		resp.Header.Del(name)
	}

	for name, value := range defaultSecurityHeaders {
		if resp.Header.Get(name) == "" {
			resp.Header.Set(name, value)
		}
	}

	// The admin's values are policy and win outright, including over a header
	// the app set. This is the half that makes a security change deployable:
	// a header added here reaches every app on the next request, with no
	// department involved and nothing rebuilt.
	//
	// An empty value removes a header rather than setting a blank one, so a
	// default can be turned off for an app it breaks.
	for name, value := range settings.Security.Headers {
		if !validHeaderName(name) {
			continue
		}
		if value == "" {
			resp.Header.Del(name)
			continue
		}
		resp.Header.Set(name, value)
	}
}

// validHeaderName rejects anything that is not a plain token.
//
// The value comes from apps.yml, which only an admin can write — but a header
// name carrying a newline would let one line of policy inject another header
// entirely, and that is worth refusing at the point of use rather than
// trusting the file to be well-formed.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_", r):
		default:
			return false
		}
	}
	return true
}
