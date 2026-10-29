// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"gitea.dev/models/db"
	org_model "gitea.dev/models/organization"
	"gitea.dev/models/perm"
	repo_model "gitea.dev/models/repo"
	system_model "gitea.dev/models/system"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/structs"
	"gitea.dev/modules/timeutil"
	repo_service "gitea.dev/services/repository"
)

// Every project lives in one organization, whatever department its people are
// in, and belongs to a person recorded beside it rather than to the namespace.
// So the URL of a project and of its app never changes with who owns it, and
// an owner who leaves the company leaves nothing to move: an administrator
// names somebody else. See docs/company/projects.md.

// settingKeyProjectOrgID follows the organization by id, so renaming it from
// its own settings page does not lose every project.
const settingKeyProjectOrgID = "company.project_org_id"

// ProjectOwner is who answers for a project. Access itself is Gitea's own
// collaboration — admin for the owner, write for members; this row says which
// collaborator is the owner.
type ProjectOwner struct {
	RepoID      int64              `xorm:"pk"`
	OwnerID     int64              `xorm:"INDEX NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated"`
}

func (ProjectOwner) TableName() string { return "company_project_owner" }

func init() { db.RegisterModel(new(ProjectOwner)) }

var (
	projectOrgMu sync.Mutex
	projectOrgID atomic.Int64
)

func projectOrgName() string {
	return setting.CfgProvider.Section("company").Key("PROJECT_ORG").MustString("projects")
}

// ProjectOrg is the organization every project lives in, created on first use.
func ProjectOrg(ctx context.Context) (*org_model.Organization, error) {
	if id := projectOrgID.Load(); id > 0 {
		if org, err := org_model.GetOrgByID(ctx, id); err == nil {
			return org, nil
		}
	}
	projectOrgMu.Lock()
	defer projectOrgMu.Unlock()

	_, all, err := system_model.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	if id, _ := strconv.ParseInt(all[settingKeyProjectOrgID], 10, 64); id > 0 {
		if org, err := org_model.GetOrgByID(ctx, id); err == nil {
			projectOrgID.Store(org.ID)
			return org, nil
		}
	}

	name := projectOrgName()
	org, err := org_model.GetOrgByName(ctx, name)
	switch {
	case err == nil:
	case org_model.IsErrOrgNotExist(err):
		owner, err := user_model.GetAdminUser(ctx)
		if err != nil {
			return nil, fmt.Errorf("ProjectOrg: no administrator to own %q: %w", name, err)
		}
		org = &org_model.Organization{Name: name, Visibility: structs.VisibleTypeLimited}
		if err := org_model.CreateOrganization(ctx, org, owner); err != nil {
			return nil, fmt.Errorf("ProjectOrg: creating %q: %w", name, err)
		}
		log.Info("company: created %s, where every project lives (owner: %s)", name, owner.Name)
	default:
		return nil, fmt.Errorf("ProjectOrg: %q: %w", name, err)
	}
	// Members of nothing reach a repository here as collaborators, and Gitea
	// hides every repository of an organization a visitor may not see.
	if org.Visibility == structs.VisibleTypePrivate {
		org.Visibility = structs.VisibleTypeLimited
		if err := user_model.UpdateUserCols(ctx, org.AsUser(), "visibility"); err != nil {
			return nil, err
		}
	}
	if err := system_model.SetSettings(ctx, map[string]string{settingKeyProjectOrgID: strconv.FormatInt(org.ID, 10)}); err != nil {
		return nil, err
	}
	projectOrgID.Store(org.ID)
	return org, nil
}

// IsProjectRepo reports whether repo is a project rather than the central
// deploy repository or something an administrator keeps elsewhere.
func IsProjectRepo(ctx context.Context, repo *repo_model.Repository) bool {
	org, err := ProjectOrg(ctx)
	return err == nil && repo.OwnerID == org.ID && !isCentralRepo(repo)
}

func isCentralRepo(repo *repo_model.Repository) bool {
	owner, name, err := centralDeployOwnerName()
	return err == nil && strings.EqualFold(repo.OwnerName, owner) && strings.EqualFold(repo.Name, name)
}

// ProjectOwnerID is the owner's user id, or 0 when nobody has been named.
func ProjectOwnerID(ctx context.Context, repoID int64) (int64, error) {
	row := &ProjectOwner{}
	has, err := db.GetEngine(ctx).ID(repoID).Get(row)
	if err != nil || !has {
		return 0, err
	}
	return row.OwnerID, nil
}

// ProjectOwnerOf is the owner, or nil when nobody is named or the account
// has been deleted since.
func ProjectOwnerOf(ctx context.Context, repoID int64) (*user_model.User, error) {
	id, err := ProjectOwnerID(ctx, repoID)
	if err != nil || id == 0 {
		return nil, err
	}
	u, err := user_model.GetUserByID(ctx, id)
	if user_model.IsErrUserNotExist(err) {
		err = nil
	}
	return u, err
}

var errNotRealUser = errors.New("only a person can own or join a project")

// SetProjectOwner names who answers for repo. The owner is the repository's
// admin — the one who may delete it once its app has left the platform
// (company/appremoval.go) — and the previous owner stays on as a plain member
// unless dropPrevious, so handing over never locks somebody out who is still
// working on it.
func SetProjectOwner(ctx context.Context, repo *repo_model.Repository, owner *user_model.User, dropPrevious bool) error {
	if !owner.IsIndividual() {
		return errNotRealUser
	}
	prevID, err := ProjectOwnerID(ctx, repo.ID)
	if err != nil {
		return err
	}
	if err := repo_service.AddOrUpdateCollaborator(ctx, repo, owner, perm.AccessModeAdmin); err != nil {
		return err
	}
	row := &ProjectOwner{RepoID: repo.ID, OwnerID: owner.ID}
	if prevID == 0 {
		if _, err := db.GetEngine(ctx).Insert(row); err != nil {
			return err
		}
	} else if _, err := db.GetEngine(ctx).ID(repo.ID).Cols("owner_id").Update(row); err != nil {
		return err
	}
	if prevID == 0 || prevID == owner.ID {
		return nil
	}
	prev, err := user_model.GetUserByID(ctx, prevID)
	if user_model.IsErrUserNotExist(err) {
		return nil // a deleted account has no access left to take
	} else if err != nil {
		return err
	}
	if dropPrevious {
		return repo_service.DeleteCollaboration(ctx, repo, prev)
	}
	return repo_service.AddOrUpdateCollaborator(ctx, repo, prev, perm.AccessModeWrite)
}

func deleteProjectOwner(ctx context.Context, repoID int64) error {
	_, err := db.GetEngine(ctx).Delete(&ProjectOwner{RepoID: repoID})
	return err
}

// ProjectMembers is everyone with access besides the owner.
func ProjectMembers(ctx context.Context, repo *repo_model.Repository) ([]*user_model.User, error) {
	ownerID, err := ProjectOwnerID(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	collaborators, _, err := repo_model.GetCollaborators(ctx, &repo_model.FindCollaborationOptions{RepoID: repo.ID})
	if err != nil {
		return nil, err
	}
	members := make([]*user_model.User, 0, len(collaborators))
	for _, c := range collaborators {
		if c.ID != ownerID {
			members = append(members, c.User)
		}
	}
	return members, nil
}

func addProjectMember(ctx context.Context, repo *repo_model.Repository, u *user_model.User) error {
	if !u.IsIndividual() {
		return errNotRealUser
	}
	if ownerID, err := ProjectOwnerID(ctx, repo.ID); err != nil || ownerID == u.ID {
		return err // the owner is already in, as more than a member
	}
	return repo_service.AddOrUpdateCollaborator(ctx, repo, u, perm.AccessModeWrite)
}

func removeProjectMember(ctx context.Context, repo *repo_model.Repository, u *user_model.User) error {
	return repo_service.DeleteCollaboration(ctx, repo, u)
}

// CanManageProject is the owner or an administrator.
func CanManageProject(ctx context.Context, doer *user_model.User, repo *repo_model.Repository) bool {
	if doer == nil {
		return false
	}
	if doer.IsAdmin {
		return true
	}
	ownerID, err := ProjectOwnerID(ctx, repo.ID)
	return err == nil && ownerID == doer.ID
}

// CreateProject creates a project owned by doer.
func CreateProject(ctx context.Context, doer *user_model.User, name, description string) (*repo_model.Repository, error) {
	org, err := ProjectOrg(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := repo_service.CreateRepository(ctx, doer, org.AsUser(), repo_service.CreateRepoOptions{
		Name:          name,
		Description:   description,
		Readme:        "Default",
		DefaultBranch: setting.Repository.DefaultBranch,
		AutoInit:      true,
		IsPrivate:     true,
	})
	if err != nil {
		return nil, err
	}
	if err := SetProjectOwner(ctx, repo, doer, false); err != nil {
		return nil, err
	}
	return repo, nil
}

// repoFollowingMoves resolves owner/name the way it was written down, even if
// the repository was renamed or moved since — a deploy request's branch keeps
// the name it was opened under.
func repoFollowingMoves(ctx context.Context, owner, name string) (*repo_model.Repository, error) {
	repo, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, name)
	if !repo_model.IsErrRepoNotExist(err) {
		return repo, err
	}
	u, uerr := user_model.GetUserByName(ctx, owner)
	if uerr != nil {
		return nil, err
	}
	id, rerr := repo_model.LookupRedirect(ctx, u.ID, name)
	if rerr != nil {
		return nil, err
	}
	return repo_model.GetRepositoryByID(ctx, id)
}
