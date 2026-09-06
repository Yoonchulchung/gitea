// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An app prints whatever it prints and a department names things however
// they like, and all of it ends up on admin screens. Go templates escape by
// default, so the rule is not "escape everything" — it is "never opt out",
// and these tests pin that rule so it survives people who did not read the
// comments that state it.

// companyTemplateRoots are every template that renders employee- or
// app-supplied text.
func companyTemplateRoots(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "custom", "templates")
	var out []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if !d.IsDir() && strings.HasSuffix(path, ".tmpl") {
			out = append(out, path)
		}
		return nil
	}))
	require.NotEmpty(t, out)
	return out
}

var templateComment = regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`)

// The escape hatches html/template offers. HTMLFormat is permitted only with
// a constant format string, which is how the upstream-copied templates use it.
var unsafeTemplateFuncs = regexp.MustCompile(`\b(SafeHTML|Str2html|safeHTML)\b`)

func TestCompanyTemplatesNeverOptOutOfEscaping(t *testing.T) {
	for _, path := range companyTemplateRoots(t) {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		code := templateComment.ReplaceAllString(string(body), "")
		assert.False(t, unsafeTemplateFuncs.MatchString(code),
			"%s uses an escape hatch on rendered content; an app's own output must never become markup", path)
	}
}

// Tr returns an unknown key verbatim as template.HTML — so a non-constant key
// is an escape hatch in disguise. Every `Tr .Something` must either be one of
// our own constant key sets, or be guarded by HasKey with an escaped
// fallback, which is what this test walks the templates to check.
var trDynamicKey = regexp.MustCompile(`ctx\.Locale\.Tr\s+(\.[A-Za-z.]+|\$[A-Za-z.]*\.[A-Za-z.]+)`)

func TestDynamicTrKeysComeFromOurOwnVocabulary(t *testing.T) {
	// The fields rendered through a dynamic Tr, and the functions that
	// produce them. Everything these return is either a company.* constant or
	// falls back through an escaped, HasKey-guarded path in the template.
	trusted := map[string]bool{
		// AppCause: keys by construction. .Summary is also deployAttempt.Summary
		// on the history pages, where it is HasKey-guarded — a key while a deploy
		// is in progress, the build log's own words once it has finished.
		".Summary": true, ".Detail": true, ".AdminHint": true, ".ActionLabel": true,
		".StatusLabel": true, "$.CompanyApp.StatusLabel": true, // departmentStatusLabel: constant set
		".What":                 true,                                                    // historyStatusLabel: HasKey-guarded in history_table.tmpl
		".DeployFormHeadingKey": true,                                                    // one of two constants in DeployForm
		".Label":                true, ".Value": true, ".Reason": true, ".Explain": true, // PermissionRow/accessOption: keys by construction; stored request labels are HasKey-guarded
		"$r.Label":   true, // admin_packages pending list: HasKey-guarded
		".Evidence":  true, // stored request evidence: HasKey-guarded
		".DetailKey": true, // PermissionRequest.DetailKey: written only by our own code, never from a form
	}
	for _, path := range companyTemplateRoots(t) {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		code := templateComment.ReplaceAllString(string(body), "")
		for _, m := range trDynamicKey.FindAllStringSubmatch(code, -1) {
			assert.True(t, trusted[m[1]],
				"%s translates dynamic key %q — if its value can carry anything but our own key constants, an unknown value renders as raw HTML", path, m[1])
		}
	}
}

// The producers of those dynamic keys: everything they can return for any
// input is either a company.* key or text the template escapes.
func TestLabelFunctionsReturnKeysOrPlainText(t *testing.T) {
	hostile := `<script>alert(1)</script>`

	// An unclassified status or reason passes through as itself — and the
	// template's fallback branch escapes it. What must never happen is a
	// hostile value coming back *prefixed* as one of our keys.
	assert.Equal(t, hostile, historyStatusLabel(hostile))
	assert.Equal(t, hostile, historyReasonLabel(hostile))
	for _, status := range []string{AppStateRunning, AppStateStopped, AppStateFailed, AppStateSuspended, AppStateQueued} {
		assert.True(t, strings.HasPrefix(historyStatusLabel(status), "company.app.history."))
		assert.True(t, strings.HasPrefix(departmentStatusLabel(&AppState{Actual: status}), "company.app.status."))
	}

	// AppCause fields are keys for every reason code that exists, and the
	// verbatim half lives in DetailText, which no template ever translates.
	for _, reason := range []string{
		ReasonInstallFailed, ReasonPackageDenied, ReasonOOM, ReasonHealthTimeout,
		ReasonCrashLoop, ReasonSuspended, ReasonNoPython, ReasonSandboxUnavailable,
		ReasonSecretError, ReasonDeployQueueFull, ReasonNoRelease,
		ReasonContractViolation, ReasonRolledBack, "something-new",
	} {
		cause := departmentCause(&AppState{Reason: reason, UserMessage: hostile, Message: hostile})
		require.NotNil(t, cause, reason)
		assert.True(t, strings.HasPrefix(cause.Summary, "company.app.cause."), reason)
		if cause.Detail != "" {
			assert.True(t, strings.HasPrefix(cause.Detail, "company.app.cause."), reason)
		}
	}
}
