// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// What an app is made of, read from its files the way buildpacks do: a
// main.py is a Python app, and a repository with only an index.html is a
// static site. See docs/company/runtimes.md.
const (
	RuntimePython = "python"
	RuntimeJava   = "java" // Spring Boot, built with Maven (company/appjava.go)
	RuntimeStatic = "static"
)

// staticRoots are where a static site's index.html is looked for, in order:
// a build's output first, since a Vite project also has an index.html at its
// root that only works before it is built.
var staticRoots = []string{"dist", "build", "", "public"}

// errNoEntry is a repository with neither a main.py nor an index.html.
var errNoEntry = userKeyError("company.err.no_main_py")

// releaseRuntimeFile records in a release what it was built as, so starting
// it never has to guess again.
const releaseRuntimeFile = "runtime"

// detectRuntime names an app's runtime and, for a static site, the folder it
// is served from. has reports whether a file exists at a slash path.
func detectRuntime(has func(rel string) bool) (runtime, root string, ok bool) {
	if has("main.py") {
		return RuntimePython, "", true
	}
	if has("pom.xml") {
		return RuntimeJava, "", true
	}
	for _, dir := range staticRoots {
		if has(path.Join(dir, "index.html")) {
			return RuntimeStatic, dir, true
		}
	}
	return "", "", false
}

func detectRuntimeDir(appDir string) (runtime, root string, ok bool) {
	return detectRuntime(func(rel string) bool {
		fi, err := os.Stat(filepath.Join(appDir, filepath.FromSlash(rel)))
		return err == nil && fi.Mode().IsRegular()
	})
}

func writeReleaseRuntime(release, runtime, root string) error {
	return os.WriteFile(filepath.Join(release, releaseRuntimeFile), []byte(strings.TrimSpace(runtime+" "+root)+"\n"), 0o600)
}

// releaseRuntime reads what a release was built as: for a static site the
// folder it serves, for Java the jar it runs. Releases from before there was
// a choice are Python.
func releaseRuntime(release string) (runtime, root string) {
	body, err := os.ReadFile(filepath.Join(release, releaseRuntimeFile))
	if err != nil {
		return RuntimePython, ""
	}
	runtime, root, _ = strings.Cut(strings.TrimSpace(string(body)), " ")
	if runtime != RuntimeStatic && runtime != RuntimeJava {
		return RuntimePython, ""
	}
	return runtime, root
}

// currentRuntime is what an app's running release was built as; empty
// before its first deploy.
func currentRuntime(owner, repo string) string {
	release, err := os.Readlink(appPathsFor(owner, repo).current)
	if err != nil {
		return ""
	}
	rt, _ := releaseRuntime(release)
	return rt
}

// probeEvery caches a check of the host for a minute at a time, so a tool an
// administrator installs is offered without restarting Gitea.
func probeEvery[T any](ttl time.Duration, probe func() (T, error)) func() (T, error) {
	var (
		mu  sync.Mutex
		at  time.Time
		v   T
		err error
	)
	return func() (T, error) {
		mu.Lock()
		defer mu.Unlock()
		if at.IsZero() || time.Since(at) >= ttl {
			v, err = probe()
			at = time.Now()
		}
		return v, err
	}
}

// platformProgram in a start argv stands for this Gitea binary.
const platformProgram = "@gitea"

func dirExists(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// staticServerArgs starts the platform's file server (`gitea deptapp-static`)
// as the app, under the same sandbox and supervisor a Python app gets.
func staticServerArgs(p appPaths, release, root string) []string {
	return []string{
		platformProgram, "deptapp-static",
		"--socket", appSocketForProcess(p),
		"--root", path.Join(appCodeDirForProcess(filepath.Join(release, "app")), root),
	}
}

// runtimeVersions is what this server runs for the guide: the versions are
// read from the host and from a built environment, never written down.
type runtimeVersions struct {
	Python    string // empty when this host has no usable Python
	Framework []installedPackage
	Java      string // empty when this host cannot build Java apps
	Maven     string
}

func supportedRuntimes(owner, repo string) runtimeVersions {
	var out runtimeVersions
	if info, err := pythonProbe(); err == nil {
		out.Python = info.Version
	}
	if info, err := javaProbe(); err == nil && info.MavenErr == nil {
		out.Java, out.Maven = info.JavaVersion, info.MavenVersion
	}
	base := SettingsFor(owner, repo).BasePackages
	for _, pkg := range platformStackPackages(base) {
		if slices.ContainsFunc(base, func(b string) bool { return normalizePackageName(b) == normalizePackageName(pkg.Name) }) {
			out.Framework = append(out.Framework, pkg)
		}
	}
	return out
}
