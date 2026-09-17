// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"net/http"
	"net/mail"
	"strings"
	"sync"

	system_model "gitea.dev/models/system"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	gitea_context "gitea.dev/services/context"
)

const tplAdminSettings templates.TplName = "company/admin_settings"

// Platform choices that change while the instance runs.
//
// app.ini already holds everything an operator decides once — paths, limits,
// whether a feature exists at all. These are different: they are decided by
// an administrator, from a screen, and they change. Who to contact changes
// when someone leaves; which AI provider departments may use changes when
// somebody signs a contract. Neither should need a file edit and a restart,
// so they live in the database.
const (
	settingKeyAnthropicVisible = "company.ai.anthropic_visible"
	settingKeySupportEmail     = "company.support_email"
)

// platformSettings is read on pages people load constantly — the settings
// navbar, the department gate — so it is cached rather than queried each
// time. Invalidated by the only writer there is, below.
var (
	platformSettingsMu     sync.RWMutex
	platformSettingsLoaded bool
	anthropicVisible       bool
	supportEmail           string
)

func loadPlatformSettings(ctx context.Context) {
	platformSettingsMu.RLock()
	loaded := platformSettingsLoaded
	platformSettingsMu.RUnlock()
	if loaded {
		return
	}
	// Read before the lock is taken, not under it. These are read on pages
	// people load constantly, so holding the write lock across a database
	// round-trip would serialise every one of them behind a slow query. A
	// duplicated read during a cold-start race costs nothing.
	//
	// The whole table, because the model has no single-key read and this runs
	// once per change rather than once per request.
	_, all, err := system_model.GetAllSettings(ctx)
	if err != nil {
		log.Error("company: reading platform settings: %v", err)
		return // stay unloaded, so the next caller tries again
	}

	platformSettingsMu.Lock()
	defer platformSettingsMu.Unlock()
	anthropicVisible = all[settingKeyAnthropicVisible] == "true"
	supportEmail = all[settingKeySupportEmail]
	platformSettingsLoaded = true
}

// AnthropicVisibleToUsers reports whether the Anthropic provider is offered
// on everyone's AI settings page.
//
// Off unless an administrator turns it on. The default is the cautious one
// because this is the provider an app's code and a department's files are
// sent to: reaching a third party should be a decision somebody made, not
// something that was already true when they first looked.
func AnthropicVisibleToUsers(ctx context.Context) bool {
	loadPlatformSettings(ctx)
	platformSettingsMu.RLock()
	defer platformSettingsMu.RUnlock()
	return anthropicVisible
}

// SupportEmail is who a person stuck at the department gate should write to,
// or "" when nobody has been named.
func SupportEmail(ctx context.Context) string {
	loadPlatformSettings(ctx)
	platformSettingsMu.RLock()
	defer platformSettingsMu.RUnlock()
	return supportEmail
}

// anthropicAllowedFor is the check every AI path goes through.
//
// Administrators keep the provider whatever the setting says: the toggle is
// about what departments are offered, and an admin who turned it off should
// still be able to try it before turning it back on. The user lookup only
// happens when the answer could still change, so the common path is one
// cached read.
func anthropicAllowedFor(ctx context.Context, userID int64) bool {
	if AnthropicVisibleToUsers(ctx) {
		return true
	}
	u, err := user_model.GetUserByID(ctx, userID)
	return err == nil && u.IsAdmin
}

// AdminSettings renders the platform settings an administrator can change.
func AdminSettings(ctx *gitea_context.Context) {
	ctx.Data["Title"] = ctx.Locale.TrString("company.adminsettings.title")
	ctx.Data["AnthropicVisible"] = AnthropicVisibleToUsers(ctx)
	ctx.Data["SupportEmail"] = SupportEmail(ctx)
	ctx.HTML(http.StatusOK, tplAdminSettings)
}

// AdminSettingsPost saves them.
func AdminSettingsPost(ctx *gitea_context.Context) {
	email := strings.TrimSpace(ctx.FormString("support_email"))
	if email != "" {
		// Parsed rather than pattern-matched: this address is printed to
		// someone who is already stuck, and a typo there leaves them with no
		// way through at all.
		if _, err := mail.ParseAddress(email); err != nil {
			ctx.Flash.Error(ctx.Locale.TrString("company.adminsettings.email_invalid", email))
			ctx.Redirect(setting.AppSubURL + "/-/admin/company-settings")
			return
		}
	}
	visible := "false"
	if ctx.FormBool("anthropic_visible") {
		visible = "true"
	}

	if err := system_model.SetSettings(ctx, map[string]string{
		settingKeyAnthropicVisible: visible,
		settingKeySupportEmail:     email,
	}); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	} else {
		invalidatePlatformSettings()
		ctx.Flash.Success(ctx.Locale.TrString("company.adminsettings.saved"))
	}
	ctx.Redirect(setting.AppSubURL + "/-/admin/company-settings")
}

func invalidatePlatformSettings() {
	platformSettingsMu.Lock()
	platformSettingsLoaded = false
	platformSettingsMu.Unlock()
}
