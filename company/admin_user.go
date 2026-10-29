// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"

	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	gitea_context "gitea.dev/services/context"
)

// The company half of the admin's user page: the projects this person owns,
// with a way to hand all of them to somebody else in one step — which is what
// somebody leaving needs — and what that work occupies.

// AdminUserPanel fills in the company half of the admin's user page.
//
// Called with one line from routers/web/admin/users.go (docs/company/patches.md);
// everything it needs is already in ctx.Data, so it adds no parameters to a
// core signature.
func AdminUserPanel(ctx *gitea_context.Context) {
	u, ok := ctx.Data["User"].(*user_model.User)
	if !ok || u == nil {
		return
	}
	projects, err := projectsOwnedBy(ctx, u.ID)
	if err != nil {
		log.Error("company: listing the projects of %s: %v", u.Name, err)
		return
	}

	// Where this account came from. On a directory-backed instance the answer
	// changes what an administrator may do with it: a local account is one
	// this platform owns and can reset, an LDAP one is a mirror of a record
	// that lives somewhere else, and treating the second like the first is
	// how somebody ends up "fixed" here and unchanged where it counts.
	ctx.Data["CompanyAccountSource"] = accountSource(ctx, u)
	ctx.Data["CompanyOwnedProjects"] = projects
	ctx.Data["CompanyUserStats"] = userStorage(ctx, projects)
}

// projectsOwnedBy lists the projects ownerID answers for, by name.
func projectsOwnedBy(ctx context.Context, ownerID int64) ([]*repo_model.Repository, error) {
	var repos []*repo_model.Repository
	err := db.GetEngine(ctx).
		Join("INNER", "company_project_owner", "company_project_owner.repo_id = repository.id").
		Where("company_project_owner.owner_id = ?", ownerID).
		Asc("repository.lower_name").Find(&repos)
	return repos, err
}

// UserStorage is the storage half of the page — what this person's projects
// actually occupy, which is the question an administrator sizing a disk is
// asking and had no screen to ask it on.
type UserStorage struct {
	Repos      int
	RepoBytes  int64
	AppBytes   int64
	AppQuota   int64
	AppPercent int
}

func userStorage(ctx *gitea_context.Context, projects []*repo_model.Repository) UserStorage {
	stats := UserStorage{Repos: len(projects)}
	for _, repo := range projects {
		stats.RepoBytes += repo.Size
		if !AppDataEnabled() {
			continue
		}
		// Only repositories that are actually apps. Counting a quota for
		// every plain repository would dilute the percentage with allowances
		// nothing can ever spend.
		if _, live := AppDataDirIfPresent(ctx, repo.OwnerName, repo.Name); !live {
			continue
		}
		if usage, ok := AppDataUsageForRepoID(repo.OwnerName, repo.Name, repo.ID); ok {
			stats.AppBytes += usage.Bytes
			stats.AppQuota += usage.QuotaBytes
		}
	}
	if stats.AppQuota > 0 {
		stats.AppPercent = int(stats.AppBytes * 100 / stats.AppQuota)
	}
	return stats
}

// AccountSource says where an account's credentials actually live.
type AccountSource struct {
	// External is true when the directory owns this account. It is the field
	// every decision keys on; Label is only for reading.
	External bool
	Label    string // "LDAP (via BindDN)", "OAuth2", "로컬" …
	Name     string // the configured source's own name, when there is one
}

// accountSource resolves it. A user whose login source has since been
// deleted still reports as external — the account was provisioned, and that
// does not stop being true because the source row went away.
func accountSource(ctx *gitea_context.Context, u *user_model.User) AccountSource {
	if u.LoginType <= auth_model.Plain {
		return AccountSource{Label: "local"}
	}
	src := AccountSource{External: true, Label: u.LoginType.String()}
	if source, err := auth_model.GetSourceByID(ctx, u.LoginSource); err == nil {
		src.Name = source.Name
	}
	return src
}
