// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/process"
)

// A requirements.txt names what an employee wants; pip installs what those
// packages need. jinja2 pulls in MarkupSafe, pydantic-settings pulls in
// python-dotenv, and neither name appears in any file anyone wrote.
//
// The allowlist is enforced against what actually gets installed, so before
// this existed the two halves disagreed: the admin approved the three
// packages the request listed, the build then refused two more nobody had
// been asked about, and the next request approved those and hit the layer
// below. There is no bound on that loop, and every turn of it leaves the
// department's deploy failed while they wait.
//
// So the tree is resolved when the request is made, and the admin approves
// the whole set at once. Slower and it needs the index reachable, which is
// the price of the admin seeing every package that will really be installed
// rather than only the ones an employee happened to name.

// depResolveTimeout bounds a resolution. Long enough for pip to walk a real
// dependency tree over the network, short enough that a hung index does not
// hold a request goroutine open indefinitely.
const depResolveTimeout = 90 * time.Second

// resolvedPackage is one entry of the install set pip computed.
type resolvedPackage struct {
	Name    string
	Version string
	// Direct marks a package the requirements.txt named itself. The rest are
	// dependencies, and an admin reads those differently: a direct package is
	// a choice someone made, a transitive one is a consequence of it.
	Direct bool
}

// pipReport is the shape of `pip install --report`.
type pipReport struct {
	Install []struct {
		Requested bool `json:"requested"`
		Metadata  struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"metadata"`
	} `json:"install"`
}

// resolveDependencies asks pip what installing these requirements would
// actually pull in, without installing anything.
//
// --dry-run --report does the full resolution and writes the result as JSON;
// --only-binary=:all: matches how the platform installs, so the answer is the
// set that will really be used rather than one that assumes source builds are
// available.
func resolveSet(ctx context.Context, requirements string) ([]resolvedPackage, error) {
	python, err := pythonPath()
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "company-depresolve-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	reqFile := filepath.Join(dir, "requirements.txt")
	if err := os.WriteFile(reqFile, []byte(requirements), 0o600); err != nil {
		return nil, err
	}
	reportFile := filepath.Join(dir, "report.json")

	ctx, cancel := context.WithTimeout(ctx, depResolveTimeout)
	defer cancel()

	// process.CommandContext, not exec's: it registers the child with Gitea's
	// process manager so a shutdown terminates it rather than leaving pip
	// holding a connection open.
	cmd := process.CommandContext(ctx, python, "-m", "pip", "install", //nolint:gosec // python comes from PATH or an admin-set config key
		"--dry-run", "--quiet", "--disable-pip-version-check",
		"--only-binary=:all:", "--report", reportFile, "-r", reqFile)
	// pip writes caches and temporary trees relative to HOME; without one of
	// its own it would touch the Gitea account's.
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "LANG=C.UTF-8"}
	if out, err := cmd.CombinedOutput(); err != nil {
		// The output names the index and the local paths pip tried, so it goes
		// to the log in full and to the caller as a classified error.
		text := strings.TrimSpace(string(out))
		log.Warn("company: resolving dependencies: %v: %s", err, text)
		return nil, &resolveError{output: text, unreachable: indexUnreachable(text)}
	}

	body2, err := os.ReadFile(reportFile)
	if err != nil {
		return nil, err
	}
	var report pipReport
	if err := json.Unmarshal(body2, &report); err != nil {
		return nil, err
	}

	resolved := make([]resolvedPackage, 0, len(report.Install))
	for _, item := range report.Install {
		resolved = append(resolved, resolvedPackage{
			Name:    item.Metadata.Name,
			Version: item.Metadata.Version,
			Direct:  item.Requested,
		})
	}
	return resolved, nil
}

// resolveDependencies returns what this requirements.txt adds on top of the
// platform's own stack.
//
// Resolved twice and subtracted, mirroring what a real build does: base
// packages are installed first and a baseline is taken before the
// department's requirements go in (installIntoVenv). Without the subtraction
// every request would also list starlette, anyio, click and the rest of
// fastapi's own tree — packages the platform already pulls in for every app,
// which no admin should be asked to approve and which the build never
// objects to.
//
// The base packages are included in the second resolution as well, not only
// subtracted afterwards: they constrain which versions are solvable, so
// resolving the department's requirements alone can report a tree that the
// real build would never produce.
func resolveDependencies(ctx context.Context, requirements string, basePackages []string) ([]resolvedPackage, error) {
	base := strings.Join(basePackages, "\n") + "\n"
	baseline, err := resolveSet(ctx, base)
	if err != nil {
		return nil, err
	}
	full, err := resolveSet(ctx, base+requirements)
	if err != nil {
		return nil, err
	}

	provided := make(map[string]bool, len(baseline))
	for _, pkg := range baseline {
		provided[normalizePackageName(pkg.Name)] = true
	}
	// Direct means "the department named it", which pip's own `requested`
	// flag cannot say here — the base packages are top-level in that file too.
	//
	// Names are read off the raw lines rather than through ParseRequirements:
	// that parser rejects an unpinned line entirely, and a line being badly
	// written does not make the package it names any less something the
	// department asked for.
	direct := namedPackages(requirements)

	var out []resolvedPackage
	for _, pkg := range full {
		key := normalizePackageName(pkg.Name)
		if provided[key] {
			continue
		}
		pkg.Direct = direct[key]
		out = append(out, pkg)
	}
	return out, nil
}

// packageRequests turns the resolution into the items an admin approves.
//
// Only what is not already allowed: repeating the platform's own stack under
// every request would bury the two names that actually need a decision.
func packageRequests(resolved []resolvedPackage, allowed []string) []PermissionRequest {
	allow := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allow[normalizePackageName(a)] = true
	}
	var out []PermissionRequest
	for _, pkg := range resolved {
		if allow[normalizePackageName(pkg.Name)] {
			continue
		}
		evidence := "requirements.txt 에 있으나 아직 승인되지 않았습니다"
		if !pkg.Direct {
			// Said plainly, because it is the thing an admin cannot work out
			// for themselves: this name is in no file the department wrote.
			evidence = "직접 요청한 패키지가 필요로 하는 의존성입니다 (requirements.txt 에는 없습니다)"
		}
		out = append(out, PermissionRequest{
			Kind:     PermKindPackage,
			Value:    pkg.Name,
			Label:    "패키지 추가",
			Detail:   pkg.Name + " (" + pkg.Version + ")",
			Evidence: evidence,
		})
	}
	return out
}

// namedPackages pulls the package names out of a requirements.txt, ignoring
// whether each line is well-formed. Shape is enforced elsewhere
// (company/requirements.go); this only needs to know who asked for what.
func namedPackages(requirements string) map[string]bool {
	out := map[string]bool{}
	for line := range strings.SplitSeq(requirements, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		name := strings.TrimSpace(line)
		if i := strings.IndexAny(name, "=<>!~[; "); i >= 0 {
			name = name[:i]
		}
		if name != "" {
			out[normalizePackageName(name)] = true
		}
	}
	return out
}

// resolveError separates "these versions cannot be installed" from "the
// package index could not be reached".
//
// The difference decides what happens next. A version conflict is the
// department's to fix and is worth stopping a request for. An unreachable
// index is the platform's problem, and refusing to accept requests while it
// lasts would punish the wrong people for it.
type resolveError struct {
	output      string
	unreachable bool
}

func (e *resolveError) Error() string { return e.output }

// indexUnreachable recognises a network failure in pip's own words. Erring
// towards "unreachable" on anything ambiguous: mistaking a real conflict for
// an outage costs a wasted approval, while mistaking an outage for a conflict
// tells a department their code is broken when it is not.
func indexUnreachable(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{
		"no matching distribution found",
		"could not find a version that satisfies",
		"resolutionimpossible",
		"conflict is caused by",
	} {
		if strings.Contains(lower, marker) {
			return false // pip resolved and said no
		}
	}
	return true
}

// resolveFailureSummary is the part of pip's output worth showing.
//
// pip prints its whole search on failure, ending with the sentence that
// matters. The tail is kept and the paths are stripped, because this reaches
// a department and the rest names the server's directories.
func resolveFailureSummary(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	kept := make([]string, 0, 3)
	for i := len(lines) - 1; i >= 0 && len(kept) < 3; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || isPipProgress(line) {
			continue
		}
		kept = append([]string{line}, kept...)
	}
	return RedactServerPaths(strings.Join(kept, "\n"))
}

// isPipProgress drops the lines pip prints while it works. They are most of
// the output and none of the answer, and keeping them pushes the sentence
// that explains the failure off the end of what is shown.
func isPipProgress(line string) bool {
	for _, prefix := range []string{
		"Collecting ", "Downloading ", "Using cached ", "Requirement already satisfied",
		"[notice]", "Obtaining ", "Installing ",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}
