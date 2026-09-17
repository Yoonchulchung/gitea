// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"strconv"
	"strings"
)

// What an app will actually run on, recorded so it can be read rather than
// discovered.
//
// The AI assistant writes code for these repositories, and every wrong guess
// it makes about the environment costs a department a failed deploy: pinning
// pydantic to a version whose wheels predate the host's interpreter is
// exactly how this platform's first real deploy broke. It has to know the
// Python version.
//
// It must not find that out by running anything. The assistant edits files
// on behalf of someone who cannot read what it produces, so giving it a way
// to execute commands on the server would make "the AI decided to" a path to
// everything the sandbox exists to prevent — and the sandbox does not apply
// to Gitea's own process. So the platform publishes a snapshot instead: the
// values are collected by code that already had them (the startup probes in
// pythonenv.go and appsandbox.go, the policy in apps.yml) and handed over as
// text.
//
// Read-only by construction. There is no call here that changes anything,
// and nothing in it comes from the AI.

// PlatformEnvironment is the snapshot.
type PlatformEnvironment struct {
	PythonVersion string // empty when the host has no usable interpreter
	PythonError   string // why, when it does not
	BasePackages  []string
	AllowedExtra  []string // what this repo may add on top
	MemoryLimitMB int
	StartCommand  string
	HealthPath    string
	NetworkMode   string
	// NetworkEnforced is whether the sandbox is actually imposing the mode
	// above. Policy and outcome differ on a host with no sandbox.
	NetworkEnforced bool
	SandboxMode     string
	// What the app's own database will run on. Measured, not assumed — the
	// filesystem under AppDataPath decides whether WAL is available at all
	// (company/appdata.go).
	DataEnabled     bool
	DataDetail      string
	DataJournalMode string
}

// DescribePlatformEnvironment collects what an app in this repository will
// run on. Every value comes from a probe or from policy that is already in
// memory, so this is cheap enough to call per request.
func DescribePlatformEnvironment(owner, repo string) PlatformEnvironment {
	settings := SettingsFor(owner, repo)
	env := PlatformEnvironment{
		BasePackages:  settings.BasePackages,
		AllowedExtra:  settings.Dependencies.AllowExtra,
		MemoryLimitMB: settings.Limits.MemoryMB,
		StartCommand:  settings.Start,
		HealthPath:    settings.HealthPath,
		NetworkMode:   settings.Network.Mode,
	}
	if info, err := pythonProbe(); err == nil {
		env.PythonVersion = info.Version
	} else {
		env.PythonError = err.Error()
	}
	mode, _ := sandboxMode()
	env.SandboxMode = string(mode)
	env.DataEnabled, env.DataDetail = DataStatus()
	if env.DataEnabled {
		env.DataJournalMode = DataJournalMode()
	}
	env.NetworkEnforced, _ = NetworkEnforced()
	return env
}

// AIContext renders the snapshot as the paragraph the assistant is given.
//
// Written as instructions rather than a data dump, because a model handed a
// table of facts still guesses at what to do with them. The Python version
// is stated with its consequence attached — that is the part that stops a
// pinned dependency whose wheels do not exist for it.
func (e PlatformEnvironment) AIContext() string {
	var b strings.Builder
	b.WriteString("\n\nThis repository is deployed as an internal web app by the platform itself. " +
		"Facts about the server it runs on, so you do not have to guess at them:\n")

	if e.PythonVersion != "" {
		fmt.Fprintf(&b, "- Python %s. Any dependency you add must have a wheel built for it — "+
			"a version released before this Python existed fails the deploy with \"No matching "+
			"distribution found\", often naming a transitive dependency the employee never wrote down. "+
			"So pin recent versions, and if you are not confident a version supports Python %s, "+
			"say so to the employee instead of guessing.\n",
			e.PythonVersion, e.PythonVersion)
	} else {
		fmt.Fprintf(&b, "- The server currently has no usable Python (%s), so nothing will deploy "+
			"until an administrator fixes that. Say so if the employee is waiting on a deploy.\n", e.PythonError)
	}

	if len(e.BasePackages) > 0 {
		fmt.Fprintf(&b, "- Already installed for every app, so do NOT add these to requirements.txt: %s.\n",
			strings.Join(e.BasePackages, ", "))
	}
	if len(e.AllowedExtra) > 0 {
		fmt.Fprintf(&b, "- Also approved for this repository: %s.\n", strings.Join(e.AllowedExtra, ", "))
	}
	b.WriteString("- Any other package needs an administrator's approval through a Deploy Request. " +
		"Adding one to requirements.txt is allowed and is how the request gets raised, but tell the " +
		"employee it will need approving rather than letting them expect it to just work.\n")

	if e.StartCommand != "" {
		fmt.Fprintf(&b, "- The app is started with: %s — so the code must expose what that expects "+
			"(for the default, an `app` object in main.py at the repository root).\n", e.StartCommand)
	}
	if e.MemoryLimitMB > 0 {
		fmt.Fprintf(&b, "- Memory limit: %s MB. Reading a large file entirely into memory will be "+
			"stopped by the platform.\n", strconv.Itoa(e.MemoryLimitMB))
	}
	if e.DataEnabled {
		// The single highest-leverage paragraph here. Department app code is
		// written by this assistant, so a model that does not know the
		// contract reproduces the original bug — a relative sqlite3 path
		// against a read-only release tree — in every app it generates.
		fmt.Fprintf(&b, "- The app has persistent storage: **one SQLite database**, opened at the path in "+
			"the DB_PATH environment variable. Always `sqlite3.connect(os.environ[\"DB_PATH\"], timeout=10)`; "+
			"never a relative path or a literal, because the release directory is read-only and a relative "+
			"path fails with \"unable to open database file\". Journal mode on this host is %s and the "+
			"platform sets it — do not set PRAGMA journal_mode yourself.\n", e.DataJournalMode)
		b.WriteString("- Do NOT create or alter tables from app code — no CREATE TABLE at import time, no " +
			"init_db(). Schema lives in numbered files the platform applies before the app starts: " +
			"`migrations/001_create_records.sql`, `migrations/002_add_note.sql`, and so on, next to main.py.\n")
		b.WriteString("- Migrations are **additive only**: add tables, add nullable columns, add columns with " +
			"a DEFAULT. Never DROP, never RENAME, never a NOT NULL column without a DEFAULT — the platform " +
			"refuses those, because any past version of the app can be redeployed and must still work. " +
			"Never edit a migration that has already shipped; add the next number instead.\n")
		b.WriteString("- Open a connection per request and close it, rather than sharing one: the synchronous " +
			"endpoints FastAPI runs in a threadpool cannot share a sqlite3 connection. Store uploaded files " +
			"as BLOBs in the database — there is no other writable directory that survives a deploy.\n")
	}

	switch {
	case e.NetworkMode == NetworkNone && e.NetworkEnforced:
		b.WriteString("- The app has NO network access: outbound HTTP calls, DNS and sockets all fail. " +
			"Do not write code that calls an external API unless the employee says that access has been " +
			"approved — suggest they request it instead.\n")
	case e.NetworkMode == NetworkNone:
		// Said as policy rather than as fact, because it is not being
		// enforced here and telling the model calls "fail" would have it
		// write code around a barrier that is not there — or trust one that
		// is not there.
		b.WriteString("- Policy for this app is no outbound network access, but this host cannot enforce it, " +
			"so calls would actually succeed. Still do not write code that calls an external API unless the " +
			"employee says it has been approved: it is not allowed, only unblocked.\n")
	}
	return b.String()
}
