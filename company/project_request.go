// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
)

// A project somebody asked for exists only once an administrator approves it
// (company/approvals.go). See docs/company/projects.md.

var (
	errProjectRequestPending    = errors.New("a request for this project name is already waiting")
	errProjectRequestNotPending = errors.New("the request has already been decided")
)

// checkProjectName refuses a name that could not become a project now, so a
// request is not approved only to fail.
func checkProjectName(ctx context.Context, name string) error {
	if err := repo_model.IsUsableRepoName(name); err != nil {
		return err
	}
	org, err := ProjectOrg(ctx)
	if err != nil {
		return err
	}
	exists, err := repo_model.IsRepositoryModelExist(ctx, org.AsUser(), name)
	if err != nil {
		return err
	}
	if exists {
		return repo_model.ErrRepoAlreadyExist{Uname: org.Name, Name: name}
	}
	return nil
}

func RequestProject(ctx context.Context, doer *user_model.User, name, description string) (*ApprovalRequest, error) {
	if err := checkProjectName(ctx, name); err != nil {
		return nil, err
	}
	req := &ApprovalRequest{Kind: ApprovalKindProject, RequesterID: doer.ID, Name: name, LowerName: strings.ToLower(name), Description: description}
	return req, db.WithTx(ctx, func(ctx context.Context) error {
		waiting, err := db.GetEngine(ctx).Where("kind = ? AND lower_name = ? AND status = ?", ApprovalKindProject, req.LowerName, ApprovalPending).Exist(new(ApprovalRequest))
		if err != nil {
			return err
		}
		if waiting {
			return errProjectRequestPending
		}
		_, err = db.GetEngine(ctx).Insert(req)
		return err
	})
}

func getProjectRequest(ctx context.Context, id int64) (*ApprovalRequest, error) {
	req, err := GetApprovalRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	if req.Kind != ApprovalKindProject {
		return nil, db.ErrNotExist{Resource: "project request", ID: id}
	}
	return req, nil
}

func decideProjectRequest(ctx context.Context, id int64, status ApprovalStatus, deciderID int64) error {
	ok, err := decideApproval(ctx, "id = ?", id, status, deciderID)
	if err != nil {
		return err
	}
	if !ok {
		return errProjectRequestNotPending
	}
	return nil
}

// ApproveProjectRequest creates the project, owned by whoever asked for it.
func ApproveProjectRequest(ctx context.Context, doer *user_model.User, id int64) (*ApprovalRequest, *repo_model.Repository, error) {
	req, err := getProjectRequest(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	requester, err := user_model.GetUserByID(ctx, req.RequesterID)
	if err != nil {
		return req, nil, err
	}
	if err := decideProjectRequest(ctx, id, ApprovalApproved, doer.ID); err != nil {
		return req, nil, err
	}
	repo, err := CreateProject(ctx, requester, req.Name, req.Description)
	if err != nil {
		_, rerr := db.GetEngine(ctx).ID(id).Cols("status", "decider_id", "decided_unix").Update(&ApprovalRequest{Status: ApprovalPending})
		return req, nil, errors.Join(err, rerr)
	}
	if _, err := db.GetEngine(ctx).ID(id).Cols("repo_id").Update(&ApprovalRequest{RepoID: repo.ID}); err != nil {
		return req, repo, fmt.Errorf("ApproveProjectRequest: recording repo %d: %w", repo.ID, err)
	}
	return req, repo, nil
}

func RejectProjectRequest(ctx context.Context, doer *user_model.User, id int64) error {
	if _, err := getProjectRequest(ctx, id); err != nil {
		return err
	}
	return decideProjectRequest(ctx, id, ApprovalRejected, doer.ID)
}

// CancelProjectRequest withdraws a request, and only its own requester can.
func CancelProjectRequest(ctx context.Context, doer *user_model.User, id int64) error {
	req, err := getProjectRequest(ctx, id)
	if err != nil {
		return err
	}
	if req.RequesterID != doer.ID {
		return db.ErrNotExist{Resource: "project request", ID: id}
	}
	return decideProjectRequest(ctx, id, ApprovalCancelled, doer.ID)
}

// ProjectRequestsBy is what one person has asked for lately, newest first.
func ProjectRequestsBy(ctx context.Context, uid int64) ([]*ApprovalRequest, error) {
	var reqs []*ApprovalRequest
	if err := db.GetEngine(ctx).Where("kind = ? AND requester_id = ?", ApprovalKindProject, uid).Desc("id").Limit(20).Find(&reqs); err != nil {
		return nil, err
	}
	for _, r := range reqs {
		if r.RepoID > 0 {
			r.Repo, _ = repo_model.GetRepositoryByID(ctx, r.RepoID)
		}
	}
	return reqs, nil
}
