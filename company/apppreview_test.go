// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A preview is the app's twin under a name no repository can have: its
// paths and socket are its own, it runs under the app's policy, and it is
// never an app the proxy would route to at the app's own address.
func TestPreviewIsItsOwnProcessUnderTheAppsPolicy(t *testing.T) {
	live, twin := appPathsFor("PO", "app"), appPathsFor("PO", previewRepo("app"))
	assert.NotEqual(t, live.home, twin.home)
	assert.NotEqual(t, live.socket, twin.socket)
	assert.True(t, isPreviewRepo(previewRepo("app")))
	assert.False(t, isPreviewRepo("app"))

	s := previewSupervisor("PO", "app")
	assert.True(t, s.preview)
	assert.Equal(t, "/apps/_preview/PO/app", s.rootPath)
	assert.Equal(t, SettingsFor("PO", "app"), SettingsFor("PO", previewRepo("app")))

	_, known := LookupApp("PO", previewRepo("app"))
	assert.False(t, known)
	assert.Equal(t, "stopped", PreviewStatusOf("PO", "app").State)
}
