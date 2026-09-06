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
	SandboxMode   string
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
			"pinning a version released before this Python existed makes the deploy fail with "+
			"\"No matching distribution found\", naming a transitive dependency the employee never wrote down. "+
			"Prefer leaving versions unpinned, or pin only versions you are certain support this interpreter.\n",
			e.PythonVersion)
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
	if e.NetworkMode == NetworkNone {
		b.WriteString("- The app has NO network access: outbound HTTP calls, DNS and sockets all fail. " +
			"Do not write code that calls an external API unless the employee says that access has been " +
			"approved — suggest they request it instead.\n")
	}
	return b.String()
}
