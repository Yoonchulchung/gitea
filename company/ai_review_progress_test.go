// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCodeReviewProgress(t *testing.T) {
	const prID = 990101
	t.Cleanup(func() { codeReviewRuns.Delete(int64(prID)) })

	run, fresh := startCodeReview(prID, "admin")
	assert.True(t, fresh)
	_, fresh = startCodeReview(prID, "admin")
	assert.False(t, fresh) // one review at a time

	run.progress(1, 3)
	s := codeReviewStatusFor(prID)
	assert.Equal(t, "running", s.Phase)
	assert.Equal(t, "admin", s.By)
	assert.Equal(t, [2]int{1, 3}, [2]int{s.Done, s.Total})

	run.finish(prID, "")
	assert.Empty(t, codeReviewStatusFor(prID).Phase) // the comments are the result

	run, _ = startCodeReview(prID, "")
	run.finish(prID, "timeout")
	assert.Equal(t, CodeReviewStatus{Phase: "failed", Failed: "timeout"}, codeReviewStatusFor(prID))
	_, fresh = startCodeReview(prID, "admin")
	assert.True(t, fresh) // a failed one does not block the next try
}
