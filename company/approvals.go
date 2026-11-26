// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"strings"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// Everything that waits for an administrator, in one table: a new project,
// and a deploy request. A deploy is still decided on its central pull
// request; this row follows it, so the central repository can later go
// without the approvals going with it. See docs/company/approvals.md.

const (
	ApprovalKindProject = "project"
	ApprovalKindDeploy  = "deploy"
)

type ApprovalStatus int

const (
	ApprovalPending ApprovalStatus = iota
	ApprovalApproved
	ApprovalRejected
	ApprovalCancelled
)

func (s ApprovalStatus) Key() string {
	switch s {
	case ApprovalApproved:
		return "company.approvals.status_approved"
	case ApprovalRejected:
		return "company.approvals.status_rejected"
	case ApprovalCancelled:
		return "company.approvals.status_cancelled"
	}
	return "company.approvals.status_pending"
}

type ApprovalRequest struct {
	ID          int64          `xorm:"pk autoincr"`
	Kind        string         `xorm:"VARCHAR(16) INDEX NOT NULL"`
	Status      ApprovalStatus `xorm:"INDEX NOT NULL DEFAULT 0"`
	RequesterID int64          `xorm:"INDEX NOT NULL"`
	// Name is the project asked for, or the app ("owner/repo") a deploy is for.
	Name        string `xorm:"NOT NULL"`
	LowerName   string `xorm:"INDEX NOT NULL"`
	Title       string
	Description string `xorm:"TEXT"`
	// RefID is the central pull request a deploy is decided on.
	RefID int64 `xorm:"INDEX"`
	// RepoID is the project an approval created.
	RepoID    int64
	DeciderID int64
	// AINote is what the platform AI said about a request it looked at.
	AINote      string             `xorm:"'ai_note' TEXT"`
	CreatedUnix timeutil.TimeStamp `xorm:"created"`
	DecidedUnix timeutil.TimeStamp

	Requester *user_model.User       `xorm:"-"`
	Decider   *user_model.User       `xorm:"-"`
	Repo      *repo_model.Repository `xorm:"-"`
	Link      string                 `xorm:"-"` // where a deploy is reviewed
}

func (ApprovalRequest) TableName() string { return "company_approval_request" }

func init() { db.RegisterModel(new(ApprovalRequest)) }

func (r *ApprovalRequest) IsPending() bool { return r.Status == ApprovalPending }
func (r *ApprovalRequest) IsProject() bool { return r.Kind == ApprovalKindProject }

func GetApprovalRequest(ctx context.Context, id int64) (*ApprovalRequest, error) {
	req := &ApprovalRequest{}
	has, err := db.GetEngine(ctx).ID(id).Get(req)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, db.ErrNotExist{Resource: "approval request", ID: id}
	}
	return req, nil
}

// decideApproval moves a pending request to status, and only a pending one:
// two administrators pressing at once decide it once.
func decideApproval(ctx context.Context, cond string, arg any, status ApprovalStatus, deciderID int64) (bool, error) {
	n, err := db.GetEngine(ctx).Where(cond+" AND status = ?", arg, ApprovalPending).Cols("status", "decider_id", "decided_unix").
		Update(&ApprovalRequest{Status: status, DeciderID: deciderID, DecidedUnix: timeutil.TimeStampNow()})
	return n > 0, err
}

func CountPendingApprovals(ctx context.Context) (int64, error) {
	return db.GetEngine(ctx).Where("status = ?", ApprovalPending).Count(new(ApprovalRequest))
}

// FindApprovals lists requests newest first; pending only, or every decided one.
func FindApprovals(ctx context.Context, pending bool, kind string, limit, offset int) ([]*ApprovalRequest, int64, error) {
	sess := func() *xorm.Session {
		op := "<>"
		if pending {
			op = "="
		}
		s := db.GetEngine(ctx).Where("status "+op+" ?", ApprovalPending)
		if kind != "" {
			s = s.And("kind = ?", kind)
		}
		return s
	}
	total, err := sess().Count(new(ApprovalRequest))
	if err != nil {
		return nil, 0, err
	}
	var reqs []*ApprovalRequest
	if err := sess().Desc("created_unix", "id").Limit(limit, offset).Find(&reqs); err != nil {
		return nil, 0, err
	}
	return reqs, total, loadApprovalDetails(ctx, reqs)
}

func loadApprovalDetails(ctx context.Context, reqs []*ApprovalRequest) error {
	ids := make([]int64, 0, 2*len(reqs))
	for _, r := range reqs {
		ids = append(ids, r.RequesterID, r.DeciderID)
	}
	users, err := user_model.GetUsersMapByIDs(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range reqs {
		r.Requester, r.Decider = users[r.RequesterID], users[r.DeciderID]
		if r.RepoID > 0 {
			r.Repo, _ = repo_model.GetRepositoryByID(ctx, r.RepoID)
		}
		if r.Kind == ApprovalKindDeploy && r.RefID > 0 {
			r.Link = deployReviewLink(ctx, r.RefID)
		}
	}
	return nil
}

func deployReviewLink(ctx context.Context, prID int64) string {
	pr, err := issues_model.GetPullRequestByID(ctx, prID)
	if err != nil {
		return ""
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return ""
	}
	return fmt.Sprintf("%s/pulls/%d", pr.BaseRepo.Link(), pr.Index)
}

// isDeployRequestPR is a deploy request on the central repository.
func isDeployRequestPR(ctx context.Context, pr *issues_model.PullRequest) (owner, repo string, requesterID int64, ok bool) {
	owner, repo, requesterID, ok = parseDeployBranchName(pr.HeadBranch)
	if !ok || pr.BaseRepoID != pr.HeadRepoID {
		return "", "", 0, false
	}
	if err := pr.LoadBaseRepo(ctx); err != nil || !isCentralRepo(pr.BaseRepo) {
		return "", "", 0, false
	}
	return owner, repo, requesterID, true
}

// recordDeployApproval adds the row for a deploy request just opened.
// Best-effort: the request exists either way, and startup fills a gap.
func recordDeployApproval(ctx context.Context, issue *issues_model.Issue) {
	if err := issue.LoadPullRequest(ctx); err != nil {
		log.Error("company: approval row for issue %d: %v", issue.ID, err)
		return
	}
	if err := upsertDeployApproval(ctx, issue.PullRequest); err != nil {
		log.Error("company: approval row for pull request %d: %v", issue.PullRequest.ID, err)
	}
}

// upsertDeployApproval makes the row for pr say what the pull request says.
func upsertDeployApproval(ctx context.Context, pr *issues_model.PullRequest) error {
	owner, repo, requesterID, ok := isDeployRequestPR(ctx, pr)
	if !ok {
		return nil
	}
	if err := pr.LoadIssue(ctx); err != nil {
		return err
	}
	status, deciderID, decidedAt := deployDecision(ctx, pr)
	row := &ApprovalRequest{}
	has, err := db.GetEngine(ctx).Where("kind = ? AND ref_id = ?", ApprovalKindDeploy, pr.ID).Get(row)
	if err != nil {
		return err
	}
	if has {
		if row.Status == status {
			return nil
		}
		_, err := db.GetEngine(ctx).ID(row.ID).Cols("status", "decider_id", "decided_unix").
			Update(&ApprovalRequest{Status: status, DeciderID: deciderID, DecidedUnix: decidedAt})
		return err
	}
	name := owner + "/" + repo
	_, err = db.GetEngine(ctx).NoAutoTime().Insert(&ApprovalRequest{ // dated by the pull request, also when filled in later
		Kind: ApprovalKindDeploy, Status: status, RequesterID: requesterID,
		Name: name, LowerName: strings.ToLower(name), Title: pr.Issue.Title,
		RefID: pr.ID, DeciderID: deciderID, DecidedUnix: decidedAt, CreatedUnix: pr.Issue.CreatedUnix,
	})
	return err
}

// deployDecision reads where a deploy request's pull request stands.
func deployDecision(ctx context.Context, pr *issues_model.PullRequest) (ApprovalStatus, int64, timeutil.TimeStamp) {
	switch {
	case pr.HasMerged:
		return ApprovalApproved, pr.MergerID, pr.MergedUnix
	case pr.Issue.IsClosed:
		if c := lastDecisionComment(ctx, pr.Issue.ID); c != nil {
			if strings.HasPrefix(c.Content, cancelCommentPrefix) {
				return ApprovalCancelled, c.PosterID, c.CreatedUnix
			}
			return ApprovalRejected, c.PosterID, c.CreatedUnix
		}
		return ApprovalRejected, 0, pr.Issue.ClosedUnix
	}
	return ApprovalPending, 0, 0
}

// lastDecisionComment is the comment that ended an unapproved request: the
// withdrawal if there was one, otherwise whoever closed it.
//
// The withdrawal wins where both exist — CancelDeployRequest writes its
// comment and then closes, so the close event is the machinery and the
// comment is the decision.
func lastDecisionComment(ctx context.Context, issueID int64) *issues_model.Comment {
	comments, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{
		IssueID: issueID,
		Type:    issues_model.CommentTypeUndefined,
	})
	if err != nil {
		log.Error("company: reading how deploy request issue %d ended: %v", issueID, err)
		return nil
	}
	var closed *issues_model.Comment
	for _, c := range comments {
		switch {
		case c.Type == issues_model.CommentTypeComment && strings.HasPrefix(c.Content, cancelCommentPrefix):
			return c
		case c.Type == issues_model.CommentTypeClose:
			closed = c
		}
	}
	return closed
}

// followDeployApproval keeps a deploy's row in step with its pull request:
// merged, closed, or reopened.
func followDeployApproval(ctx context.Context, pr *issues_model.PullRequest) {
	if err := upsertDeployApproval(ctx, pr); err != nil {
		log.Error("company: approval row for pull request %d: %v", pr.ID, err)
	}
}

// SyncDeployApprovals brings every deploy request on the central repository
// into the table, and corrects any row an event was missed for. Run at startup.
func SyncDeployApprovals(ctx context.Context) {
	central, err := centralRepoByName(ctx)
	if err != nil {
		return // no central repository: no deploy requests
	}
	var prs []*issues_model.PullRequest
	if err := db.GetEngine(ctx).Where("base_repo_id = ? AND head_repo_id = ? AND head_branch LIKE ?", central.ID, central.ID, "deploy/%").Find(&prs); err != nil {
		log.Error("company: syncing deploy approvals: %v", err)
		return
	}
	for _, pr := range prs {
		if err := upsertDeployApproval(ctx, pr); err != nil {
			log.Error("company: syncing deploy approval for pull request %d: %v", pr.ID, err)
		}
	}
}

func centralRepoByName(ctx context.Context) (*repo_model.Repository, error) {
	owner, name, err := centralDeployOwnerName()
	if err != nil {
		return nil, err
	}
	return repo_model.GetRepositoryByOwnerAndName(ctx, owner, name)
}

func approvalsLink() string { return setting.AppSubURL + companyPrefix + "/approvals" }
