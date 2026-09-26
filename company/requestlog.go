// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/translation"
	files_service "gitea.dev/services/repository/files"
)

// A Deploy Request is a pull request, and a pull request with no diff cannot
// be opened or merged. That is fine while a request means "ship this code",
// and wrong the moment it also means "approve these packages": a department
// told their build needs MarkupSafe has nothing to change in their own
// repository, so the snapshot is identical to what is already on central,
// the commit is empty, and the only route to approval does not exist.
//
// The request itself is the thing being submitted, so it is written down.
// Each request appends an entry here, which gives every PR a real diff and
// makes the record of "who asked for what, and why" a commit in the same
// repository as the policy it changes — the same reasoning that puts
// approvals in apps.yml rather than in a table (permissions_commit.go).
//
// Placed under `.company/`, never under `<owner>/<repo>/`. The department's
// prefix is a mirror of their repository: snapshotFilesUnderPrefix deletes
// everything there that is not in the department repo, so a log written into
// it would be removed by the next deploy. `.company/` is outside that prefix
// and structurally unreachable by an employee — every path they can produce
// begins with an owner name, and owner names must start alphanumeric.

// requestLogLimit bounds one app's log. The file is small, but it is rewritten
// on every request and reviewed as a diff, and a thousand entries make both
// worse. The history that matters beyond this is the commit history of the
// file itself, which keeps everything.
const requestLogLimit = 50

// requestLogPath is one app's log.
func requestLogPath(owner, repo string) string {
	return ".company/requests/" + owner + "/" + repo + ".md"
}

// permRequestID is one item's stable name, in the form the policy file uses.
//
// The label beside it is prose — translated, and rewritten whenever somebody
// improves the wording. The decision must survive both, so it is keyed by
// what actually goes into apps.yml.
func permRequestID(r PermissionRequest) string { return r.Kind + ":" + r.Value }

// permLogItem finds one decided item in a log entry: the tick, and the id
// carried in a comment that renders as nothing.
var permLogItem = regexp.MustCompile(`(?m)^- \[([ xX])\][^\n]*<!-- perm:([^>]+?) -->`)

// requestLogEntry renders one request as Markdown.
//
// Written to be read in a PR diff by someone deciding whether to merge it,
// so the permissions come first and the prose last: an admin scanning this is
// answering "what is this asking to be allowed to do".
//
// Each item is a checkbox, and the ticks are the decision — the merge applies
// what is ticked here and nothing else (parseRequestLogDecisions,
// ApplyPermissionsOnMerge). That is why the file is in the pull request: an
// administrator narrowing a request leaves a commit saying so, and the same
// edit made by hand in the Files tab means exactly the same thing. See
// docs/company/permission-lifecycle.md.
func requestLogEntry(locale translation.Locale, actor, title, body string, requests []PermissionRequest, at time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s — @%s\n\n", at.Format("2006-01-02 15:04:05 -0700"), actor)
	fmt.Fprintf(&b, "%s\n\n", title)

	if len(requests) > 0 {
		b.WriteString("승인이 필요한 항목:\n\n")
		for _, r := range requests {
			tick := "x"
			if r.Decision == decisionReject {
				tick = " "
			}
			fmt.Fprintf(&b, "- [%s] **%s** — %s <!-- perm:%s -->\n", tick, permLabel(locale, r), permDetail(locale, r), permRequestID(r))
			if r.Evidence != "" {
				fmt.Fprintf(&b, "  - %s\n", permEvidence(locale, r))
			}
		}
		// One reason covers the request, so it is stated once rather than
		// repeated under every item.
		if reason := firstReason(requests); reason != "" {
			fmt.Fprintf(&b, "\n사유: %s\n", reason)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("코드 변경만 있고 새로 승인받을 항목은 없습니다.\n\n")
	}

	if body != "" {
		fmt.Fprintf(&b, "%s\n\n", body)
	}
	return b.String()
}

// permLabel, permDetail and permEvidence say the item in words.
//
// These fields hold locale keys, and writing the key into the file put
// "company.perm.kind.network" in front of the administrator who had to decide
// on it. Translated with the submitter's locale, like the rest of the file,
// whose headings are written in the platform's own language already.
func permLabel(locale translation.Locale, r PermissionRequest) string {
	return translatedOr(locale, r.Label, r.Label)
}

func permDetail(locale translation.Locale, r PermissionRequest) string {
	if r.DetailKey != "" {
		return translatedOr(locale, r.DetailKey, r.Detail)
	}
	return r.Detail
}

func permEvidence(locale translation.Locale, r PermissionRequest) string {
	if r.EvidenceArg != nil && locale.HasKey(r.Evidence) {
		//nolint:gocritic // the one-arg form below cannot carry the argument
		return locale.TrString(r.Evidence, r.EvidenceArg)
	}
	return translatedOr(locale, r.Evidence, r.Evidence)
}

// translatedOr keeps text that is already text. Not everything in these
// fields is a key — a host, a package name and a version are data.
func translatedOr(locale translation.Locale, key, fallback string) string {
	if locale.HasKey(key) {
		return locale.TrString(key)
	}
	return fallback
}

// parseRequestLogDecisions reads the ticks off the newest entry.
//
// Only the newest: the entries below it are previous requests, already
// decided, and a tick left in one of those must not grant anything now.
//
// The second return value says whether this entry has checkboxes at all.
// Entries written before they existed carry none, and a request that was
// sitting in front of an administrator when the platform was upgraded has to
// keep meaning what it meant when they read it — everything on it, approved
// by the merge.
func parseRequestLogDecisions(md string) (map[string]bool, bool) {
	_, rest, found := strings.Cut(md, "\n## ")
	if !found {
		return nil, false
	}
	entry, _, _ := strings.Cut(rest, "\n## ")
	matches := permLogItem.FindAllStringSubmatch(entry, -1)
	if len(matches) == 0 {
		return nil, false
	}
	out := make(map[string]bool, len(matches))
	for _, m := range matches {
		out[strings.TrimSpace(m[2])] = strings.EqualFold(m[1], "x")
	}
	return out, true
}

// rewriteRequestLogDecisions edits the ticks in the newest entry in place.
//
// A rewrite rather than a regeneration, so the commit an administrator leaves
// on the request says exactly what they changed: the ticks, and the lines of
// anything withdrawn. Regenerating the entry would rewrite the timestamp, the
// wording and the reason too, and bury the one thing the diff is there to
// show.
//
// Withdrawn items are removed outright; refused ones stay, unticked. That is
// the difference between the requester taking something back — it was never
// decided, so there is nothing to record — and an administrator refusing it,
// where the record of what was asked and turned down is the point.
func rewriteRequestLogDecisions(md string, approved, withdrawn map[string]bool) (string, bool) {
	head, rest, found := strings.Cut(md, "\n## ")
	if !found {
		return md, false
	}
	entry, older, hasOlder := strings.Cut(rest, "\n## ")

	var out []string
	changed, dropping := false, false
	for _, line := range strings.Split(entry, "\n") {
		if dropping && strings.HasPrefix(line, "  ") {
			continue // the withdrawn item's own evidence line
		}
		dropping = false
		m := permLogItem.FindStringSubmatch(line)
		if m == nil {
			out = append(out, line)
			continue
		}
		id := strings.TrimSpace(m[2])
		if withdrawn[id] {
			changed, dropping = true, true
			continue
		}
		tick := " "
		if approved[id] {
			tick = "x"
		}
		_, tail, _ := strings.Cut(line, "]")
		updated := "- [" + tick + "]" + tail
		if updated != line {
			changed = true
		}
		out = append(out, updated)
	}

	result := head + "\n## " + strings.Join(out, "\n")
	if hasOlder {
		result += "\n## " + older
	}
	return result, changed
}

func firstReason(requests []PermissionRequest) string {
	for _, r := range requests {
		if r.Reason != "" {
			return r.Reason
		}
	}
	return ""
}

// requestLogHeader names the file for whoever opens it directly rather than
// as a diff.
func requestLogHeader(owner, repo string) string {
	return "# " + owner + "/" + repo + " 배포·승인 요청 기록\n\n" +
		"이 파일은 배포 요청을 제출할 때 플랫폼이 자동으로 기록합니다. 기록 자체는 고치지 마세요.\n" +
		"맨 위 요청의 체크박스는 **승인할 항목**을 뜻합니다 — 체크를 푼 항목은 승인되지 않습니다.\n\n"
}

// buildRequestLogFile returns the change that appends one entry, newest
// first.
//
// Newest first because this is read as a diff at the top of a review, and
// because appending to the end of a growing file makes the reviewer scroll
// past everything that already happened to find what is being asked now.
func buildRequestLogFile(ctx context.Context, central *repo_model.Repository, owner, repo, entry string) *files_service.ChangeRepoFile {
	path := requestLogPath(owner, repo)
	existing, found := readCentralFile(ctx, central, path)

	body := requestLogHeader(owner, repo) + entry
	if found {
		body += trimToRecentEntries(existing, requestLogLimit-1)
	}

	operation := "update"
	if !found {
		operation = "create"
	}
	return &files_service.ChangeRepoFile{
		Operation:     operation,
		TreePath:      path,
		ContentReader: strings.NewReader(body),
	}
}

// trimToRecentEntries drops the header and everything past the limit,
// returning the entries to keep.
func trimToRecentEntries(existing string, limit int) string {
	if limit <= 0 {
		return ""
	}
	// Entries start at a level-2 heading; anything before the first one is the
	// header, which is rewritten rather than carried over.
	_, rest, found := strings.Cut(existing, "\n## ")
	if !found {
		return ""
	}
	entries := strings.Split(rest, "\n## ")
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return "\n## " + strings.Join(entries, "\n## ")
}

// readCentralFile reads one path from the central repo's default branch.
// Absence is normal — the first request for an app creates the file — so it
// is reported as "not found" rather than as an error.
func readCentralFile(ctx context.Context, central *repo_model.Repository, path string) (string, bool) {
	return readCentralFileAt(ctx, central, "", path)
}

// readCentralFileAt reads it as of one branch or commit, or of the default
// branch when ref is empty.
//
// A request's decisions live on its own branch while it is open and in the
// merge commit once it is not — by then the branch has been cleaned up
// (company/deploy_notifier.go), so the merge commit is the only place the
// file as the administrator left it still exists. Both are refs, and which
// one the caller has depends only on when it is asking.
func readCentralFileAt(ctx context.Context, central *repo_model.Repository, ref, path string) (string, bool) {
	gitRepo, err := git.OpenRepository(ctx, central)
	if err != nil {
		return "", false
	}
	defer gitRepo.Close()

	if ref == "" {
		ref = central.DefaultBranch
	}
	commit, err := gitRepo.GetBranchCommit(ctx, ref)
	if err != nil {
		commit, err = gitRepo.GetCommit(ctx, ref)
	}
	if err != nil {
		return "", false
	}
	entry, err := commit.GetTreeEntryByPath(ctx, gitRepo, path)
	if err != nil {
		return "", false
	}
	blob := entry.Blob(gitRepo)
	content, err := blob.GetBlobBytes(ctx, blob.Size(ctx))
	if err != nil {
		return "", false
	}
	return string(content), true
}
