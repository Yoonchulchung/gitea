// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	issues_model "gitea.dev/models/issues"

	"github.com/stretchr/testify/assert"
)

// The sidebar lists every package the snapshot installs, and merging
// approves exactly the ones not allowed yet.
func TestReviewPackagesMarkWhatMergingWouldApprove(t *testing.T) {
	settings := AppSettings{Dependencies: AppDependencies{Allow: []string{"FastAPI"}}}
	got := reviewPackages("fastapi==0.115.0\npandas==2.2.0\n# a comment\n", settings)
	assert.Equal(t, []reviewPackage{
		{Name: "fastapi", Version: "0.115.0", Allowed: true},
		{Name: "pandas", Version: "2.2.0", Allowed: false},
	}, got)
	assert.Empty(t, reviewPackages("", settings))
}

// The assistant is told what was asked for; the diff is cut, not dropped,
// when it is long, and its absence is said rather than silent.
func TestDeployReviewContextSaysWhatIsMissing(t *testing.T) {
	pr := &issues_model.PullRequest{Issue: &issues_model.Issue{Title: "add report"}}
	assert.Contains(t, deployReviewContext(pr, "PO", "app", ""), "no longer available")
	long := deployReviewContext(pr, "PO", "app", string(make([]byte, deployReviewDiffMaxChars+1)))
	assert.Contains(t, long, "get_diff returns all of it")
	assert.Contains(t, long, "No extra permissions are requested.")
}
