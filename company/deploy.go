// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
	git_service "gitea.dev/services/git"
	issue_service "gitea.dev/services/issue"
	pull_svc "gitea.dev/services/pull"
	files_service "gitea.dev/services/repository/files"
)

const (
	tplDeployForm templates.TplName = "company/deploy"
	tplSubmitted  templates.TplName = "company/submitted"
)

// centralDeployRepo loads the one company-wide deployment target
// configured in app.ini's [company] section. One repo for the whole
// company, not per-department, so this is a single static config value
// rather than a new admin settings page/DB table — see the "Deploy
// Request" design note in docs/company/architecture.md.
func centralDeployRepo(ctx *context.Context) (*repo_model.Repository, error) {
	ownerRepo := setting.CfgProvider.Section("company").Key("CENTRAL_DEPLOY_REPO").String()
	owner, name, ok := strings.Cut(ownerRepo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("[company] CENTRAL_DEPLOY_REPO must be set to an \"owner/name\" repo path")
	}
	return repo_model.GetRepositoryByOwnerAndName(ctx, owner, name)
}

// deployBranchPrefix is the namespace every deploy request's files (and
// the branch that carries them) live under in the central repo — "PO" and
// "vv" both writing a top-level README.md would otherwise collide the
// moment two departments' deploy requests landed near each other; see
// docs/company/architecture.md.
func deployPathPrefix(owner, name string) string {
	return owner + "/" + name
}

// deployBranchName encodes which department repo a central-deploy branch
// (and the PR built from it) belongs to, since the PR itself is now
// same-repo (head=central, base=central) — there's no separate HeadRepoID
// to key off like a cross-repo PR would give for free. It also carries the
// real requester's user ID: the PR/issue itself is authored as the central
// repo's own owner (see DeployPost) since the requester normally has no
// access to central at all, so the branch name is the only place left to
// remember who actually asked. "/"-separated (not "-") specifically so
// parsing it back out (branch names may not contain "/" as their own char,
// but owner/repo names can't contain "/" either) is never ambiguous, unlike
// gluing owner-name-id-timestamp together with hyphens would be for
// repo/org names that themselves contain one.
func deployBranchName(owner, name string, requesterID int64) string {
	return fmt.Sprintf("%s%d/%d", deployBranchPrefix(owner, name), requesterID, time.Now().Unix())
}

// deployBranchPrefix is deployBranchName without the requester ID/timestamp
// suffix — what every deploy branch for this department repo starts with,
// for a SQL LIKE match (company/deploystatus.go's latestDeployRequest,
// company/deployrequests.go).
func deployBranchPrefix(owner, name string) string {
	return fmt.Sprintf("deploy/%s/%s/", owner, name)
}

// parseDeployBranchName is deployBranchName's inverse — used to find "this
// department repo's deploy requests" among central-deploy's PRs
// (company/deployrequests.go, company/deploystatus.go, company/deployrequestfiles.go,
// Submitted below) without a HeadRepoID to filter on, and to recover who
// actually requested one (the DB's own Issue.Poster is the central repo's
// owner, not the requester — see deployBranchName above). Returns ok=false
// for anything not shaped like a deploy branch.
func parseDeployBranchName(branch string) (owner, name string, requesterID int64, ok bool) {
	parts := strings.Split(branch, "/")
	if len(parts) != 5 || parts[0] != "deploy" {
		return "", "", 0, false
	}
	requesterID, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return "", "", 0, false
	}
	return parts[1], parts[2], requesterID, true
}

// DeployForm renders the "Deploy Request" message form for the repo in
// the URL — mounted at /{owner}/{repo}/deploy alongside Gitea's own repo
// routes (routers/web/web.go), so ctx.Repo.Repository/.Permission are
// already resolved by the time this runs. Also lists every file that will
// actually be uploaded (DeployPost snapshots the whole default branch, not
// just recently-changed files — see snapshotFilesUnderPrefix), so staff can
// see what they're sending before they click through.
func DeployForm(ctx *context.Context) {
	if !ctx.Repo.Permission.CanRead(unit.TypeCode) {
		ctx.NotFound(nil)
		return
	}
	files, err := defaultBranchFilePaths(ctx, ctx.Repo.Repository)
	if err != nil {
		ctx.ServerError("defaultBranchFilePaths", err)
		return
	}
	ctx.Data["Title"] = "Deploy Request"
	ctx.Data["Repo"] = ctx.Repo.Repository
	ctx.Data["DeployFiles"] = files
	ctx.HTML(http.StatusOK, tplDeployForm)
}

// defaultBranchFilePaths lists every non-directory, non-submodule path in
// repo's default branch, sorted — the same tree snapshotFilesUnderPrefix
// walks to build the upload list, minus reading each blob's content (the
// form only needs to show paths).
func defaultBranchFilePaths(ctx *context.Context, repo *repo_model.Repository) ([]string, error) {
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, repo)
	if err != nil {
		return nil, err
	}
	commit, err := gitRepo.GetBranchCommit(ctx, repo.DefaultBranch)
	if err != nil {
		return nil, err
	}
	tree, err := commit.SubTree(ctx, gitRepo, "/")
	if err != nil {
		return nil, err
	}
	entries, err := tree.ListEntriesRecursiveFast(ctx, gitRepo)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.IsSubModule() {
			continue
		}
		paths = append(paths, entry.Name())
	}
	sort.Strings(paths)
	return paths, nil
}

// DeployPost snapshots this department repo's current default branch into
// the central deploy repo, namespaced under <owner>/<repo>/ (so two
// departments' deploy requests never collide there — see deployPathPrefix),
// as a new commit on a fresh central-deploy branch, then opens a same-repo
// PR (that branch -> central's default branch). Department staff already
// write directly to their own repo's main (custom/templates/repo/editor/
// commit_form.tmpl's "direct" commit_choice), so by the time anyone clicks
// "Deploy Request" the department repo already holds exactly what should
// ship; this just brings a copy of it into the central repo as a PR the
// admin reviews natively. See docs/company/architecture.md.
//
// The requester (ctx.Doer) is deliberately never the one writing to or
// opening the PR on the central repo — by design (docs/company/architecture.md
// on cross-department privacy) they normally have no access to it at all,
// same as everyone outside admin. Both the commit and the PR are made as
// the central repo's own owner instead; deployBranchName carries the real
// requester's ID so the rest of this package can still show/authorize
// against them without granting any real access.
func DeployPost(ctx *context.Context) {
	if !ctx.Repo.Permission.CanRead(unit.TypeCode) {
		ctx.NotFound(nil)
		return
	}
	deptRepo := ctx.Repo.Repository

	message := strings.TrimSpace(ctx.Req.FormValue("message"))
	if message == "" {
		ctx.HTTPError(http.StatusBadRequest, "message required")
		return
	}

	central, err := centralDeployRepo(ctx)
	if err != nil {
		ctx.ServerError("centralDeployRepo", err)
		return
	}
	centralOwner, err := user_model.GetUserByID(ctx, central.OwnerID)
	if err != nil {
		ctx.ServerError("GetUserByID", err)
		return
	}

	files, err := snapshotFilesUnderPrefix(ctx, deptRepo, deployPathPrefix(deptRepo.OwnerName, deptRepo.Name))
	if err != nil {
		ctx.ServerError("snapshotFilesUnderPrefix", err)
		return
	}
	if len(files) == 0 {
		ctx.HTTPError(http.StatusBadRequest, "repo has no files to deploy")
		return
	}

	newBranch := deployBranchName(deptRepo.OwnerName, deptRepo.Name, ctx.Doer.ID)
	if _, err := files_service.ChangeRepoFiles(ctx, central, centralOwner, &files_service.ChangeRepoFilesOptions{
		OldBranch: central.DefaultBranch,
		NewBranch: newBranch,
		Message:   message,
		Files:     files,
	}); err != nil {
		ctx.ServerError("ChangeRepoFiles", err)
		return
	}

	content := fmt.Sprintf("Requested by @%s (%s).\n\n%s", ctx.Doer.Name, deptRepo.FullName(), message)
	pullIssue, err := openPullRequest(ctx, central, central, newBranch, message, content, centralOwner)
	if err != nil {
		ctx.ServerError("openPullRequest", err)
		return
	}

	// Best-effort: an admin still reviews and approves every Deploy
	// Request regardless, so a slow/failing/unconfigured AI review must
	// never block or fail the actual submission — see postAIReviewComment.
	postAIReviewComment(ctx, central, centralOwner, pullIssue, newBranch, deptRepo, message)

	ctx.Redirect(deptRepo.Link() + "/deploy/submit")
}

// aiReviewDiffMaxChars caps how much raw diff text gets sent to the AI
// tool per review — a very large deploy (a full initial import, say) could
// otherwise blow well past the model's context window; a truncated diff
// still gives a useful review of what it does show, flagged as partial in
// the prompt itself.
const aiReviewDiffMaxChars = 40000

const aiReviewSystemPrompt = `You are reviewing a pull request for an internal deploy pipeline: a department's files being brought into a shared repo an admin will approve before it ships. Reply in Korean, briefly (a few sentences to a short bullet list) — call out anything that looks like a real problem (secrets/credentials committed, obviously broken code, anything odd for what the request claims to be), or say plainly that nothing stood out. This is advisory context for the human reviewer, not a blocker — don't refuse to review, and don't recommend it, just report what you see.`

// postAIReviewComment posts an AI-generated review as a plain comment on
// the deploy-request PR, authored as centralOwner (the admin who owns the
// central repo, and the same identity DeployPost already uses for the
// commit/PR itself — see its own doc comment on why the real requester
// isn't used here). Best-effort only: unconfigured AI, an API error, or
// anything else here is logged and swallowed, never surfaced to the
// employee submitting the request — the admin still reviews and approves
// every deploy request themselves regardless of what (if anything) the AI
// said.
func postAIReviewComment(ctx *context.Context, central *repo_model.Repository, centralOwner *user_model.User, pullIssue *issues_model.Issue, headBranch string, deptRepo *repo_model.Repository, message string) {
	if !AIConfiguredFor(ctx, centralOwner.ID) {
		return
	}

	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, central)
	if err != nil {
		log.Error("company: AI review: open central repo: %v", err)
		return
	}
	var diffBuf bytes.Buffer
	if err := gitRepo.GetDiff(ctx, central.DefaultBranch+"..."+headBranch, &diffBuf); err != nil {
		log.Error("company: AI review: get diff: %v", err)
		return
	}
	diffText := diffBuf.String()
	truncated := len(diffText) > aiReviewDiffMaxChars
	diffText = truncate(diffText, aiReviewDiffMaxChars)

	userPrompt := fmt.Sprintf("Department repo: %s\nRequest message: %s\n\nDiff%s:\n%s",
		deptRepo.FullName(), message, map[bool]string{true: " (truncated)"}[truncated], diffText)

	review, err := aiChat(ctx, centralOwner.ID, aiReviewSystemPrompt, userPrompt)
	if err != nil {
		log.Error("company: AI review: %v", err)
		return
	}

	if _, err := issue_service.CreateIssueComment(ctx, centralOwner, central, pullIssue, "🤖 **AI Review**\n\n"+review, nil); err != nil {
		log.Error("company: AI review: post comment: %v", err)
	}
}

// snapshotFilesUnderPrefix reads every file in repo's default branch and
// stages it (as a ChangeRepoFile, operation "upload") under the given path
// prefix, ready to hand to files_service.ChangeRepoFiles against a
// *different* repo. Content is read fully into memory — fine for the
// small internal repos this is built for, not for anything approaching
// Git LFS-sized files.
func snapshotFilesUnderPrefix(ctx *context.Context, repo *repo_model.Repository, prefix string) ([]*files_service.ChangeRepoFile, error) {
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, repo)
	if err != nil {
		return nil, err
	}
	commit, err := gitRepo.GetBranchCommit(ctx, repo.DefaultBranch)
	if err != nil {
		return nil, err
	}
	tree, err := commit.SubTree(ctx, gitRepo, "/")
	if err != nil {
		return nil, err
	}
	entries, err := tree.ListEntriesRecursiveFast(ctx, gitRepo)
	if err != nil {
		return nil, err
	}

	files := make([]*files_service.ChangeRepoFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.IsSubModule() {
			continue
		}
		blob := entry.Blob(gitRepo)
		// GetBlobBytes treats a non-positive limit as "read nothing", not
		// "unlimited" — pass the blob's real size instead of -1.
		content, err := blob.GetBlobBytes(ctx, blob.Size(ctx))
		if err != nil {
			return nil, fmt.Errorf("read blob for %s: %w", entry.Name(), err)
		}
		files = append(files, &files_service.ChangeRepoFile{
			Operation:     "upload",
			TreePath:      prefix + "/" + entry.Name(),
			ContentReader: bytes.NewReader(content),
		})
	}
	return files, nil
}

// openPullRequest creates the PR that stands in for "Deploy Request" —
// the admin's native PR review screen (docs/company/architecture.md) is
// where this actually gets approved. target and headRepo may be the same
// repo (a same-repo PR, DeployPost's case) or different: services/git/compare.go's
// GetCompareInfo fetches whatever commits it needs across repos regardless.
// poster is the identity NewPullRequest's own permission check runs
// against — services/pull/pull.go requires it to be target's
// owner/member/collaborator, or to hold write access to headRepo; DeployPost
// passes the central repo's own owner for exactly that reason, since the
// real requester normally has neither.
func openPullRequest(ctx *context.Context, target, headRepo *repo_model.Repository, headBranch, title, content string, poster *user_model.User) (*issues_model.Issue, error) {
	baseRef := git.RefNameFromBranch(target.DefaultBranch)
	headRef := git.RefNameFromBranch(headBranch)

	headGitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, headRepo)
	if err != nil {
		return nil, err
	}

	ci, err := git_service.GetCompareInfo(ctx, target, headRepo, headGitRepo, baseRef, headRef, false, true)
	if err != nil {
		return nil, err
	}

	pullIssue := &issues_model.Issue{
		RepoID:   target.ID,
		Repo:     target,
		Title:    title,
		Content:  content,
		PosterID: poster.ID,
		Poster:   poster,
		IsPull:   true,
	}
	pullRequest := &issues_model.PullRequest{
		HeadRepoID: headRepo.ID,
		BaseRepoID: target.ID,
		HeadBranch: headBranch,
		BaseBranch: target.DefaultBranch,
		HeadRepo:   headRepo,
		BaseRepo:   target,
		MergeBase:  ci.CompareBase,
		Type:       issues_model.PullRequestGitea,
	}
	if err := pull_svc.NewPullRequest(ctx, &pull_svc.NewPullRequestOptions{
		Repo:        target,
		Issue:       pullIssue,
		PullRequest: pullRequest,
	}); err != nil {
		return nil, err
	}
	return pullIssue, nil
}

// Submitted is the plain-language landing page after a deploy request is
// registered — deliberately not Gitea's own PR page (diff/commits/merge
// button), per the decision recorded in docs/company/architecture.md.
// Mounted at /{owner}/{repo}/deploy/submit — no PR index in the URL, it
// just shows this repo's own most recent deploy request, found by
// matching the central repo's PR branch names against this repo's
// owner/name (see parseDeployBranchName) since the PR itself is a
// same-repo PR on the central repo now, not something HeadRepoID
// identifies as "belonging" to this repo.
func Submitted(ctx *context.Context) {
	deptRepo := ctx.Repo.Repository

	pr, err := latestDeployRequest(ctx, deptRepo.OwnerName, deptRepo.Name)
	if err != nil {
		ctx.ServerError("latestDeployRequest", err)
		return
	}
	if pr == nil {
		ctx.NotFound(nil)
		return
	}

	ctx.Data["Title"] = "Submitted"
	ctx.Data["Repo"] = deptRepo
	ctx.Data["Issue"] = pr.Issue
	ctx.HTML(http.StatusOK, tplSubmitted)
}
