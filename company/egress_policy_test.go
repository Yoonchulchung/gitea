// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withCompanyINI(t *testing.T, body string) {
	t.Helper()
	cfg, err := setting.NewConfigProviderFromData("[company]\n" + body)
	require.NoError(t, err)
	prev := setting.CfgProvider
	setting.CfgProvider = cfg
	t.Cleanup(func() { setting.CfgProvider = prev })
}

// Off unless said otherwise. The default follows the stated policy rather
// than convenience: a stray request to a forbidden provider is an incident,
// and "it was on by default" is not an answer anyone wants to give.
func TestAIIsOffUnlessExplicitlyOn(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "false": false, "yes": false, "1": false, "true": true} {
		withCompanyINI(t, "AI_ENABLED = "+raw)
		assert.Equal(t, want, AIEnabled(), "AI_ENABLED=%q", raw)
	}
}

// pip reaches pypi.org by default, so the only safe states are "an internal
// index is named" or "the public one was explicitly allowed". Anything else
// must refuse before pip starts, because by the time pip reports a network
// error the request has already left.
func TestPipEgressRefusedWithoutASanctionedIndex(t *testing.T) {
	cases := []struct {
		ini     string
		allowed bool
		envLen  int
	}{
		{"", false, 0},
		{"PIP_ALLOW_PUBLIC_INDEX = false", false, 0},
		{"PIP_ALLOW_PUBLIC_INDEX = true", true, 0},
		{"PIP_INDEX_URL = https://pypi.internal/simple", true, 1},
		{"PIP_INDEX_URL =   \n", false, 0}, // whitespace is not an index
	}
	for _, tc := range cases {
		withCompanyINI(t, tc.ini)
		assert.Equal(t, tc.allowed, pipEgressAllowed(), "ini=%q", tc.ini)
		assert.Len(t, pipIndexEnv(), tc.envLen, "ini=%q", tc.ini)
	}
	withCompanyINI(t, "PIP_INDEX_URL = https://pypi.internal/simple")
	assert.Equal(t, []string{"PIP_INDEX_URL=https://pypi.internal/simple"}, pipIndexEnv())
}
