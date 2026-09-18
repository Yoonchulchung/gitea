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

	if available, detail := PythonStatus(); !available {
		// Every department app is a Python app, so this stops the platform
		// dead. Said at boot rather than discovered through the first deploy,
		// where it would surface as a package problem and send a department
		// editing requirements.txt against it.
		log.Error("company: no usable Python — no app can be built: %s", detail)
	}
	if available, detail := SandboxStatus(); !available {
		// Not fatal here — whether an app may start unsandboxed is decided per
		// start (appsandbox.go). But an operator has to learn this at boot,
		// not from a deploy that mysteriously refuses to run.
		log.Warn("company: app sandboxing is unavailable: %s", detail)
	}
	if AppDataEnabled() {
		// Measured at boot so an operator learns it here rather than from an
		// app whose writes mysteriously behave differently than on their
		// laptop (company/appdata.go).
		if ok, detail := DataStatus(); ok {
			log.Info("company: app data: %s", detail)
		} else {
			log.Error("company: app data is switched on but unusable: %s", detail)
		}
	}
	// Before anything reads state or files: shortening appKey renamed every
	// directory the platform owns, and an instance that starts without this
	// finds its apps with no releases and no secrets (company/appkeymigrate.go).
	migrateAppKeyLength()

	// The proxy answers "is this a real app?" from memory, so the registry has
	// to know about everything deployed before the first request arrives.
	loadAppRegistry()
	loadDepartmentAccess()
	// Releases built before they recorded their own commit id can still be
	// identified from the deploy history, and until they are no screen can say
	// which version is serving (company/appreleaseid.go).
	backfillReleaseSHAs(ctx)
	// And what those releases have installed: an administrator comparing
	// package policy against reality cannot do it against "unknown".
	backfillInstalledPackages(ctx)

	StartDeployWorkers()
	StartMetricsFlusher()
	StartLivenessChecks()
	StartAppDataGC()
	StartProxyGuard()
	logAIPolicyAtBoot()

	// Bring back what was running before this restart. Runs in the
	// background: reconciliation starts app processes and health-checks them,
	// which must not hold up the rest of startup.
	go ReconcileApps()
}
