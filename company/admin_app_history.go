// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const tplAdminAppHistory templates.TplName = "company/admin_app_history"

// Every deploy this app has ever had, on its own page.
//
// The app state keeps only the last ten events, because it doubles as the
// rollback index and a longer one would offer releases that have been cleaned
// up. That is the right size for "what happened recently" and the wrong one
// for "when did this app last deploy successfully, and how often does it
// fail" — questions that are about the shape of the record rather than its
// head.
//
// The build log already holds the full answer: every attempt writes a header
// line with its time, outcome and commit before its output. Reading those
// back costs nothing extra to maintain and cannot drift from what actually
// happened, because it *is* what happened.

// deployAttempt is one line of the history.
type deployAttempt struct {
	At      int64
	SHA     string
	Outcome string // OK | FAILED, as written by appendBuildLog
	Failed  bool
	Summary string // the first line of output, which for a failure is the cause
}

// buildLogHeader is the marker appendBuildLog writes:
//
//	===== 2026-09-06T20:32:22+09:00  FAILED  51f705abb655 =====
const buildLogHeader = "====="

// AdminAppHistory renders the deploy history.
func AdminAppHistory(ctx *context.Context) {
	owner, repo := ctx.PathParam("owner"), ctx.PathParam("repo")
	st := LoadAppState(owner, repo)

	attempts := readDeployAttempts(appPathsFor(owner, repo))

	// Paged: an app that has been redeployed weekly for a year has fifty of
	// these, and the page exists precisely for the ones that are not recent.
	page := max(ctx.FormInt("page"), 1)
	const perPage = 25
	start := min((page-1)*perPage, len(attempts))
	end := min(start+perPage, len(attempts))

	ctx.Data["Title"] = owner + "/" + repo + " 배포 이력"
	ctx.Data["App"] = st
	ctx.Data["Attempts"] = attempts[start:end]
	ctx.Data["TotalAttempts"] = len(attempts)
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(int64(len(attempts))).PerPageLimit(perPage).CurPage(page).Build()
	ctx.Data["HistoryRows"] = describeHistory(st.History)
	ctx.Data["Running"] = CurrentRelease(owner, repo)
	ctx.Data["AdminAppLink"] = setting.AppSubURL + "/-/admin/company-deploys/" + owner + "/" + repo
	ctx.HTML(http.StatusOK, tplAdminAppHistory)
}

// readDeployAttempts parses the build log's headers, newest first.
//
// Rotated files are read too, so the history reaches back as far as the logs
// do. Best-effort throughout: this page is a record, and a missing or
// half-written log is a smaller problem than a 500 in front of the admin
// trying to read it.
func readDeployAttempts(p appPaths) []deployAttempt {
	files, err := filepath.Glob(filepath.Join(p.logs, buildLogName+"*"))
	if err != nil {
		return nil
	}
	var attempts []deployAttempt
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		attempts = append(attempts, scanDeployAttempts(f)...)
		_ = f.Close()
	}
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].At > attempts[j].At })
	return attempts
}

func scanDeployAttempts(f *os.File) []deployAttempt {
	var out []deployAttempt
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	wantSummary := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, buildLogHeader) {
			// The first line after a header is the outcome in one sentence:
			// for a failure that is the cause, which is the column an admin
			// actually reads down.
			if wantSummary && line != "" {
				out[len(out)-1].Summary = line
				wantSummary = false
			}
			continue
		}
		fields := strings.Fields(strings.Trim(line, "= "))
		if len(fields) < 2 {
			continue
		}
		at, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}
		attempt := deployAttempt{At: at.Unix(), Outcome: fields[1], Failed: fields[1] == "FAILED"}
		if len(fields) > 2 {
			attempt.SHA = fields[2]
		}
		out = append(out, attempt)
		wantSummary = true
	}
	return out
}
