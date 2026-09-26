// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"strings"
	"sync/atomic"

	system_model "gitea.dev/models/system"
	"gitea.dev/modules/log"
)

// Where the central deploy repository is now.
//
// `[company] CENTRAL_DEPLOY_REPO` names it once, in app.ini, which was fine
// while it never moved. It can: an administrator may rename it or transfer
// it to another owner from its own settings page, and nothing about this
// platform should stand in the way of that — it is a repository like any
// other, and the one screen where moving it is offered is the screen where
// somebody would do it.
//
// What must not happen is the platform losing it. Every policy read, every
// deploy request and every approval resolves the repository by owner and
// name, so a move used to leave app.ini pointing at nothing until an
// operator edited the file and restarted: the apps kept running on the
// policy last read, and every new request failed.
//
// So the move is followed. The new location is written to the database and
// read back at boot, and app.ini stays the answer until something has moved
// — an instance that never moves it never needs the setting.
const settingKeyCentralRepo = "company.central_deploy_repo"

// centralRepoMoved holds the current location, "" while it is still where
// app.ini says. An atomic value rather than the settings cache next door
// because centralDeployOwnerName has no context to read the database with —
// it is called from notifier hooks that run outside any request.
var centralRepoMoved atomic.Value // string

func init() { centralRepoMoved.Store("") }

// LoadCentralRepoLocation reads back a move made before this restart.
// Called from InitAppPlatform, once the database is up.
func LoadCentralRepoLocation(ctx context.Context) {
	_, all, err := system_model.GetAllSettings(ctx)
	if err != nil {
		log.Error("company: reading where the central deploy repository is: %v", err)
		return
	}
	if moved := all[settingKeyCentralRepo]; moved != "" {
		centralRepoMoved.Store(moved)
		log.Info("company: the central deploy repository is %s (moved from the [company] CENTRAL_DEPLOY_REPO value)", moved)
	}
}

// followCentralRepoMove records that the central repository is somewhere
// else now.
//
// Best-effort and quiet about the common case: this runs from a notifier on
// every rename and transfer on the instance, and almost none of them are
// this repository.
func followCentralRepoMove(ctx context.Context, oldOwner, oldName, newOwner, newName string) {
	owner, name, err := centralDeployOwnerName()
	if err != nil || !strings.EqualFold(owner, oldOwner) || !strings.EqualFold(name, oldName) {
		return
	}
	moved := newOwner + "/" + newName
	centralRepoMoved.Store(moved)
	if err := system_model.SetSettings(ctx, map[string]string{settingKeyCentralRepo: moved}); err != nil {
		// In force for this process either way; the next restart would fall
		// back to app.ini, which is why this is an error and not a warning.
		log.Error("company: the central deploy repository moved to %s but that could not be saved: %v", moved, err)
		return
	}
	log.Info("company: the central deploy repository moved from %s/%s to %s", oldOwner, oldName, moved)
}
