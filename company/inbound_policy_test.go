// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The ceiling wins at request time: a mode committed before the ceiling was
// lowered must not stay in force just because it is still in the file.
func TestAccessCeilingClampsWiderModes(t *testing.T) {
	withCompanyINI(t, "APP_MAX_ACCESS = login")
	assert.Equal(t, AccessLogin, clampAccess(AccessPublic), "public is wider than login")
	assert.Equal(t, AccessLogin, clampAccess(""), "unset means public, and is clamped too")
	assert.Equal(t, AccessOrg, clampAccess(AccessOrg), "narrower stays as written")
	assert.True(t, accessWiderThanCeiling(AccessPublic))
	assert.False(t, accessWiderThanCeiling(AccessLogin))

	withCompanyINI(t, "APP_MAX_ACCESS = bogus")
	assert.Equal(t, AccessPublic, MaxAppAccess(), "an unreadable ceiling falls back to the open default, not to closed")
}

// Off unless said otherwise: `open` is the one mode in which an app can bind
// a port and become a server on the company network.
func TestOpenNetworkIsForbiddenByDefault(t *testing.T) {
	withCompanyINI(t, "")
	assert.False(t, NetworkOpenAllowed())
	withCompanyINI(t, "APP_NETWORK_ALLOW_OPEN = true")
	assert.True(t, NetworkOpenAllowed())
}

// The refused ones are the point: a refusal nobody can see later is
// indistinguishable from one that never happened. Newest first, and the
// blocked count covers everything on record, not just the page.
func TestRecentAccessNewestFirstWithBlockedCount(t *testing.T) {
	withTempAppData(t)
	ref := AppRef{Owner: "PO", Repo: "audit"}
	ring := accessRingFor(ref.Owner, ref.Repo)
	push := func(path, blocked string) {
		ring.mu.Lock()
		ring.recs[ring.next] = AccessRecord{At: time.Now(), IP: "10.0.0.1", Path: path, Status: 200, Blocked: blocked}
		ring.next = (ring.next + 1) % accessRingSize
		ring.mu.Unlock()
	}
	push("/first", "")
	push("/.env", "probe for /.env")
	push("/third", "")

	recs, blocked := RecentAccess(ref.Owner, ref.Repo, 2)
	assert.Equal(t, 1, blocked, "counted across the whole ring")
	if assert.Len(t, recs, 2) {
		assert.Equal(t, "/third", recs[0].Path, "newest first")
		assert.Equal(t, "/.env", recs[1].Path)
	}
	recs, _ = RecentAccess(ref.Owner, ref.Repo, 0)
	assert.Len(t, recs, 3, "zero means everything on record")
	assert.Equal(t, "10.0.0.1", recs[0].Client(), "no account means the address is the identity")
}

// Lifting a ban by hand clears the strikes with it: an administrator who
// decided this visitor is fine has decided the whole history is.
func TestUnbanClearsBanAndStrikes(t *testing.T) {
	v := &guardVisitor{bannedTo: time.Now().Add(time.Hour), strikes: 3}
	guardVisitors.Store("t:unban", v)
	defer guardVisitors.Delete("t:unban")

	assert.Len(t, ListBannedVisitors(), 1)
	assert.True(t, UnbanVisitor("t:unban", "admin"))
	assert.Empty(t, ListBannedVisitors())
	assert.Equal(t, 0, v.strikes)
	assert.False(t, UnbanVisitor("t:unban", "admin"), "already lifted")
}
