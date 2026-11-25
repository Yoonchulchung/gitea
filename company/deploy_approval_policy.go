// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"errors"
	"fmt"

	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
	"gitea.dev/modules/process"
	issue_service "gitea.dev/services/issue"
	pull_service "gitea.dev/services/pull"
)

// An app's first deploy always waits for an administrator. Once the app is
// on central, later deploys are merged right away unless an administrator
// asks for approval of every deploy, or the request asks for something whose
// approval is still switched on.
const (
	// The administrator in whose name later deploys are merged; empty means every deploy is approved by a person.
	settingKeyApprovalWaivedBy = "company.deploy.approval_waived_by"
	settingKeyPackageApproval  = "company.deploy.package_approval"
	settingKeyNetworkApproval  = "company.deploy.network_approval"
)

// noApprovalComment carries the AI comment marker so the department's "why was this rejected" list skips it.
func noApprovalComment(title, body string) string {
	return aiReviewCommentMarker + "\n#### 배포 승인 정책 · " + title + "\n\n" + body
}

type deployApprovalPolicy struct {
	// WaivedBy is nil when every deploy needs an administrator.
	WaivedBy        *user_model.User
	PackageApproval bool
	NetworkApproval bool
}

// currentDeployApprovalPolicy is the policy in force now. An account that stopped
// being an active administrator takes the waiver with it.
func currentDeployApprovalPolicy(ctx context.Context) deployApprovalPolicy {
	loadPlatformSettings(ctx)
	platformSettingsMu.RLock()
	p := deployApprovalPolicy{PackageApproval: packageApproval, NetworkApproval: networkApproval}
	id := approvalWaivedBy
	platformSettingsMu.RUnlock()
	if id != 0 {
		if u, err := user_model.GetUserByID(ctx, id); err == nil && u.IsAdmin && u.IsActive && !u.ProhibitLogin {
			p.WaivedBy = u
		}
	}
	return p
}

// needsApproval reports whether a deploy request has to wait for an
// administrator. Kinds other than packages and outbound hosts widen what an
// app may do in ways this policy was never asked to waive.
func (p deployApprovalPolicy) needsApproval(deployedBefore bool, requests []PermissionRequest) bool {
	if p.WaivedBy == nil || !deployedBefore {
		return true
	}
	for _, r := range requests {
		switch r.Kind {
		case PermKindPackage:
			if p.PackageApproval {
				return true
			}
		case PermKindNetwork:
			if p.NetworkApproval {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// ScheduleDeployWithoutApproval merges a deploy request in the background,
// once Gitea's conflict check has finished. A request that cannot be merged
// stays open for an administrator, with the reason on it.
func ScheduleDeployWithoutApproval(central *repo_model.Repository, merger *user_model.User, prID int64, deptOwner, deptName string) {
	go func() {
		ctx, _, finished := process.GetManager().AddContext(graceful.GetManager().ShutdownContext(), fmt.Sprintf("company: deploy request %d without approval", prID))
		defer finished()
		defer recoverBackground("deploy request %d without approval", prID)
		pr, err := mergeWithoutApproval(ctx, central, merger, prID)
		if err == nil {
			log.Info("company: deploy request #%d for %s/%s merged without approval on behalf of %s", prID, deptOwner, deptName, merger.Name)
			return
		}
		log.Error("company: deploy request #%d for %s/%s without approval: %v", prID, deptOwner, deptName, err)
		if pr != nil && pr.Issue != nil && !pr.HasMerged && !pr.Issue.IsClosed {
			body := noApprovalComment("자동 배포 보류", "자동으로 배포하지 못했습니다: "+err.Error()+"\n\n관리자가 직접 승인해 주세요.")
			if _, err := issue_service.CreateIssueComment(ctx, merger, central, pr.Issue, body, nil); err != nil {
				log.Error("company: deploy request #%d without approval: post comment: %v", prID, err)
			}
		}
	}()
}

func mergeWithoutApproval(ctx context.Context, central *repo_model.Repository, merger *user_model.User, prID int64) (*issues_model.PullRequest, error) {
	pr, err := waitForMergeCheck(ctx, prID)
	if err != nil {
		return nil, err
	}
	gitRepo, err := git.OpenRepository(ctx, central)
	if err != nil {
		return pr, fmt.Errorf("mergeWithoutApproval: %w", err)
	}
	defer gitRepo.Close()
	head, err := gitRepo.GetBranchCommit(ctx, pr.HeadBranch)
	if err != nil {
		return pr, fmt.Errorf("mergeWithoutApproval: %w", err)
	}
	perm, err := access_model.GetDoerRepoPermission(ctx, central, merger)
	if err != nil {
		return pr, fmt.Errorf("mergeWithoutApproval: %w", err)
	}
	if err := pull_service.CheckPullMergeable(ctx, merger, &perm, pr, pull_service.MergeCheckTypeGeneral, repo_model.MergeStyleMerge, false); err != nil {
		if errors.Is(err, pull_service.ErrIsClosed) || errors.Is(err, pull_service.ErrHasMerged) {
			return pr, nil // decided by somebody else meanwhile
		}
		return pr, err
	}
	_, body, err := pull_service.GetDefaultMergeMessage(ctx, gitRepo, pr, repo_model.MergeStyleMerge)
	if err != nil {
		return pr, fmt.Errorf("mergeWithoutApproval: %w", err)
	}
	comment := noApprovalComment("자동 배포", "이미 승인된 적 있는 앱이고 배포마다 승인이 꺼져 있어 바로 배포합니다.\n\n@"+merger.Name+" 님이 배포마다 승인을 껐습니다.")
	if _, err := issue_service.CreateIssueComment(ctx, merger, central, pr.Issue, comment, nil); err != nil {
		log.Error("company: deploy request #%d without approval: post comment: %v", prID, err)
	}
	message := pr.Issue.Title + "\n\n승인자: 자동 배포 (배포마다 승인 꺼짐, 위임: " + merger.Name + ")\n" + body
	// head pins the merge to the submitted snapshot; a newer push fails it.
	return pr, pull_service.Merge(ctx, pr.ID, merger, repo_model.MergeStyleMerge, head.ID.String(), message, false)
}
