// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import "strings"

// Where packages come from.
//
// Every deploy runs pip, and pip's default index is pypi.org — a public site
// on the open internet. On a network where unsanctioned outside traffic is an
// incident, that default is the incident: one deploy, one silent connection.
// So the index is a setting, and while it is blank the platform refuses to
// deploy rather than fall back to the public one. Refusing is loud and
// recoverable; a connection nobody approved is neither.

// pipIndexURL is the configured index, or "" when none is.
func pipIndexURL() string { return strings.TrimSpace(companySetting("PIP_INDEX_URL")) }

// pipPublicIndexAllowed is the explicit opt-in to pypi.org.
func pipPublicIndexAllowed() bool { return companySetting("PIP_ALLOW_PUBLIC_INDEX") == "true" }

// pipEgressAllowed reports whether a pip run may proceed at all: either an
// internal index is named, or reaching the public one was explicitly
// permitted. Checked before pip is started, because by the time pip reports
// a network error the request has already left.
func pipEgressAllowed() bool { return pipIndexURL() != "" || pipPublicIndexAllowed() }

// errPipNoIndex is the refusal: written for the administrator, since the
// department cannot fix it and should not be sent to try.
var errPipNoIndex = audienceKeyError("company.err.pip_no_index", "company.err.pip_no_index.admin")

// pipIndexEnv is what every pip invocation adds to its environment. pip
// honours PIP_INDEX_URL natively, so the override needs no flag threading —
// and applying it through the environment means the two pip call sites
// cannot drift apart on which index they use.
func pipIndexEnv() []string {
	if url := pipIndexURL(); url != "" {
		return []string{"PIP_INDEX_URL=" + url}
	}
	return nil
}
