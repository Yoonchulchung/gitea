// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"slices"

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
	return withDependencies(ctx, repo, requirements, out, ctx.FormString("perm_reason"))
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
