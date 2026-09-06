// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"fmt"
	"html/template"
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
	"gitea.dev/modules/highlight"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/modules/typesniffer"
	"gitea.dev/services/context"
	git_service "gitea.dev/services/git"
	issue_service "gitea.dev/services/issue"
	pull_svc "gitea.dev/services/pull"
	files_service "gitea.dev/services/repository/files"

	"github.com/sergi/go-diff/diffmatchpatch"
)

const (
	tplDeployForm templates.TplName = "company/deploy"
	tplSubmitted  templates.TplName = "company/submitted"
)

// centralDeployOwnerName parses [company] CENTRAL_DEPLOY_REPO — split out
// from centralDeployRepo below purely so callers that only have a plain
// stdlib context.Context (company/deploy_notifier.go's notify.Notifier
// hook, which runs outside any HTTP request) can still resolve the repo
// without needing our web-request-scoped *context.Context.
func centralDeployOwnerName() (owner, name string, err error) {
	ownerRepo := setting.CfgProvider.Section("company").Key("CENTRAL_DEPLOY_REPO").String()
	owner, name, ok := strings.Cut(ownerRepo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("[company] CENTRAL_DEPLOY_REPO must be set to an \"owner/name\" repo path")
	}
	return owner, name, nil
}

// centralDeployRepo loads the one company-wide deployment target
// configured in app.ini's [company] section. One repo for the whole
// company, not per-department, so this is a single static config value
// rather than a new admin settings page/DB table — see the "Deploy
// Request" design note in docs/company/architecture.md.
func centralDeployRepo(ctx *context.Context) (*repo_model.Repository, error) {
	owner, name, err := centralDeployOwnerName()
	if err != nil {
		return nil, err
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

// deployFilePreviewMaxSize caps how much of a file DeployForm will render
// inline — staff are meant to eyeball what's shipping, not read a whole
// generated data file; anything bigger falls back to "too large to preview"
// with a link out to the native file view instead.
const deployFilePreviewMaxSize = 200 * 1024

// deployFilesVisibleLimit is how many files deploy.tmpl's file list shows
// by default — a snapshot deploy can easily carry dozens of unrelated
// files, and presenting every one of them at once made the page itself the
// slow part of reviewing a deploy request. The rest are still rendered
// (server-rendered HTML, just hidden), so filtering/searching them
// client-side needs no extra request — see company-deploy-form.ts, which
// reveals the rest on demand behind a "load remaining" button.
const deployFilesVisibleLimit = 20

// deploySplitRow is one row of a deployFilePreview's side-by-side diff —
// GitHub/Gitea's own "split" view convention: an old-side line and a
// new-side line shown together, each with its own real line number so a
// reviewer can tell exactly which line moved where, not just that
// something did. Either side can be blank (Num 0, empty Content) when a
// line only exists on the other side — a pure add has no OldNum, a pure
// del has no NewNum, and an uneven substitution (say 3 old lines replaced
// by 5 new ones) leaves the shorter side blank for the extra rows.
// Content is already syntax-highlighted (modules/highlight), same as the
// old flat unified dump was.
type deploySplitRow struct {
	OldNum     int
	OldType    string // "context", "del", or "" (blank filler)
	OldContent template.HTML
	NewNum     int
	NewType    string // "context", "add", or "" (blank filler)
	NewContent template.HTML
}

// deployDiffSegment groups consecutive deploySplitRows for rendering —
// either a normal segment shown as-is, or a run of pure-context rows (no
// actual change on either side) far enough from the nearest change that
// `git diff` itself wouldn't show it by default either. Collapsed segments
// still carry their real Rows (deploy.tmpl renders them, just hidden) so
// expanding one is a pure client-side reveal — see company-deploy-form.ts —
// not a second request.
type deployDiffSegment struct {
	Collapsed bool
	Rows      []deploySplitRow
}

// deployFilePreview is one row of DeployForm's file list — a real diff
// against whatever's already live (the corresponding path under
// deployPathPrefix on the central repo's default branch), same idea as a
// pull request's Files Changed tab, not just a flat dump of the new
// content. IsNew/IsRemoved cover the two cases where there's only one
// side to show — deploy.tmpl renders those as a single full-width column
// (Rows' NewContent/OldContent respectively) rather than the split view,
// since the other side is always entirely blank for them anyway.
type deployFilePreview struct {
	Path      string
	IsBinary  bool
	TooLarge  bool
	IsNew     bool
	IsRemoved bool
	Rows      []deploySplitRow
	Segments  []deployDiffSegment // Rows grouped for collapsing — only meaningful (and only used by deploy.tmpl) when neither IsNew nor IsRemoved
	Adds      int                 // count of Rows with a NewType "add" — the file list's own +N (deploy.tmpl's toolbar/diffstat)
	Dels      int                 // count of Rows with an OldType "del"
	TypeClass string              // "A"/"M"/"D" — IsNew/IsRemoved flattened into one value the template and its filter buttons can key off of directly
}

// DeployForm renders the "Deploy Request" message form for the repo in
// the URL — mounted at /{owner}/{repo}/deploy alongside Gitea's own repo
// routes (routers/web/web.go), so ctx.Repo.Repository/.Permission are
// already resolved by the time this runs. Also shows every file that will
// actually be uploaded, content included (DeployPost snapshots the whole
// default branch, not just recently-changed files — see
// snapshotFilesUnderPrefix), so staff can see exactly what they're sending
// before they click through.
func DeployForm(ctx *context.Context) {
	if !ctx.Repo.Permission.CanRead(unit.TypeCode) {
		ctx.NotFound(nil)
		return
	}
	files, err := deployFilePreviews(ctx, ctx.Repo.Repository)
	if err != nil {
		ctx.ServerError("deployFilePreviews", err)
		return
	}
	pr, err := latestDeployRequest(ctx, ctx.Repo.Repository.OwnerName, ctx.Repo.Repository.Name)
	if err != nil {
		ctx.ServerError("latestDeployRequest", err)
		return
	}
	// What this deploy needs that the app is not allowed yet, worked out
	// from the files themselves rather than asked for on a blank form — a
	// non-developer handed an empty permissions form fills in either nothing
	// or something unusable. They supply only the reason. See
	// company/permissions.go.
	ctx.Data["PermissionRequests"] = DetectPermissionRequests(
		ctx.Repo.Repository.OwnerName, ctx.Repo.Repository.Name,
		readRepoFile(ctx, ctx.Repo.Repository, "requirements.txt"), ctx.FormString("access"))
	// Both computed here rather than with a chain of {{eq .Status "..."}} in
	// the template — deploy.tmpl uses DeployStatusIcon for both the header
	// badge and the bigger status box's icon bubble, and the resubmit
	// section's own heading ("Revise & resubmit" vs "Submit a deploy
	// request") the same way.
	formHeadingKey := "company.deploy.new_heading"
	if pr != nil {
		status, err := deployStatusFor(ctx, pr)
		if err != nil {
			ctx.ServerError("deployStatusFor", err)
			return
		}
		ctx.Data["DeployRequestStatus"] = status
		switch status.Status {
		case "approved":
			ctx.Data["DeployStatusIcon"] = "octicon-check"
		case "deployed":
			ctx.Data["DeployStatusIcon"] = "octicon-check-circle"
		case "rejected", "cancelled", "deploy_failed":
			ctx.Data["DeployStatusIcon"] = "octicon-x-circle"
		default: // pending, deploying
			ctx.Data["DeployStatusIcon"] = "octicon-diff"
		}

		// pr.Issue.Poster is the central repo's own owner (see DeployPost) —
		// the actual requester's identity only lives in the branch name
		// (same lookup company/deployrequests.go does for its own list).
		// Best-effort: a lookup failure here just means the header's byline
		// is omitted, not that the whole page fails.
		if _, _, requesterID, ok := parseDeployBranchName(pr.HeadBranch); ok {
			if requester, err := user_model.GetUserByID(ctx, requesterID); err == nil {
				ctx.Data["DeployRequester"] = requester
			}
		}

		// Only fetched when there's actually something to explain — most
		// often an admin's own reason for rejecting this, typed into
		// Gitea's native "close with comment" box on the PR. Shared with
		// DeployStatus (company/deploystatus.go), which summarizes the same
		// reasons into the repo home page badge's tooltip.
		if status.Status == "rejected" {
			formHeadingKey = "company.deploy.resubmit_heading"
			reasons, err := rejectionReasons(ctx, pr)
			if err != nil {
				ctx.ServerError("rejectionReasons", err)
				return
			}
			ctx.Data["RejectionComments"] = reasons
		}
	}

	// Totals for the file list's toolbar (per-type filter counts) and its
	// overall diffstat — summed here rather than with template arithmetic,
	// which Go's html/template has none of.
	var added, modified, removed, totalAdds, totalDels int
	for _, f := range files {
		switch f.TypeClass {
		case "A":
			added++
		case "D":
			removed++
		default:
			modified++
		}
		totalAdds += f.Adds
		totalDels += f.Dels
	}
	ctx.Data["DeployFilesAdded"] = added
	ctx.Data["DeployFilesModified"] = modified
	ctx.Data["DeployFilesRemoved"] = removed
	ctx.Data["DeployDiffAdds"] = totalAdds
	ctx.Data["DeployDiffDels"] = totalDels
	ctx.Data["DeployFilesVisibleLimit"] = deployFilesVisibleLimit
	if len(files) > deployFilesVisibleLimit {
		ctx.Data["DeployFilesRemaining"] = len(files) - deployFilesVisibleLimit
	}

	ctx.Data["DeployFormHeadingKey"] = formHeadingKey

	ctx.Data["Title"] = string(ctx.Locale.Tr("company.deploy.title"))
	ctx.Data["Repo"] = ctx.Repo.Repository
	ctx.Data["DeployFiles"] = files
	ctx.HTML(http.StatusOK, tplDeployForm)
}

// deployFilePreviews computes a real diff between repo's current default
// branch and whatever's already live for this department (the
// corresponding path under deployPathPrefix, on the central repo's own
// default branch) — same idea as a pull request's Files Changed tab,
// not just a flat dump of the new content.
func deployFilePreviews(ctx *context.Context, repo *repo_model.Repository) ([]deployFilePreview, error) {
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
	afterEntries, err := tree.ListEntriesRecursiveFast(ctx, gitRepo)
	if err != nil {
		return nil, err
	}
	afterPaths := make(map[string]bool, len(afterEntries))
	for _, entry := range afterEntries {
		if !entry.IsDir() && !entry.IsSubModule() {
			afterPaths[entry.Name()] = true
		}
	}

	central, err := centralDeployRepo(ctx)
	if err != nil {
		return nil, err
	}
	centralGitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, central)
	if err != nil {
		return nil, err
	}
	centralCommit, err := centralGitRepo.GetBranchCommit(ctx, central.DefaultBranch)
	if err != nil {
		return nil, err
	}
	prefix := deployPathPrefix(repo.OwnerName, repo.Name)
	beforePaths, err := centralPrefixPaths(ctx, centralGitRepo, centralCommit, prefix)
	if err != nil {
		return nil, err
	}

	allPaths := make(map[string]bool, len(afterPaths)+len(beforePaths))
	for p := range afterPaths {
		allPaths[p] = true
	}
	for p := range beforePaths {
		allPaths[p] = true
	}

	previews := make([]deployFilePreview, 0, len(allPaths))
	for path := range allPaths {
		preview := deployFilePreview{Path: path, IsNew: !beforePaths[path], IsRemoved: !afterPaths[path]}
		switch {
		case preview.IsNew:
			preview.TypeClass = "A"
		case preview.IsRemoved:
			preview.TypeClass = "D"
		default:
			preview.TypeClass = "M"
		}

		var beforeContent, afterContent []byte
		var tooLarge bool
		if beforePaths[path] {
			content, big, err := readBlobAt(ctx, centralCommit, centralGitRepo, prefix+"/"+path)
			if err != nil {
				return nil, fmt.Errorf("read central blob for %s: %w", path, err)
			}
			beforeContent, tooLarge = content, tooLarge || big
		}
		if afterPaths[path] {
			content, big, err := readBlobAt(ctx, commit, gitRepo, path)
			if err != nil {
				return nil, fmt.Errorf("read blob for %s: %w", path, err)
			}
			afterContent, tooLarge = content, tooLarge || big
		}
		if tooLarge {
			preview.TooLarge = true
			previews = append(previews, preview)
			continue
		}
		// A path present on both sides with byte-identical content isn't
		// actually changing — before this check it still landed here as
		// "Modified" with an empty, all-context diff (+0/-0), which the
		// file list's own Added/Modified/Removed filter then dutifully
		// counted and showed as if something had changed. Nothing to skip
		// for IsNew/IsRemoved: those have only one side to begin with, so
		// they're never spuriously "identical".
		if preview.TypeClass == "M" && bytes.Equal(beforeContent, afterContent) {
			continue
		}
		if !typesniffer.DetectContentType(beforeContent).IsText() || !typesniffer.DetectContentType(afterContent).IsText() {
			// DetectContentType([]byte{}) reports plain text, so an empty
			// nil-content side (the file doesn't exist there) never
			// wrongly trips this on its own.
			preview.IsBinary = true
			previews = append(previews, preview)
			continue
		}

		preview.Rows = diffPreviewSplitRows(path, beforeContent, afterContent)
		for _, row := range preview.Rows {
			if row.OldType == "del" {
				preview.Dels++
			}
			if row.NewType == "add" {
				preview.Adds++
			}
		}
		preview.Segments = groupDiffSegments(preview.Rows)
		previews = append(previews, preview)
	}
	sort.Slice(previews, func(i, j int) bool { return previews[i].Path < previews[j].Path })
	return previews, nil
}

// deletePathsFor returns the paths that are live on central but no longer
// present in the department repo — i.e. what the staff deleted. Sorted so
// a given pair of trees always produces byte-identical commit contents;
// map iteration order is not stable and would otherwise leak into the
// commit.
//
// Split out from snapshotFilesUnderPrefix purely so this — the logic whose
// absence was the bug — is testable without building two git repositories.
func deletePathsFor(kept, live map[string]bool) []string {
	deletes := make([]string, 0, len(live))
	for path := range live {
		if !kept[path] {
			deletes = append(deletes, path)
		}
	}
	sort.Strings(deletes)
	return deletes
}

// centralPrefixPaths lists every file currently live under prefix on the
// central repo's default branch, keyed by its path *relative to prefix*.
//
// Shared deliberately by deployFilePreviews (which shows the staff what
// will change) and snapshotFilesUnderPrefix (which builds the commit that
// actually changes it). Those two used to compute this separately, and
// only the preview side did — so a file the staff deleted was shown as
// "Removed" and then silently survived the merge. Any future change to
// "what counts as live" has to land in one place for the two to stay
// honest with each other.
//
// A missing prefix subtree just means this department has never deployed
// anything yet — every file is new, so an empty set (not an error) is the
// right answer.
func centralPrefixPaths(ctx *context.Context, centralGitRepo *git.Repository, centralCommit *git.Commit, prefix string) (map[string]bool, error) {
	paths := map[string]bool{}
	tree, err := centralCommit.SubTree(ctx, centralGitRepo, prefix)
	if err != nil {
		return paths, nil //nolint:nilerr // a missing subtree is "nothing deployed yet", not a failure
	}
	entries, err := tree.ListEntriesRecursiveFast(ctx, centralGitRepo)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && !entry.IsSubModule() {
			paths[entry.Name()] = true
		}
	}
	return paths, nil
}

// readBlobAt reads one file's content at path out of commit (in gitRepo),
// or (nil, true, nil) if it's over deployFilePreviewMaxSize — same size
// guard deployFilePreviews always applied, now shared by both the
// "before" (central) and "after" (department) side.
func readBlobAt(ctx *context.Context, commit *git.Commit, gitRepo *git.Repository, path string) (content []byte, tooLarge bool, err error) {
	entry, err := commit.GetTreeEntryByPath(ctx, gitRepo, path)
	if err != nil {
		return nil, false, err
	}
	blob := entry.Blob(gitRepo)
	size := blob.Size(ctx)
	if size > deployFilePreviewMaxSize {
		return nil, true, nil
	}
	// GetBlobBytes treats a non-positive limit as "read nothing", not
	// "unlimited" — pass the blob's real size instead of -1.
	content, err = blob.GetBlobBytes(ctx, size)
	return content, false, err
}

// diffPreviewSplitRows computes a line-level diff between before and after
// (empty on whichever side doesn't apply — a brand-new or fully-removed
// file), syntax-highlighting each side once (modules/highlight, same as the
// native file view), and pairs the result into side-by-side rows
// (deploySplitRow) with real old/new line numbers on each side — the same
// "split" view convention Gitea's own PR diff offers, rather than a single
// flat +/- column with no numbering that left old vs. new ambiguous.
func diffPreviewSplitRows(filename string, before, after []byte) []deploySplitRow {
	dmp := diffmatchpatch.New()
	a, b, lineArray := dmp.DiffLinesToChars(string(before), string(after))
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lineArray)

	beforeLines, _ := highlight.RenderFullFile(filename, "", before)
	afterLines, _ := highlight.RenderFullFile(filename, "", after)

	var rows []deploySplitRow
	var bi, ai int // count of beforeLines/afterLines already committed to rows
	var pendingDel, pendingAdd []template.HTML

	// takeLines grabs up to n lines starting at start, clamped to what's
	// actually left — beforeLines/afterLines can run short of what the
	// diff's own line count implies if RenderFullFile ever splits
	// differently than DiffLinesToChars did (same defensive clamp the old
	// unified renderer applied per line).
	takeLines := func(lines []template.HTML, start, n int) []template.HTML {
		end := min(start+n, len(lines))
		start = min(start, len(lines))
		return lines[start:end]
	}

	// flushPending pairs up one contiguous run of deleted/inserted lines
	// side by side — a plain del-then-add block (the common "line
	// changed" case) becomes one row per line with both sides filled; an
	// uneven count (more deletes than inserts or vice versa) leaves the
	// shorter side's remaining rows blank rather than misaligning content
	// that was never actually a pair.
	flushPending := func() {
		n := max(len(pendingDel), len(pendingAdd))
		for i := range n {
			var row deploySplitRow
			if i < len(pendingDel) {
				bi++
				row.OldNum, row.OldType, row.OldContent = bi, "del", pendingDel[i]
			}
			if i < len(pendingAdd) {
				ai++
				row.NewNum, row.NewType, row.NewContent = ai, "add", pendingAdd[i]
			}
			rows = append(rows, row)
		}
		pendingDel, pendingAdd = nil, nil
	}

	for _, d := range diffs {
		text := strings.TrimSuffix(d.Text, "\n")
		if text == "" {
			continue
		}
		n := strings.Count(text, "\n") + 1
		switch d.Type {
		case diffmatchpatch.DiffDelete:
			pendingDel = append(pendingDel, takeLines(beforeLines, bi+len(pendingDel), n)...)
		case diffmatchpatch.DiffInsert:
			pendingAdd = append(pendingAdd, takeLines(afterLines, ai+len(pendingAdd), n)...)
		default: // Equal — same content on both sides, so both line counters always advance together
			flushPending()
			for _, line := range takeLines(afterLines, ai, n) {
				bi++
				ai++
				rows = append(rows, deploySplitRow{OldNum: bi, OldType: "context", OldContent: line, NewNum: ai, NewType: "context", NewContent: line})
			}
		}
	}
	flushPending()
	return rows
}

// deployDiffContextMargin is how many unchanged lines stay visible
// immediately around each actual change — the same convention `git diff`
// itself uses by default (-U3). Anything further from a change collapses
// behind a "Show N more lines" toggle instead of dumping a whole file's
// untouched content by default, matching the reference UI: only the
// changed parts are visible at first, with the rest available on demand.
const deployDiffContextMargin = 3

// deployDiffCollapseMin is the smallest a run of hideable context has to be
// before collapsing it is worth doing at all — collapsing away a single
// line just to show a "Show 1 more line" button would be more clutter than
// it saves.
const deployDiffCollapseMin = deployDiffContextMargin + 2

// groupDiffSegments groups rows into segments for deploy.tmpl: a change
// (deleted or added line) always keeps deployDiffContextMargin lines of
// plain context visible on either side of it; a run of context longer than
// that, once trimmed to deployDiffCollapseMin or more, becomes one
// collapsed segment instead of deployDiffCollapseMin-plus individual rows.
func groupDiffSegments(rows []deploySplitRow) []deployDiffSegment {
	keep := make([]bool, len(rows))
	for i, r := range rows {
		if r.OldType != "del" && r.NewType != "add" {
			continue
		}
		for j := max(0, i-deployDiffContextMargin); j <= min(len(rows)-1, i+deployDiffContextMargin); j++ {
			keep[j] = true
		}
	}

	var segments []deployDiffSegment
	for i := 0; i < len(rows); {
		j := i
		for j < len(rows) && keep[j] == keep[i] {
			j++
		}
		if !keep[i] && j-i >= deployDiffCollapseMin {
			segments = append(segments, deployDiffSegment{Collapsed: true, Rows: rows[i:j]})
		} else {
			segments = append(segments, deployDiffSegment{Rows: rows[i:j]})
		}
		i = j
	}
	return segments
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

	title := strings.TrimSpace(ctx.Req.FormValue("title"))
	if title == "" {
		ctx.HTTPError(http.StatusBadRequest, "title required")
		return
	}
	body := strings.TrimSpace(ctx.Req.FormValue("message")) // optional — see the form field's own label/placeholder for why it's still called "message"

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

	files, err := snapshotFilesUnderPrefix(ctx, deptRepo, central, deployPathPrefix(deptRepo.OwnerName, deptRepo.Name))
	if err != nil {
		ctx.ServerError("snapshotFilesUnderPrefix", err)
		return
	}
	// Zero files means "nothing to say": the department repo is empty *and*
	// nothing of theirs is live on central. An empty repo whose files were
	// all deleted still produces deletes, and that is a legitimate request —
	// it's how a department un-deploys. Rejecting on "no uploads" alone would
	// leave them unable to take their app down.
	if len(files) == 0 {
		ctx.HTTPError(http.StatusBadRequest, "nothing to deploy: the repository is empty and nothing is currently deployed")
		return
	}

	newBranch := deployBranchName(deptRepo.OwnerName, deptRepo.Name, ctx.Doer.ID)
	if _, err := files_service.ChangeRepoFiles(ctx, central, centralOwner, &files_service.ChangeRepoFilesOptions{
		OldBranch: central.DefaultBranch,
		NewBranch: newBranch,
		Message:   title,
		Files:     files,
	}); err != nil {
		ctx.ServerError("ChangeRepoFiles", err)
		return
	}

	// This snapshot is always the department repo's full current state, so
	// any deploy request still pending review is now stale the moment this
	// one lands — auto-withdraw it rather than leave the admin with two
	// open requests for the same repo, one of which is guaranteed to be
	// out of date. Done only after the snapshot above succeeds, so a
	// failed submission never cancels the still-good previous request.
	if err := cancelOpenDeployRequests(ctx, central, deptRepo.OwnerName, deptRepo.Name, ctx.Doer); err != nil {
		ctx.ServerError("cancelOpenDeployRequests", err)
		return
	}

	content := fmt.Sprintf("Requested by @%s (%s).", ctx.Doer.Name, deptRepo.FullName())
	if body != "" {
		content += "\n\n" + body
	}
	pullIssue, err := openPullRequest(ctx, central, central, newBranch, title, content, centralOwner)
	if err != nil {
		ctx.ServerError("openPullRequest", err)
		return
	}

	// Best-effort, same reasoning as the AI review comment below — see
	// applyDeployLabels (company/labels.go).
	applyDeployLabels(ctx, central, deptRepo, pullIssue, centralOwner)

	// Attach the permission items to this PR so the admin reviewing the code
	// sees, on the same screen, what the app is asking to be allowed to do.
	// Best-effort: the deploy request itself has already been created, and
	// failing it now over bookkeeping would lose the submission.
	if requests := collectPermissionRequests(ctx, deptRepo); len(requests) > 0 {
		if err := SavePermissionRequests(deptRepo.OwnerName, deptRepo.Name, pullIssue.ID, requests); err != nil {
			log.Error("company: saving permission requests for %s: %v", deptRepo.FullName(), err)
		}
	}

	// title (required) + body (optional) as the AI review's own context —
	// deliberately not `content` above, which also carries "Requested by
	// @x (dept/repo)" boilerplate generateAIReview's caller already gets
	// as a separate deptRepoFullName argument.
	reviewContext := title
	if body != "" {
		reviewContext += "\n\n" + body
	}
	// Best-effort: an admin still reviews and approves every Deploy
	// Request regardless, so a slow/failing/unconfigured AI review must
	// never block or fail the actual submission — see postAIReviewComment.
	postAIReviewComment(ctx, central, centralOwner, pullIssue, newBranch, deptRepo, reviewContext)

	ctx.Redirect(deptRepo.Link() + "/deploy/submit")
}

// aiReviewSystemPrompt was flattened before — everything from "hardcoded
// secret" to "weird filename" landed as equally-weighted bullets, so a
// genuine security finding could sit buried between cosmetic ones where a
// skim would miss it. The user's own test case surfaced this directly: a
// file reading /lib/ac and writing its content into the repo's own
// ./deploy output (an exfiltration-via-deployment-artifact pattern, not
// just a print) got one hedged bullet ("확인 필요") among six, no more
// prominent than a stray test file. This version forces the security
// category to lead as its own section, states findings as fact rather
// than a question, and explicitly names two easy-to-rationalize-away
// shapes: a read paired with any kind of write/output (including landing
// in what gets deployed, not just stdout), and a general-purpose
// recursive/bulk file-reader function — a risk by capability even if
// unparameterized, unvalidated, or not currently called from anywhere.
const aiReviewSystemPrompt = `You are reviewing a pull request for an internal deploy pipeline: a department's files being brought into a shared repo an admin will approve before it ships. Reply in Korean.

Actively look for these — they are the actual point of this review, not one item among many:
- Code that reads a file (or anything else — env vars, other processes' data) from somewhere OUTSIDE what the stated purpose plainly needs, and then exposes that content anywhere: prints it, logs it, sends it over the network, or writes it into a file — including a file that becomes part of what gets deployed/shipped. That last form is easy to miss: copying an external file's content into an output path inside the repo (a "deploy" folder, a build artifact, anything that ships) is exfiltration through a completely legitimate-looking channel, not merely a print statement to flag as a curiosity. Covers absolute paths, paths outside the project (../, /etc, /lib, home directories, other users' data), paths assembled from unvalidated input, and reads of config/credential-shaped files. This is the single most important pattern to catch — a read-then-expose pair can leak arbitrary data the deploy process can reach, even when the target path looks made-up, harmless, or like test filler.
- A general-purpose function that recursively walks a directory tree (or otherwise loops over many files/paths) and reads/prints/returns their contents — flag this even if it's unparameterized, called with a default like "." rather than something obviously sensitive, or not invoked anywhere in this diff at all. The risk is the capability sitting in shipped code: it can be called with any path later, by this code or by someone editing it next. "Not called from main" is not a reason to skip it.
- Hardcoded secrets, credentials, API keys, tokens, or connection strings.
- Network calls to hosts that have no obvious reason to be there, especially ones sending data out.
- Code that does something materially different from what the request's title/description claims.
- Obviously broken or incomplete code.

Structure your reply exactly like this:
1. If you found ANY item from the list above: start with a "🔴 보안 확인 필요" section. For each finding, name the exact file, and explain the actual mechanism in a full sentence or two — not "확인이 필요합니다", but what the code concretely does and what an attacker or accident could get out of it (e.g. "a.py의 read_and_deploy_ac_file()이 /lib/ac 파일을 읽어 ./deploy/ac로 그대로 복사합니다 — 이 프로세스가 접근 가능한 시스템 파일 내용이 배포 결과물을 통해 그대로 유출됩니다"). Never soften this into a question when the code's behavior is unambiguous — if it reads and exposes something, or ships a general-purpose file-dumping capability, say so as a fact, not a thing to "check."
2. Everything else (typos, unclear commit message, stray test files, naming oddities) goes in a separate section after, and can stay brief — a short bullet list is fine there.
3. If nothing from the security list applies and nothing else stood out either, say so plainly in one line — don't manufacture concerns to fill space.

This is advisory context for the human reviewer, not a blocker — don't refuse to review and don't recommend rejecting it, just make sure a reviewer skimming quickly still can't miss a real finding.`

// aiReviewChunkMaxChars caps one chunk's diff size — small enough that a
// review actually reads it closely instead of skimming a wall of text, big
// enough that a typical single file's diff doesn't get split further
// (splitDiffIntoChunks only ever splits at file boundaries, never mid-file).
// Replaces the old flat aiReviewDiffMaxChars truncation: a large diff used
// to just lose everything past the cutoff with no comment on what was
// skipped; now it's reviewed in full, one chunk at a time.
const aiReviewChunkMaxChars = 20000

// aiReviewMaxChunks caps how many separate review comments one Deploy
// Request can generate — an extreme diff (a full initial import) chunked
// without a ceiling would mean dozens of AI calls and dozens of comments,
// which stops being a helpful review and starts being noise the admin has
// to scroll past. Whatever doesn't fit gets one final note saying so
// (appended in generateAIReviews below), rather than silently reviewing
// only the first few chunks with no explanation of what got skipped.
const aiReviewMaxChunks = 8

// splitDiffIntoChunks breaks a full git diff into pieces that never cut a
// single file's diff in half, each capped at maxChars — packs whole
// per-file sections greedily until the next one would overflow the current
// chunk, then starts a new one. A single file's diff bigger than maxChars
// on its own becomes its own oversized chunk: there's no good place to
// split a diff hunk without losing context a review needs, so it's left
// over-budget rather than cut arbitrarily.
func splitDiffIntoChunks(diff string, maxChars int) []string {
	if diff == "" {
		return nil
	}
	// git diff output is a sequence of "diff --git a/X b/Y" sections;
	// splitting on the delimiter below (and restoring what it ate, for
	// every section but the first, which already keeps its own header)
	// never separates a file's diff from the header naming it.
	sections := strings.Split(diff, "\ndiff --git ")
	for i := 1; i < len(sections); i++ {
		sections[i] = "diff --git " + sections[i]
	}

	chunks := make([]string, 0, len(sections))
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
		}
	}
	for _, section := range sections {
		if section == "" {
			continue
		}
		if current.Len() > 0 && current.Len()+len(section) > maxChars {
			flush()
		}
		current.WriteString(section)
	}
	flush()
	return chunks
}

// generateAIReviews runs the AI review prompt against headBranch's diff on
// central, one chunk at a time (splitDiffIntoChunks), and returns one
// review string per chunk actually reviewed — the part postAIReviewComment
// (automatic, on every submission) and TriggerDeployRequestAIReview
// (on-demand, an admin's own button click, company/pull_ai_review.go) both
// need, differing only in whose AI settings run the request and what
// happens with the results. Each returned string becomes its own separate
// PR comment (never concatenated into one) — a big diff reviewed as five
// chunks means five comments, not one comment five times as long, so each
// stays readable and the model actually looks closely at its own slice
// instead of skimming a huge combined prompt.
//
// Stops and returns what succeeded so far, plus the error, the moment any
// one chunk's request fails — a transient failure on chunk 3 of 5 still
// means chunks 1-2's real findings are worth keeping and posting.
func generateAIReviews(ctx *context.Context, aiUserID int64, central *repo_model.Repository, headBranch, deptRepoFullName, message string) ([]string, error) {
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, central)
	if err != nil {
		return nil, fmt.Errorf("open central repo: %w", err)
	}
	var diffBuf bytes.Buffer
	if err := gitRepo.GetDiff(ctx, central.DefaultBranch+"..."+headBranch, &diffBuf); err != nil {
		return nil, fmt.Errorf("get diff: %w", err)
	}

	chunks := splitDiffIntoChunks(diffBuf.String(), aiReviewChunkMaxChars)
	omitted := 0
	if len(chunks) > aiReviewMaxChunks {
		omitted = len(chunks) - aiReviewMaxChunks
		chunks = chunks[:aiReviewMaxChunks]
	}

	reviews := make([]string, 0, len(chunks)+1)
	for i, chunk := range chunks {
		userPrompt := fmt.Sprintf("Department repo: %s\nRequest message: %s\n\n", deptRepoFullName, message)
		if len(chunks) > 1 {
			userPrompt += fmt.Sprintf("This is part %d of %d of the full diff for this request — only the file(s) below, reviewed on their own; other parts cover the rest:\n", i+1, len(chunks))
		}
		userPrompt += "Diff:\n" + chunk

		review, err := aiChat(ctx, aiUserID, aiReviewSystemPrompt, userPrompt)
		if err != nil {
			return reviews, err
		}
		if len(chunks) > 1 {
			review = fmt.Sprintf("**(파트 %d/%d)**\n\n%s", i+1, len(chunks), review)
		}
		reviews = append(reviews, review)
	}
	if omitted > 0 {
		reviews = append(reviews, fmt.Sprintf("⚠️ 변경 사항이 많아 %d개 부분 중 처음 %d개만 자동 리뷰했습니다. 나머지 %d개 부분은 직접 확인해주세요.", len(chunks)+omitted, len(chunks), omitted))
	}
	return reviews, nil
}

// postAIReviewComment posts an AI-generated review as a plain comment on
// the deploy-request PR, authored as centralOwner (the admin who owns the
// central repo, and the same identity DeployPost already uses for the
// commit/PR itself — see its own doc comment on why the real requester
// isn't used here). Best-effort only: unconfigured AI, an API error, or
// anything else here is logged and swallowed, never surfaced to the
// employee submitting the request — the admin still reviews and approves
// every deploy request themselves regardless of what (if anything) the AI
// said. An admin can also trigger a fresh one on demand, run as their own
// AI settings instead of centralOwner's — see
// company/pull_ai_review.go's TriggerDeployRequestAIReview.
// aiReviewCommentMarker prefixes every AI review comment this package ever
// posts (both the automatic one below and TriggerDeployRequestAIReview's
// on-demand one, company/pull_ai_review.go) — a stable way to tell "an AI
// review" apart from something a person actually typed, used by DeployForm
// to keep the automatic pre-review (posted on every submission regardless
// of outcome) out of its "why was this rejected" section.
const aiReviewCommentMarker = "🤖 **AI Review**"

func postAIReviewComment(ctx *context.Context, central *repo_model.Repository, centralOwner *user_model.User, pullIssue *issues_model.Issue, headBranch string, deptRepo *repo_model.Repository, message string) {
	if !AIConfiguredFor(ctx, centralOwner.ID) {
		return
	}
	reviews, err := generateAIReviews(ctx, centralOwner.ID, central, headBranch, deptRepo.FullName(), message)
	if err != nil {
		log.Error("company: AI review: %v", err) // still post whatever chunks succeeded before the error, below
	}
	for _, review := range reviews {
		if _, err := issue_service.CreateIssueComment(ctx, centralOwner, central, pullIssue, aiReviewCommentMarker+"\n\n"+review, nil); err != nil {
			log.Error("company: AI review: post comment: %v", err)
		}
	}
}

// snapshotFilesUnderPrefix stages repo's default branch under the given
// path prefix, ready to hand to files_service.ChangeRepoFiles against the
// *central* repo: an "upload" for every file that exists now, plus a
// "delete" for every file that is still live under prefix on central but
// is gone from repo. The snapshot is a full mirror, so without the deletes
// a file the staff removed would survive on central forever — the preview
// (deployFilePreviews) already showed it as "Removed", so the two disagreed
// and the merge silently kept running deleted code.
//
// The deletes are derived from central's *actual* tree via the shared
// centralPrefixPaths, never from a blanket recursive delete of the prefix:
// handleCheckErrors (services/repository/files/update.go) verifies every
// delete target exists *before* running any operation, and its
// IsErrNotExist escape hatch is upload-only — so a blanket delete would
// fail the whole submission for a department deploying for the first time.
//
// Content is read fully into memory — fine for the small internal repos
// this is built for, not for anything approaching Git LFS-sized files.
func snapshotFilesUnderPrefix(ctx *context.Context, repo, central *repo_model.Repository, prefix string) ([]*files_service.ChangeRepoFile, error) {
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
	kept := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.IsSubModule() {
			continue
		}
		kept[entry.Name()] = true
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

	centralGitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, central)
	if err != nil {
		return nil, err
	}
	centralCommit, err := centralGitRepo.GetBranchCommit(ctx, central.DefaultBranch)
	if err != nil {
		return nil, err
	}
	livePaths, err := centralPrefixPaths(ctx, centralGitRepo, centralCommit, prefix)
	if err != nil {
		return nil, err
	}
	// Deletes last: the two sets are disjoint by construction (a path is
	// either still in the department repo or not), so ordering is cosmetic
	// — ChangeRepoFiles walks opts.Files in slice order.
	for _, path := range deletePathsFor(kept, livePaths) {
		files = append(files, &files_service.ChangeRepoFile{
			Operation: "delete",
			TreePath:  prefix + "/" + path,
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

	ctx.Data["Title"] = string(ctx.Locale.Tr("company.deploy.submitted_title"))
	ctx.Data["Repo"] = deptRepo
	ctx.Data["Issue"] = pr.Issue
	ctx.HTML(http.StatusOK, tplSubmitted)
}
