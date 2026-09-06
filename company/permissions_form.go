// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"slices"
	"strings"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
	"gitea.dev/services/context"
)

// Glue between the Deploy Request screens and company/permissions.go.

// readRepoFile returns one file's content from a repository's default
// branch, or "" if it isn't there.
//
// Absence is the normal case — most apps have no requirements.txt — so this
// deliberately has no error return: a missing or unreadable file means
// "nothing to request", which is exactly what an empty string produces.
func readRepoFile(ctx *context.Context, repo *repo_model.Repository, path string) string {
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, repo)
	if err != nil {
		return ""
	}
	commit, err := gitRepo.GetBranchCommit(ctx, repo.DefaultBranch)
	if err != nil {
		return ""
	}
	entry, err := commit.GetTreeEntryByPath(ctx, gitRepo, path)
	if err != nil {
		return ""
	}
	blob := entry.Blob(gitRepo)
	// GetBlobBytes treats a non-positive limit as "read nothing", so the
	// blob's real size has to be passed rather than -1.
	content, err := blob.GetBlobBytes(ctx, blob.Size(ctx))
	if err != nil {
		return ""
	}
	return string(content)
}

// collectPermissionRequests builds the items a submitted Deploy Request
// carries, pairing each detected item with the reason the requester typed.
//
// The reason is required by the form rather than optional, because an
// operator who is not a developer cannot judge "allow outbound access to
// api.foo.com?" on its own — the request without a reason is not a request
// they can act on. See docs/company/app-platform.md.
func collectPermissionRequests(ctx *context.Context, repo *repo_model.Repository) []PermissionRequest {
	requirements := readRepoFile(ctx, repo, "requirements.txt")
	detected := DetectPermissionRequests(repo.OwnerName, repo.Name,
		requirements, ctx.FormString("access"))

	out := make([]PermissionRequest, 0, len(detected))
	for _, item := range detected {
		// Only items the requester actually ticked. Submitting everything the
		// platform noticed would put things in front of an admin that nobody
		// asked for.
		if ctx.FormString("perm_"+item.Kind+"_"+item.Value) == "" {
			continue
		}
		item.Reason = ctx.FormString("perm_reason")
		out = append(out, item)
	}
	out = append(out, requestedByHand(ctx, repo)...)
	return withDependencies(ctx, repo, requirements, out, ctx.FormString("perm_reason"))
}

// requestedByHand collects the items the platform cannot detect.
//
// Packages come from requirements.txt and a memory increase comes from the
// recorded times the limit was hit, so both can be proposed with evidence.
// Nothing in an app says "this needs to reach erp.internal" or "this needs to
// hand people a file" — those are intentions, and until they are asked for
// there is nowhere to say them. A department that needs one had no route to a
// request at all.
func requestedByHand(ctx *context.Context, repo *repo_model.Repository) []PermissionRequest {
	settings := SettingsFor(repo.OwnerName, repo.Name)
	reason := ctx.FormString("perm_reason")
	var out []PermissionRequest

	if ctx.FormString("perm_want_download") != "" && settings.Download.Policy != "allow" {
		out = append(out, PermissionRequest{
			Kind:  PermKindDownload,
			Value: "allow",
			Label: "파일 다운로드 허용",
			// Said as the consequence, because that is what an admin is
			// approving — see docs/company/app-platform.md on what this control
			// does and does not stop.
			Detail: "앱이 파일을 내려받게 할 수 있습니다 (현재는 차단됨)",
			Reason: reason,
		})
	}

	// One host per request. A form that took a list would need a syntax, and
	// the person filling it in is not a developer; a second host is a second
	// request, which is also a second decision for the admin.
	if host := strings.TrimSpace(ctx.FormString("perm_want_host")); host != "" {
		methods := strings.TrimSpace(ctx.FormString("perm_want_methods"))
		if methods == "" {
			methods = "GET"
		}
		out = append(out, PermissionRequest{
			Kind:   PermKindNetwork,
			Value:  host,
			Label:  "외부 통신 허용",
			Detail: host + " (" + strings.ToUpper(methods) + ")",
			// Whether the address is internal is the first thing an admin
			// checks, so it is stated rather than left to be recognised.
			Evidence: outboundEvidence(host),
			Reason:   reason,
		})
	}
	return out
}

// outboundEvidence says whether the destination looks internal.
//
// Not a security control — a name proves nothing — but an operator who is not
// a developer has no other way to tell "erp.internal" from "api.example.com",
// and that difference is most of the decision.
func outboundEvidence(host string) string {
	name := strings.ToLower(host)
	if i := strings.Index(name, "/"); i >= 0 {
		name = name[:i]
	}
	// Matched per label rather than as a suffix: the usual internal name is
	// erp.internal.company.com, where the marker is in the middle and a
	// suffix test would call it an internet address.
	labels := strings.Split(name, ".")
	if len(labels) == 1 {
		return "사내 주소로 보입니다 (도메인 없음)"
	}
	for _, label := range labels {
		switch label {
		case "internal", "local", "lan", "corp", "intranet":
			return "사내 주소로 보입니다"
		}
	}
	return "인터넷 주소로 보입니다 — 승인하면 이 앱이 사외로 데이터를 보낼 수 있습니다"
}

// withDependencies adds the packages the ticked ones will drag in.
//
// Done here rather than when the form is rendered because resolution talks to
// the package index and takes seconds: the employee ticking boxes should not
// wait for it, and the list that matters is the one the admin approves.
//
// Resolution runs only when a package was actually requested, and a failure
// is not fatal. If the index is unreachable the request still goes through
// carrying what the employee named, and the build reports the missing
// dependencies as it did before — degraded, but a department that cannot
// reach PyPI is not helped by also being unable to ask.
func withDependencies(ctx *context.Context, repo *repo_model.Repository, requirements string, out []PermissionRequest, reason string) []PermissionRequest {
	if !slices.ContainsFunc(out, func(r PermissionRequest) bool { return r.Kind == PermKindPackage }) {
		return out
	}
	settings := SettingsFor(repo.OwnerName, repo.Name)
	resolved, err := resolveDependencies(ctx, requirements, settings.BasePackages)
	if err != nil {
		log.Warn("company: %s: could not resolve dependencies; the request lists only the named packages: %v",
			repo.FullName(), err)
		return out
	}

	seen := make(map[string]bool, len(out))
	for _, r := range out {
		if r.Kind == PermKindPackage {
			seen[normalizePackageName(r.Value)] = true
		}
	}
	for _, item := range packageRequests(resolved, settings.AllowedPackages()) {
		if seen[normalizePackageName(item.Value)] {
			continue
		}
		// The requester's reason carries over: they did not name this package,
		// but it is here because of one they did, and an item with no reason
		// at all reads to an admin as though nobody asked for it.
		item.Reason = reason
		out = append(out, item)
	}
	return out
}

// SetDeployRequestPermissions attaches a Deploy Request's permission items to
// the admin's review screen, so the code diff and the permissions it asks for
// are judged together. "Why does this app suddenly need to reach
// erp.internal?" is only answerable next to the change that introduced it.
func SetDeployRequestPermissions(ctx *context.Context, owner, repo string, prID int64) {
	if requests := LoadPermissionRequests(owner, repo, prID); len(requests) > 0 {
		ctx.Data["PermissionRequests"] = requests
	}
}

// allowedOutboundHosts names what this app may already reach, so someone does
// not request access it already has.
func allowedOutboundHosts(settings AppSettings) []string {
	if settings.Network.Mode == NetworkOpen {
		return []string{"제한 없음"}
	}
	hosts := make([]string, 0, len(settings.Network.Allow))
	for _, rule := range settings.Network.Allow {
		hosts = append(hosts, rule.Host)
	}
	return hosts
}
