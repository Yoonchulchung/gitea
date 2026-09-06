// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"strings"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
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

// requestLogEntry renders one request as Markdown.
//
// Written to be read in a PR diff by someone deciding whether to merge it,
// so the permissions come first and the prose last: an admin scanning this is
// answering "what is this asking to be allowed to do".
func requestLogEntry(actor, title, body string, requests []PermissionRequest, at time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s — @%s\n\n", at.Format("2006-01-02 15:04:05 -0700"), actor)
	fmt.Fprintf(&b, "%s\n\n", title)

	if len(requests) > 0 {
		b.WriteString("승인이 필요한 항목:\n\n")
		for _, r := range requests {
			fmt.Fprintf(&b, "- **%s** — %s\n", r.Label, r.Detail)
			if r.Evidence != "" {
				fmt.Fprintf(&b, "  - %s\n", r.Evidence)
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
		"이 파일은 배포 요청을 제출할 때 플랫폼이 자동으로 기록합니다. 직접 편집하지 마세요.\n\n"
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
	gitRepo, err := git.OpenRepository(ctx, central)
	if err != nil {
		return "", false
	}
	defer gitRepo.Close()

	commit, err := gitRepo.GetBranchCommit(ctx, central.DefaultBranch)
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
