// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAutoSnapshotDue(t *testing.T) {
	now := time.Unix(100*24*60*60, 0)
	day := int64(24 * 60 * 60)
	st := &AppState{AutoSnapshot: true, HasRelease: true}
	assert.True(t, autoSnapshotDue(st, now), "the first one is taken at once")
	assert.Equal(t, 7, st.autoSnapshotDays(), "seven days unless chosen")

	st.AutoSnapshotAt = now.Unix() - 6*day
	assert.False(t, autoSnapshotDue(st, now))
	st.AutoSnapshotAt = now.Unix() - 7*day
	assert.True(t, autoSnapshotDue(st, now))

	st.AutoSnapshotDays = 3
	st.AutoSnapshotAt = now.Unix() - 3*day
	assert.True(t, autoSnapshotDue(st, now))
	st.AutoSnapshotDays = 5 // not offered, so the default
	assert.False(t, autoSnapshotDue(st, now))

	assert.False(t, autoSnapshotDue(&AppState{AutoSnapshot: false, HasRelease: true}, now))
	assert.False(t, autoSnapshotDue(&AppState{AutoSnapshot: true}, now), "nothing deployed, nothing to run the helper with")
}
