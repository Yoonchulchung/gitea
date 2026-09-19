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
	got := reviewPackages("fastapi==0.115.0\npandas==2.2.0\nnumpy==2.0.0\n# a comment\n", settings,
		[]PermissionRequest{{Kind: PermKindPackage, Value: "NumPy"}})
	assert.Equal(t, []reviewPackage{
		{Name: "fastapi", Version: "0.115.0", Allowed: true},
		{Name: "pandas", Version: "2.2.0"},
		{Name: "numpy", Version: "2.0.0", Requested: true},
	}, got)
	assert.Empty(t, reviewPackages("", settings, nil))

	// Only names the request needs, is refused, and does not already carry
	// pass — however they are spelt.
	approved := packagesToApprove([]string{"Pandas", "fastapi", "numpy", "requests"}, got)
	assert.Len(t, approved, 1)
	assert.Equal(t, "pandas", approved[0].Value)
	assert.Equal(t, "approve", approved[0].Decision)
}

func TestDeployRequestMessageDropsTheBoilerplate(t *testing.T) {
	assert.Equal(t, "Fix the report", deployRequestMessage("Requested by @kim (PO/app).\n\nFix the report"))
	assert.Equal(t, "", deployRequestMessage("Requested by @kim (PO/app)."))
	assert.Equal(t, "plain", deployRequestMessage("plain"))
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
