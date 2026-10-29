// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"errors"
	"fmt"

	activities_model "gitea.dev/models/activities"
	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	repo_service "gitea.dev/services/repository"
)

// The repositories departments made before projects existed are moved into
// the projects organization as they are: same name, same history, same app
// and data, which follow the move (company/apprename.go). Everyone who could
// reach one through its department becomes a member, so nobody loses access,
// and whoever created it becomes its owner.

// ProjectMigration is one department repository and what moving it means.
type ProjectMigration struct {
	Repo       *repo_model.Repository
	Department *user_model.User
	Owner      *user_model.User // nil leaves the owner for an administrator to name
	Members    int
	// Blocked is why it cannot move yet, as a locale key; empty when it can.
	Blocked string
}

// PlanProjectMigration lists every repository still owned by a department.
func PlanProjectMigration(ctx context.Context) ([]*ProjectMigration, error) {
	org, err := ProjectOrg(ctx)
	if err != nil {
		return nil, err
	}
	var repos []*repo_model.Repository
	if err := db.GetEngine(ctx).
		Where("owner_id <> ?", org.ID).
		And("owner_id IN (SELECT id FROM `user` WHERE type = ?)", user_model.UserTypeOrganization).
		Asc("owner_name", "lower_name").Find(&repos); err != nil {
		return nil, err
	}
	central, _ := centralRepoForMigration(ctx)

	plan := make([]*ProjectMigration, 0, len(repos))
	for _, repo := range repos {
		if isCentralRepo(repo) {
			continue
		}
		item, err := planProjectMigration(ctx, org.AsUser(), central, repo)
		if err != nil {
			return nil, err
		}
		plan = append(plan, item)
	}
	return plan, nil
}

func planProjectMigration(ctx context.Context, projects *user_model.User, central, repo *repo_model.Repository) (*ProjectMigration, error) {
	if err := repo.LoadOwner(ctx); err != nil {
		return nil, err
	}
	members, err := departmentMemberIDs(ctx, repo.OwnerID)
	if err != nil {
		return nil, err
	}
	item := &ProjectMigration{
		Repo:       repo,
		Department: repo.Owner,
		Owner:      guessProjectOwner(ctx, repo, central),
		Members:    len(members),
	}
	taken, err := repo_model.IsRepositoryModelExist(ctx, projects, repo.Name)
	switch {
	case err != nil:
		return nil, err
	case repo.Status != repo_model.RepositoryReady:
		item.Blocked = "company.projects.migrate_busy"
	case taken:
		item.Blocked = "company.projects.migrate_name_taken"
	case central != nil && hasOpenDeployRequest(ctx, central, repo.OwnerName, repo.Name):
		// Its snapshot names the department; merged after the move it
		// would deploy to an app that is no longer there.
		item.Blocked = "company.projects.migrate_open_request"
	}
	return item, nil
}

// MigrateRepoToProject moves one department repository, refusing what the
// plan would leave where it is.
func MigrateRepoToProject(ctx context.Context, doer *user_model.User, repo *repo_model.Repository) error {
	org, err := ProjectOrg(ctx)
	if err != nil {
		return err
	}
	if repo.OwnerID == org.ID || isCentralRepo(repo) {
		return fmt.Errorf("MigrateRepoToProject: %s is not a department repository", repo.FullName())
	}
	central, _ := centralRepoForMigration(ctx)
	item, err := planProjectMigration(ctx, org.AsUser(), central, repo)
	if err != nil {
		return err
	}
	if item.Blocked != "" {
		return fmt.Errorf("MigrateRepoToProject: %s cannot move yet (%s)", repo.FullName(), item.Blocked)
	}
	return migrateToProject(ctx, doer, org.AsUser(), item)
}

// RunProjectMigration moves every repository the plan does not block.
func RunProjectMigration(ctx context.Context, doer *user_model.User) (moved, skipped int, err error) {
	plan, err := PlanProjectMigration(ctx)
	if err != nil {
		return 0, 0, err
	}
	org, err := ProjectOrg(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, item := range plan {
		if item.Blocked != "" {
			skipped++
			continue
		}
		if err := migrateToProject(ctx, doer, org.AsUser(), item); err != nil {
			return moved, skipped, fmt.Errorf("RunProjectMigration: %s: %w", item.Repo.FullName(), err)
		}
		moved++
	}
	return moved, skipped, nil
}

func migrateToProject(ctx context.Context, doer, projects *user_model.User, item *ProjectMigration) error {
	from := item.Repo.FullName()
	members, err := departmentMemberIDs(ctx, item.Repo.OwnerID)
	if err != nil {
		return err
	}
	if err := repo_service.StartRepositoryTransfer(ctx, doer, projects, item.Repo, nil); err != nil {
		return err
	}
	repo, err := repo_model.GetRepositoryByID(ctx, item.Repo.ID)
	if err != nil {
		return err
	}
	users, err := user_model.GetUsersByIDs(ctx, members)
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := addProjectMember(ctx, repo, u); err != nil && !errors.Is(err, errNotRealUser) {
			return err
		}
	}
	if item.Owner != nil {
		if err := SetProjectOwner(ctx, repo, item.Owner, false); err != nil {
			return err
		}
	}
	log.Info("company: %s moved %s to %s (%d members, owner: %v)", doer.Name, from, repo.FullName(), len(users), item.Owner != nil)
	return nil
}

// departmentMemberIDs is everyone in the organization but administrators,
// who reach every project anyway.
func departmentMemberIDs(ctx context.Context, orgID int64) ([]int64, error) {
	var ids []int64
	err := db.GetEngine(ctx).Table("org_user").
		Join("INNER", "`user`", "`user`.id = org_user.uid").
		Where("org_user.org_id = ? AND `user`.is_admin = ?", orgID, false).
		Cols("org_user.uid").Find(&ids)
	return ids, err
}

// guessProjectOwner is whoever created the repository, or, when that was an
// administrator setting it up for a department, whoever last asked to deploy
// it. Nil when neither is a person who can still sign in.
func guessProjectOwner(ctx context.Context, repo, central *repo_model.Repository) *user_model.User {
	usable := func(u *user_model.User) bool {
		return u != nil && u.IsIndividual() && u.IsActive && !u.ProhibitLogin && !u.IsAdmin
	}
	var created activities_model.Action
	if has, err := db.GetEngine(ctx).Where("repo_id = ? AND op_type = ?", repo.ID, activities_model.ActionCreateRepo).
		Asc("id").Get(&created); err == nil && has {
		if u, err := user_model.GetUserByID(ctx, created.ActUserID); err == nil && usable(u) {
			return u
		}
	}
	if central == nil {
		return nil
	}
	var pr issues_model.PullRequest
	if has, err := db.GetEngine(ctx).
		Where("base_repo_id = ? AND head_repo_id = ?", central.ID, central.ID).
		And("head_branch LIKE ?", deployBranchPrefix(repo.OwnerName, repo.Name)+"%").
		Desc("id").Get(&pr); err == nil && has {
		if _, _, requesterID, ok := parseDeployBranchName(pr.HeadBranch); ok {
			if u, err := user_model.GetUserByID(ctx, requesterID); err == nil && usable(u) {
				return u
			}
		}
	}
	return nil
}

func hasOpenDeployRequest(ctx context.Context, central *repo_model.Repository, owner, name string) bool {
	has, err := db.GetEngine(ctx).
		Join("INNER", "issue", "issue.id = pull_request.issue_id").
		Where("pull_request.base_repo_id = ? AND pull_request.head_repo_id = ?", central.ID, central.ID).
		And("pull_request.head_branch LIKE ?", deployBranchPrefix(owner, name)+"%").
		And("issue.is_closed = ?", false).
		Exist(new(issues_model.PullRequest))
	return err != nil || has // unknown is not "nothing open"
}

func centralRepoForMigration(ctx context.Context) (*repo_model.Repository, error) {
	owner, name, err := centralDeployOwnerName()
	if err != nil {
		return nil, err
	}
	return repo_model.GetRepositoryByOwnerAndName(ctx, owner, name)
}
