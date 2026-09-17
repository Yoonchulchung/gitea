// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Accounts come from the directory, so a local password is not the
// credential. The page that would change one is closed to department members
// in every case — including while an administrator has forced a change, which
// is the case that tempted an exception.
func TestLocalPasswordChangeIsNeverReachable(t *testing.T) {
	for _, p := range []string{
		"/user/settings/change_password",
		"/user/settings/account", // owns email changes and account deletion
		"/user/settings/security",
		"/user/settings/keys",
	} {
		assert.False(t, isUserSettingsAllowed(p), "%s must stay blocked", p)
	}

	// What department members do get: their own profile, appearance, and
	// their own AI key.
	for _, p := range []string{"/user/settings", "/user/settings/appearance", "/user/settings/ai"} {
		assert.True(t, isUserSettingsAllowed(p), "%s stays allowed", p)
	}
}
