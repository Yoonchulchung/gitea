// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAutoApproveCountdownAndStop(t *testing.T) {
	const prID = 990001
	t.Cleanup(func() { finishAutoApproveRun(prID) })

	run, fresh := startAutoApproveRun(prID)
	assert.True(t, fresh)
	_, fresh = startAutoApproveRun(prID)
	assert.False(t, fresh) // a second review of the same request is not started
	assert.Equal(t, autoApprovePhaseReviewing, autoApproveStatusFor(prID).Phase)

	// Stopped while the AI is still reading: the wait never starts.
	assert.True(t, stopAutoApprove(prID, "admin"))
	assert.False(t, stopAutoApprove(prID, "admin")) // once is enough
	who, ok := run.waitBeforeMerge(context.Background(), time.Hour)
	assert.False(t, ok)
	assert.Equal(t, "admin", who)
	assert.Equal(t, "stopped", autoApproveStatusFor(prID).Phase)

	// Not stopped: the countdown is visible, then the merge goes ahead.
	finishAutoApproveRun(prID)
	run, _ = startAutoApproveRun(prID)
	done := make(chan bool)
	go func() { _, ok := run.waitBeforeMerge(context.Background(), 300*time.Millisecond); done <- ok }()
	assert.Eventually(t, func() bool { return autoApproveStatusFor(prID).Phase == autoApprovePhaseWaiting }, time.Second, 10*time.Millisecond)
	assert.NotZero(t, autoApproveStatusFor(prID).MergeAt)
	assert.True(t, <-done)

	// Stopped during the countdown.
	finishAutoApproveRun(prID)
	run, _ = startAutoApproveRun(prID)
	go func() { _, ok := run.waitBeforeMerge(context.Background(), time.Hour); done <- ok }()
	assert.Eventually(t, func() bool { return autoApproveStatusFor(prID).Phase == autoApprovePhaseWaiting }, time.Second, 10*time.Millisecond)
	assert.True(t, stopAutoApprove(prID, "admin"))
	assert.False(t, <-done)

	assert.False(t, stopAutoApprove(990002, "admin")) // nothing running
	assert.Equal(t, -1, parseAutoApproveDelay(""))
	assert.Equal(t, 0, parseAutoApproveDelay("0"))
}
