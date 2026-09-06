// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"
	"sync"
)

// The proxy sits on every request to every deployed app, so answering "is
// this a real app?" must not read a file or hit the database — see
// docs/company/app-platform-impl.md §3. This registry is that answer, kept
// in memory and updated whenever app state changes.
//
// It is also a security boundary, not just a cache: without it the URL
// "/apps/../../something" or any invented owner/repo would be turned into a
// filesystem path and dialled as a unix socket. Only apps that got here by
// actually being deployed are reachable.

// AppRef is the canonical spelling of one app's owner and repository.
type AppRef struct{ Owner, Repo string }

// Keyed case-insensitively because Gitea resolves owner and repository names
// that way, so /apps/po/myapp and /apps/PO/myapp are the same app to a user
// and must be the same app here.
var appRegistry sync.Map // lowercased "owner/repo" -> AppRef

func appRegistryKey(owner, repo string) string {
	return strings.ToLower(owner + "/" + repo)
}

// RegisterApp makes an app reachable through the proxy. Called from every
// path that writes app state, so an app becomes routable at the same moment
// it becomes real.
func RegisterApp(owner, repo string) {
	if owner == "" || repo == "" {
		return
	}
	appRegistry.Store(appRegistryKey(owner, repo), AppRef{Owner: owner, Repo: repo})
}

// UnregisterApp removes an app, for when an admin deletes it. A request for
// it then 404s rather than dialling a socket that no longer has an owner.
func UnregisterApp(owner, repo string) {
	appRegistry.Delete(appRegistryKey(owner, repo))
}

// LookupApp resolves a URL's owner/repo to the canonical spelling, or
// reports that no such app exists.
func LookupApp(owner, repo string) (AppRef, bool) {
	v, ok := appRegistry.Load(appRegistryKey(owner, repo))
	if !ok {
		return AppRef{}, false
	}
	ref, ok := v.(AppRef)
	return ref, ok
}

// loadAppRegistry seeds the registry from state files at startup. Every app
// that has ever been deployed has one, so this covers stopped apps too —
// they must resolve, so the proxy can say "this app is stopped" rather than
// "no such app".
func loadAppRegistry() {
	for _, st := range ListAppStates() {
		RegisterApp(st.Owner, st.Repo)
	}
}
