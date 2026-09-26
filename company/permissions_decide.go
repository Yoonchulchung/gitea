// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	gitea_context "gitea.dev/services/context"
	files_service "gitea.dev/services/repository/files"
)

// Deciding a request item by item, from the request itself.
//
// Both sides of the request use this one endpoint, because both are the same
// edit to the same file — the tick list in `.company/requests/<dept>/<app>.md`
// on the request's own branch, which is what the merge reads
// (company/permissions_commit.go). They differ only in what they may do to it:
//
//   - the administrator decides, so unticking means refused, and the refused
//     item stays on the record;
//   - the requester asks, so unticking means withdrawn, and the item goes —
//     there is no decision to record about something nobody was asked for.
//
// Neither can add an item. What is on a request came from the code being
// deployed or from the form that raised it, both of which carry the evidence
// an administrator judges it on; a line typed in afterwards has none. Asking
// for something else is a new request, which is also the only way the code
// and the permission stay reviewed together.
//
// See docs/company/permission-lifecycle.md.

// DeployRequestPermissions records which items of an open deploy request are
// to be approved.
func DeployRequestPermissions(ctx *gitea_context.Context) {
	pr, deptOwner, deptName, ok := decidableDeployRequest(ctx)
	if !ok {
		return
	}
	requests := LoadPermissionRequests(deptOwner, deptName, pr.ID)
	if len(requests) == 0 {
		ctx.NotFound(nil)
		return
	}

	keep := ctx.Req.Form["perm"]
	approved := make(map[string]bool, len(keep))
	for _, id := range keep {
		approved[id] = true
	}

	// The requester's unticks take the item off the request; the
	// administrator's refuse it in place.
	withdrawn := map[string]bool{}
	if !ctx.Doer.IsAdmin {
		for _, r := range requests {
			if id := permRequestID(r); !approved[id] {
				withdrawn[id] = true
			}
		}
	}

	back := decisionRedirect(ctx, pr, deptOwner)
	if err := commitRequestDecisions(ctx, pr, deptOwner, deptName, approved, withdrawn); err != nil {
		// Both sides press this, and a raw error carries the path it failed on
		// — inside Gitea's data directory (company/usererror.go). Only the
		// administrator, who can act on it, is told what it was.
		if ctx.Doer.IsAdmin {
			ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		} else {
			ctx.Flash.Error(DepartmentSafeErrorL(ctx.Locale, "changing the items of deploy request #"+strconv.FormatInt(pr.ID, 10), err))
		}
		ctx.Redirect(back)
		return
	}

	kept := make([]PermissionRequest, 0, len(requests))
	for _, r := range requests {
		id := permRequestID(r)
		if withdrawn[id] {
			continue
		}
		r.Decision = decisionReject
		if approved[id] {
			r.Decision = decisionApprove
		}
		kept = append(kept, r)
	}
	if err := SavePermissionRequests(deptOwner, deptName, pr.ID, kept); err != nil {
		// The file is what the merge reads, and it is already written, so the
		// decision holds. This record is what the department's own screens
		// show, and it is now behind.
		log.Error("company: recording the decisions on deploy request #%d: %v", pr.ID, err)
	}

	ctx.Flash.Success(ctx.Locale.TrString("company.review.perm_saved"))
	ctx.Redirect(back)
}

// decisionRedirect sends each side back where it came from: the request page
// for the administrator deciding on it, the department's own list for the
// requester, who cannot open the central repository's pull request at all
// (company/gate.go).
//
// The department name comes from the branch, not from the form that was
// submitted — a redirect built out of a posted field is a redirect somebody
// else can choose.
func decisionRedirect(ctx *gitea_context.Context, pr *issues_model.PullRequest, deptOwner string) string {
	if ctx.Req.FormValue("org") == "" {
		return pr.Issue.Link()
	}
	return fmt.Sprintf("%s/org/%s/dashboard/deploy-requests", setting.AppSubURL, url.PathEscape(deptOwner))
}

// decidableDeployRequest resolves the request in the URL and says whether
// this person may change what it asks for.
//
// The requester or an administrator, and only while it is open — the same
// rule as cancelling (company/deployrequests.go), for the same reason: after
// it is decided there is nothing to change, and changing it would rewrite
// what somebody approved.
func decidableDeployRequest(ctx *gitea_context.Context) (*issues_model.PullRequest, string, string, bool) {
	pr, err := issues_model.GetPullRequestByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		ctx.NotFound(err)
		return nil, "", "", false
	}
	if err := pr.LoadIssue(ctx); err != nil {
		ctx.ServerError("LoadIssue", err)
		return nil, "", "", false
	}
	deptOwner, deptName, ok := verifyDeployRequestPR(ctx, pr, false)
	if !ok {
		ctx.NotFound(nil)
		return nil, "", "", false
	}
	_, _, requesterID, ok := parseDeployBranchName(pr.HeadBranch)
	if !ok || (!ctx.Doer.IsAdmin && requesterID != ctx.Doer.ID) {
		ctx.NotFound(nil)
		return nil, "", "", false
	}
	if pr.Issue.IsClosed {
		ctx.HTTPError(http.StatusBadRequest, "already closed")
		return nil, "", "", false
	}
	return pr, deptOwner, deptName, true
}

// commitRequestDecisions writes the edited tick list onto the request's own
// branch, authored by whoever made the decision.
//
// On the branch, not on main: this is a change to what the request asks for,
// and the diff an administrator is reading is where it belongs. It is also
// why approving is two presses rather than one — a commit written to the head
// branch during the merge would race the merge's own view of that branch.
func commitRequestDecisions(ctx *gitea_context.Context, pr *issues_model.PullRequest, owner, repo string, approved, withdrawn map[string]bool) error {
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return err
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		return err
	}
	path := requestLogPath(owner, repo)
	body, found := readCentralFileAt(ctx, central, pr.HeadBranch, path)
	if !found {
		return userKeyError("company.err.request_log_missing")
	}
	updated, changed := rewriteRequestLogDecisions(body, approved, withdrawn)
	if !changed {
		return nil // nothing to say, and an empty commit would say it anyway
	}

	_, err = files_service.ChangeRepoFiles(ctx, central, ctx.Doer, &files_service.ChangeRepoFilesOptions{
		OldBranch: pr.HeadBranch,
		NewBranch: pr.HeadBranch,
		Message:   decisionCommitMessage(ctx.Doer.Name, len(approved), len(withdrawn), owner, repo, ctx.Doer.IsAdmin),
		Files: []*files_service.ChangeRepoFile{{
			Operation:     "update",
			TreePath:      path,
			ContentReader: strings.NewReader(updated),
		}},
	})
	return err
}

// decisionCommitMessage names what actually changed, because there is no
// second record of it: the commit is the record.
func decisionCommitMessage(actor string, approved, withdrawn int, owner, repo string, isAdmin bool) string {
	key := owner + "/" + repo
	if !isAdmin {
		return fmt.Sprintf("chore(requests): withdraw %d item(s) from %s\n\nWithdrawn by %s.", withdrawn, key, actor)
	}
	return fmt.Sprintf("chore(requests): approve %d item(s) for %s\n\nSelected by %s.", approved, key, actor)
}

// permissionRequestRows pairs each item with its id and current tick, for the
// review sidebar's checkboxes.
type permissionRequestRow struct {
	PermissionRequest
	ID       string
	Approved bool
}

// permissionRequestRows is what the page renders. Nothing is refused until
// somebody says so, so an item with no decision on it yet is ticked.
func permissionRequestRows(requests []PermissionRequest) []permissionRequestRow {
	rows := make([]permissionRequestRow, 0, len(requests))
	for _, r := range requests {
		rows = append(rows, permissionRequestRow{
			PermissionRequest: r,
			ID:                permRequestID(r),
			Approved:          r.Decision != decisionReject,
		})
	}
	return rows
}

// refusedItems is what a merged request did not grant, for the comment that
// tells the department without them having to ask.
func refusedItems(requests []PermissionRequest) []PermissionRequest {
	return slices.DeleteFunc(slices.Clone(requests), func(r PermissionRequest) bool {
		return r.Decision != decisionReject
	})
}
