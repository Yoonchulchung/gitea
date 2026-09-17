// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// A scanner asks for these before it knows what it is talking to. None is a
// page a department app serves, so matching them costs nothing legitimate
// and the app never has to answer a probe.
func TestProbePathsMatchScannerTargetsOnly(t *testing.T) {
	for _, p := range []string{"/.env", "/.git/config", "/wp-admin/x", "/PHPMyAdmin/", "/.aws/credentials", "/etc/passwd"} {
		assert.True(t, isProbePath(p), "%s is a probe", p)
	}
	for _, p := range []string{"/", "/api/items", "/static/app.js", "/environment", "/gitlog", "/docs"} {
		assert.False(t, isProbePath(p), "%s is ordinary", p)
	}
}

// Strikes decay with the ban clock, and the ban outlives the strikes that
// caused it: a visitor who earned a ban is not released by the counter
// resetting.
func TestGuardStrikesBanOnThreshold(t *testing.T) {
	v := &guardVisitor{}
	now := time.Now()
	ref := AppRef{Owner: "PO", Repo: "app"}
	for i := 1; i < guardBanStrikesDefault; i++ {
		guardStrike(v, now, "a:1.2.3.4", ref, "probe")
		assert.True(t, now.After(v.bannedTo), "strike %d must not ban yet", i)
	}
	guardStrike(v, now, "a:1.2.3.4", ref, "probe")
	assert.True(t, v.bannedTo.After(now), "the threshold strike bans")
	assert.Equal(t, 0, v.strikes, "the counter resets when the ban lands")
}

// The sweep is what keeps a flood of fresh addresses from exhausting memory,
// but a ban must survive it — dropping a banned visitor would lift the ban.
func TestGuardSweepKeepsBansDropsIdle(t *testing.T) {
	now := time.Now()
	idle := &guardVisitor{seen: now.Add(-time.Hour)}
	banned := &guardVisitor{seen: now.Add(-time.Hour), bannedTo: now.Add(time.Hour)}
	fresh := &guardVisitor{seen: now}
	guardVisitors.Store("t:idle", idle)
	guardVisitors.Store("t:banned", banned)
	guardVisitors.Store("t:fresh", fresh)
	defer func() {
		for _, k := range []string{"t:idle", "t:banned", "t:fresh"} {
			guardVisitors.Delete(k)
		}
	}()

	sweepGuardVisitors(now, guardIdleEvict)

	_, idleKept := guardVisitors.Load("t:idle")
	_, bannedKept := guardVisitors.Load("t:banned")
	_, freshKept := guardVisitors.Load("t:fresh")
	assert.False(t, idleKept, "idle visitors are dropped")
	assert.True(t, bannedKept, "a ban is worth keeping")
	assert.True(t, freshKept, "recent visitors stay")
}
