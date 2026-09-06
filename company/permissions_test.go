// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"
	"time"

	"gitea.dev/modules/translation"
	"gitea.dev/services/context"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withPolicy(t *testing.T, yml string) {
	t.Helper()
	cfg, err := ParseAppsConfig([]byte(yml))
	require.NoError(t, err)
	SetAppsConfig(cfg)
	t.Cleanup(func() { SetAppsConfig(&AppsConfig{}) })
}

func TestDetectPermissionRequests(t *testing.T) {
	withTempAppData(t)
	withPolicy(t, `
defaults:
  access: org
  limits: {memoryMB: 512}
  dependencies:
    allow: [fastapi, uvicorn]
`)

	requirements := "fastapi==0.115.0\nuvicorn==0.30.6\nopenpyxl==3.1.5\n"

	t.Run("only unapproved packages are proposed", func(t *testing.T) {
		got := DetectPermissionRequests("PO", "app", requirements, "")
		require.Len(t, got, 1)
		assert.Equal(t, PermKindPackage, got[0].Kind)
		assert.Equal(t, "openpyxl", got[0].Value)
		// The department did not type this into a form — it is what their own
		// file asks for, which is what makes the request real rather than
		// speculative.
		assert.NotEmpty(t, got[0].Evidence)
	})

	t.Run("widening access is proposed, narrowing is not", func(t *testing.T) {
		widened := DetectPermissionRequests("PO", "app", "", AccessPublic)
		require.Len(t, widened, 1)
		assert.Equal(t, PermKindAccess, widened[0].Kind)
		assert.Equal(t, AccessPublic, widened[0].Value)

		// org is already the narrowest, so asking for it again proposes
		// nothing; and there is no reason to make someone wait to be safer.
		assert.Empty(t, DetectPermissionRequests("PO", "app", "", AccessOrg))
	})

	t.Run("a memory increase is proposed only with evidence", func(t *testing.T) {
		assert.Empty(t, DetectPermissionRequests("PO", "quiet", "", ""),
			"an app that never hit its limit must not be offered more memory")

		require.NoError(t, MutateAppState("PO", "hungry", func(st *AppState) bool {
			st.History = []AppHistoryEntry{
				{Status: AppStateFailed, Reason: ReasonOOM},
				{Status: AppStateFailed, Reason: ReasonOOM},
			}
			return true
		}))
		got := DetectPermissionRequests("PO", "hungry", "", "")
		require.Len(t, got, 1)
		assert.Equal(t, PermKindMemory, got[0].Kind)
		assert.Equal(t, "1024", got[0].Value)
		// The evidence has to be a fact, not an assertion: the count travels
		// with the key so the sentence can be written in any language.
		assert.Equal(t, "company.evidence.limit_hits", got[0].Evidence)
		assert.Equal(t, 2, got[0].EvidenceArg)
	})
}

func TestApplyApprovedPermissions(t *testing.T) {
	base := AppSettings{
		Access:   AccessOrg,
		Limits:   AppLimits{MemoryMB: 512},
		Network:  AppNetwork{Mode: NetworkNone},
		Download: AppDownload{Policy: "block"},
	}
	updated := ApplyApprovedPermissions(base, []PermissionRequest{
		{Kind: PermKindPackage, Value: "openpyxl", Decision: "approve"},
		{Kind: PermKindPackage, Value: "pandas", Decision: "reject"},
		{Kind: PermKindAccess, Value: AccessLogin, Decision: "approve"},
		{Kind: PermKindMemory, Value: "1024", Decision: "approve"},
		{Kind: PermKindDownload, Decision: "approve"},
		{Kind: PermKindNetwork, Value: "erp.internal GET,POST", Decision: "approve"},
	})

	// allowExtra, not allow: approving a package for one department must not
	// open it for every other app.
	assert.Equal(t, []string{"openpyxl"}, updated.Dependencies.AllowExtra)
	assert.Empty(t, updated.Dependencies.Allow)
	assert.Equal(t, AccessLogin, updated.Access)
	assert.Equal(t, 1024, updated.Limits.MemoryMB)
	assert.Equal(t, "allow", updated.Download.Policy)
	assert.Equal(t, NetworkBroker, updated.Network.Mode)
	require.Len(t, updated.Network.Allow, 1)
	assert.Equal(t, "erp.internal", updated.Network.Allow[0].Host)
	assert.Equal(t, []string{"GET", "POST"}, updated.Network.Allow[0].Methods)

	t.Run("nothing is applied without an approval", func(t *testing.T) {
		unchanged := ApplyApprovedPermissions(base, []PermissionRequest{
			{Kind: PermKindAccess, Value: AccessPublic},
			{Kind: PermKindMemory, Value: "4096", Decision: "reject"},
		})
		assert.Equal(t, base, unchanged)
	})

	t.Run("approving the same package twice does not duplicate it", func(t *testing.T) {
		once := ApplyApprovedPermissions(base, []PermissionRequest{
			{Kind: PermKindPackage, Value: "openpyxl", Decision: "approve"},
		})
		// "OpenPyXL" and "openpyxl" are one package to pip, so the allowlist
		// must not grow a second entry for the other spelling. (Note that
		// "open_pyxl" would be a genuinely different package under PEP 503 —
		// the separator normalises, the absence of one does not.)
		twice := ApplyApprovedPermissions(once, []PermissionRequest{
			{Kind: PermKindPackage, Value: "OpenPyXL", Decision: "approve"},
		})
		assert.Len(t, twice.Dependencies.AllowExtra, 1)
	})
}

func TestPermissionRequestsRoundTrip(t *testing.T) {
	withTempAppData(t)
	requests := []PermissionRequest{{Kind: PermKindPackage, Value: "openpyxl", Reason: "엑셀 보고서"}}
	require.NoError(t, SavePermissionRequests("PO", "app", 42, requests))

	got := LoadPermissionRequests("PO", "app", 42)
	require.Len(t, got, 1)
	assert.Equal(t, "엑셀 보고서", got[0].Reason)

	// Keyed by PR so an abandoned request never shows up against a new one.
	assert.Empty(t, LoadPermissionRequests("PO", "app", 43))
	assert.Empty(t, LoadPermissionRequests("PO", "other", 42))
}

// The permissions file must not be mistaken for app state: ListAppStates
// reads every *.json in the state directory, so sharing it would produce a
// phantom app with no owner on the admin dashboard.
func TestPermissionFileIsNotListedAsAnApp(t *testing.T) {
	withTempAppData(t)
	require.NoError(t, MutateAppState("PO", "app", func(*AppState) bool { return true }))
	require.NoError(t, SavePermissionRequests("PO", "app", 1,
		[]PermissionRequest{{Kind: PermKindPackage, Value: "x"}}))

	states := ListAppStates()
	require.Len(t, states, 1)
	assert.Equal(t, "PO", states[0].Owner)
}

// The whole point of resolving the tree: an admin approving jinja2 must be
// approving MarkupSafe in the same click, or the build refuses a package
// nobody was ever asked about and the next request hits the layer below it.
func TestPackageRequestsSeparateNamedFromPulledIn(t *testing.T) {
	resolved := []resolvedPackage{
		{Name: "Jinja2", Version: "3.1.6", Direct: true},
		{Name: "MarkupSafe", Version: "3.0.3"},
		{Name: "fastapi", Version: "0.141.1", Direct: true},
	}
	got := packageRequests(resolved, []string{"fastapi"})

	require.Len(t, got, 2, "an already-allowed package is not a request")
	assert.Equal(t, "Jinja2", got[0].Value)
	assert.Equal(t, "company.evidence.in_requirements", got[0].Evidence, "named by the department")
	assert.Equal(t, "MarkupSafe", got[1].Value)
	assert.Equal(t, "company.evidence.transitive", got[1].Evidence,
		"an admin cannot tell on their own that this name is in no file the department wrote")
}

// Names are needed whether or not the line is well-formed: shape is enforced
// elsewhere, and a missing version does not make the package unasked-for.
func TestNamedPackagesReadsUnpinnedAndPinnedAlike(t *testing.T) {
	got := namedPackages("jinja2\npydantic-settings==2.15.0\n# comment\nhttpx>=0.27  # inline\n\n")
	assert.Equal(t, map[string]bool{"jinja2": true, "pydantic-settings": true, "httpx": true}, got)
}

// The dead end this closes: once an app's own requirements are all approved,
// the request form has nothing to tick, so a transitive dependency that
// stopped the build can never be asked for and the app stays broken with no
// action available to anyone.
func TestMissingPackagesBecomeRequestable(t *testing.T) {
	withTempAppData(t)
	require.NoError(t, MutateAppState("PO", "app", func(st *AppState) bool {
		st.MissingPackages = []string{"MarkupSafe", "python-dotenv"}
		return true
	}))

	got := DetectPermissionRequests("PO", "app", "", "")
	require.Len(t, got, 2)
	for _, r := range got {
		assert.Equal(t, PermKindPackage, r.Kind)
		assert.Equal(t, "company.evidence.build_stopped", r.Evidence)
	}
	assert.Equal(t, []string{"MarkupSafe", "python-dotenv"}, []string{got[0].Value, got[1].Value})
}

// The log has to give every request a diff, including one that changes no
// code — that is the case it exists for.
func TestRequestLogEntryNamesWhatIsBeingAsked(t *testing.T) {
	entry := requestLogEntry("kim", "패키지 승인 요청", "",
		[]PermissionRequest{{Label: "패키지 추가", Detail: "MarkupSafe (3.0.3)", Evidence: "의존성입니다", Reason: "빌드가 멈춰서"}},
		time.Unix(1700000000, 0))
	assert.Contains(t, entry, "@kim")
	assert.Contains(t, entry, "MarkupSafe (3.0.3)")
	assert.Contains(t, entry, "빌드가 멈춰서")
}

// Newest first, and bounded: this file is reviewed as a diff, and an admin
// should not scroll past a year of history to find what is being asked now.
func TestRequestLogKeepsRecentEntriesNewestFirst(t *testing.T) {
	existing := requestLogHeader("PO", "app") + "## older\nbody\n\n## oldest\nbody\n"
	kept := trimToRecentEntries(existing, 1)
	assert.Contains(t, kept, "older")
	assert.NotContains(t, kept, "oldest")
	assert.NotContains(t, kept, "직접 편집하지 마세요", "the header is rewritten, not carried")

	assert.Empty(t, trimToRecentEntries("no entries yet", 5))
}

// An admin has to be able to unblock an app without talking a department
// through a form: a dependency the build discovered is not something they can
// request, because it appears in no file they wrote.
func TestClearMissingPackagesDropsOnlyWhatWasApproved(t *testing.T) {
	withTempAppData(t)
	require.NoError(t, MutateAppState("PO", "app", func(st *AppState) bool {
		st.MissingPackages = []string{"MarkupSafe", "python-dotenv"}
		return true
	}))

	// Normalization matters here: apps.yml carries the name as written, and
	// "markupsafe" and "MarkupSafe" are one package to pip.
	clearMissingPackages("PO", "app", []string{"markupsafe"})
	assert.Equal(t, []string{"python-dotenv"}, LoadAppState("PO", "app").MissingPackages)
}

// Packages come from requirements.txt and a memory increase comes from the
// recorded times the limit was hit, so both can be proposed with evidence.
// Nothing in an app says it needs to reach a host or hand over a file — those
// are intentions, and without somewhere to say them a department that needed
// one had no route to a request at all.
func TestOutboundEvidenceSeparatesInternalFromInternet(t *testing.T) {
	for _, host := range []string{"erp.internal.company.com", "billing.local", "reports.corp", "erp"} {
		assert.Contains(t, outboundEvidence(host), "company.evidence.internal", host)
	}
	// The distinction an operator who is not a developer cannot make alone,
	// and it is most of the decision.
	for _, host := range []string{"api.example.com", "hooks.slack.com"} {
		assert.Equal(t, "company.evidence.external", outboundEvidence(host), host)
	}
}

// Someone should not be asked to request access the app already has.
//
// The unrestricted case needs a locale (it is the one entry that is a word
// rather than a host), so it is covered on the handler side; here the two
// data cases are what matter.
func TestAllowedOutboundHostsReportsWhatIsInForce(t *testing.T) {
	ctx := &context.Context{Base: &context.Base{Locale: translation.MockLocale{}}}
	assert.Empty(t, allowedOutboundHosts(ctx, AppSettings{Network: AppNetwork{Mode: NetworkNone}}))
	assert.Equal(t, []string{"erp.internal"}, allowedOutboundHosts(ctx, AppSettings{Network: AppNetwork{
		Mode: NetworkBroker, Allow: []AppNetworkRule{{Host: "erp.internal"}},
	}}))
}

// The distinction that decides whether a submission is stopped: a version
// conflict is the department's to fix, an unreachable index is the platform's.
// Erring towards "unreachable" on anything ambiguous, because telling someone
// their code is broken when the index was briefly down is worse than saying
// nothing.
func TestIndexUnreachableIsNotAVersionConflict(t *testing.T) {
	conflicts := []string{
		"ERROR: No matching distribution found for pydantic==2.5.0",
		"ERROR: Could not find a version that satisfies the requirement foo",
		"ResolutionImpossible: for help visit ...",
		"The conflict is caused by: fastapi 0.141.1 depends on starlette<1.7.0",
	}
	for _, out := range conflicts {
		assert.False(t, indexUnreachable(out), out)
	}
	outages := []string{
		"WARNING: Retrying after connection broken by 'NewConnectionError'",
		"ERROR: Could not install packages due to an OSError: [Errno 28] No space left",
		"", // nothing to go on, so it must not read as the department's fault
	}
	for _, out := range outages {
		assert.True(t, indexUnreachable(out), out)
	}
}

// pip prints its whole search on failure and the sentence that matters is at
// the end; the rest names the server's directories.
func TestResolveFailureSummaryKeepsTheTail(t *testing.T) {
	got := resolveFailureSummary("Collecting a\nCollecting b\n" +
		"ERROR: Could not find a version that satisfies the requirement pydantic==2.5.0\n[notice] upgrade pip\n")
	assert.Contains(t, got, "pydantic==2.5.0")
	assert.NotContains(t, got, "[notice]")
	assert.NotContains(t, got, "Collecting a", "only the tail is worth showing")
}

// An approval flow that can only ever add is a ratchet, and "we approved that
// by mistake" has to have an answer.
func TestOutboundRulesCanBeAddedReplacedAndWithdrawn(t *testing.T) {
	s := AppSettings{Network: AppNetwork{Mode: NetworkNone}}

	addOutbound("erp.internal", []string{"GET"})(&s)
	assert.Equal(t, NetworkBroker, s.Network.Mode, "adding a host opens the broker")
	require.Len(t, s.Network.Allow, 1)

	// Re-approving the same host replaces its methods rather than listing it
	// twice, which would leave two rules disagreeing about what is allowed.
	addOutbound("ERP.Internal", []string{"GET", "POST"})(&s)
	require.Len(t, s.Network.Allow, 1)
	assert.Equal(t, []string{"GET", "POST"}, s.Network.Allow[0].Methods)

	// Withdrawing the last host closes the network: broker mode with an empty
	// list reads on the admin screen as "outbound is on" while nothing is
	// actually reachable.
	removeOutbound("erp.internal")(&s)
	assert.Empty(t, s.Network.Allow)
	assert.Equal(t, NetworkNone, s.Network.Mode)

	// "Unrestricted" is an explicit exception and is not downgraded by adding
	// a host to it.
	open := AppSettings{Network: AppNetwork{Mode: NetworkOpen}}
	addOutbound("a.internal", nil)(&open)
	assert.Equal(t, NetworkOpen, open.Network.Mode)
}

// A scheme, a path or a wildcard would be ignored by the broker while reading
// on the admin screen as though it had been applied.
func TestOutboundHostMustBeAHostname(t *testing.T) {
	for _, host := range []string{"erp.internal.company.com", "erp", "a-b.example.com"} {
		assert.True(t, validOutboundHost(host), host)
	}
	for _, host := range []string{
		"", "https://erp.internal", "erp.internal/api", "*.internal",
		"erp.internal:8080", ".internal", "-erp.internal", "erp internal",
	} {
		assert.False(t, validOutboundHost(host), host)
	}
}

// Least privilege by default: reading is what almost every internal
// integration needs, and writing is a separate decision.
func TestParseMethodsDefaultsToReadOnly(t *testing.T) {
	assert.Equal(t, []string{"GET"}, parseMethods(""))
	assert.Equal(t, []string{"GET"}, parseMethods("  ,  "))
	assert.Equal(t, []string{"GET", "POST"}, parseMethods("get, post"))
}

// Pasting a URL is what a person naturally does — they have the site open in
// another tab — and refusing it taught them nothing about what the field
// wanted.
func TestOutboundHostAcceptsAPastedURL(t *testing.T) {
	cases := map[string]string{
		"https://www.hyundaimotorgroup.com/ko/news/newsMain": "www.hyundaimotorgroup.com",
		"http://erp.internal.company.com/api/v1/employees":   "erp.internal.company.com",
		"https://erp.internal.company.com":                   "erp.internal.company.com",
		"erp.internal.company.com/api":                       "erp.internal.company.com",
		"erp.internal.company.com:8443":                      "erp.internal.company.com",
		"  erp.internal.company.com  ":                       "erp.internal.company.com",
		"erp.internal.company.com.":                          "erp.internal.company.com",
	}
	for in, want := range cases {
		got, ok := normalizeOutboundHost(in)
		require.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}

	// Still refused: a wildcard the broker would ignore while this screen
	// showed it as applied, and anything with no host in it at all.
	for _, in := range []string{"", "   ", "*.internal", "ftp://erp.internal", "/api/v1"} {
		_, ok := normalizeOutboundHost(in)
		assert.False(t, ok, in)
	}
}

// The methods someone chose were dropped on the way through approval: the
// request stored the host in Value while approval expected "host METHODS" and
// split it on a space, so every approved rule came out with none.
func TestApprovedOutboundKeepsItsMethods(t *testing.T) {
	got := ApplyApprovedPermissions(AppSettings{Network: AppNetwork{Mode: NetworkNone}}, []PermissionRequest{{
		Kind: PermKindNetwork, Value: "erp.internal", Methods: []string{"GET", "POST"}, Decision: "approve",
	}})
	require.Len(t, got.Network.Allow, 1)
	assert.Equal(t, "erp.internal", got.Network.Allow[0].Host)
	assert.Equal(t, []string{"GET", "POST"}, got.Network.Allow[0].Methods)
	assert.Equal(t, NetworkBroker, got.Network.Mode)

	// A request written before Methods existed is still understood rather than
	// silently granting nothing — one may be waiting on an admin right now.
	legacy := ApplyApprovedPermissions(AppSettings{}, []PermissionRequest{{
		Kind: PermKindNetwork, Value: "erp.internal GET,POST", Decision: "approve",
	}})
	require.Len(t, legacy.Network.Allow, 1)
	assert.Equal(t, []string{"GET", "POST"}, legacy.Network.Allow[0].Methods)

	// And one that says nothing about methods grants reading, not writing.
	bare := ApplyApprovedPermissions(AppSettings{}, []PermissionRequest{{
		Kind: PermKindNetwork, Value: "erp.internal", Decision: "approve",
	}})
	assert.Equal(t, []string{"GET"}, bare.Network.Allow[0].Methods)
}
