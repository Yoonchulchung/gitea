// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"

	"gitea.dev/modules/log"
)

// InitAppPlatform starts everything the deployed-app platform needs at
// runtime. Called once from routers/init.go, after settings and the database
// are up — the workers need both, so this cannot be a package init().
//
// One entry point rather than several calls in core code, because every line
// added to routers/init.go is a line that can conflict on the next upstream
// merge (docs/company/patches.md).
func InitAppPlatform(ctx context.Context) {
	// The policy in force comes from git, so a restart has to read it back —
	// otherwise every app silently reverts to built-in defaults and the
	// packages an admin approved last week stop being installable.
	LoadAppsConfigFromRepo(ctx)

	if available, detail := SandboxStatus(); !available {
		// Not fatal here — whether an app may start unsandboxed is decided per
		// start (appsandbox.go). But an operator has to learn this at boot,
		// not from a deploy that mysteriously refuses to run.
		log.Warn("company: app sandboxing is unavailable: %s", detail)
	}
	// The proxy answers "is this a real app?" from memory, so the registry has
	// to know about everything deployed before the first request arrives.
	loadAppRegistry()

	StartDeployWorkers()
	StartMetricsFlusher()

	// Bring back what was running before this restart. Runs in the
	// background: reconciliation starts app processes and health-checks them,
	// which must not hold up the rest of startup.
	go ReconcileApps()
}
