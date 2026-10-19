// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gitea.dev/modules/setting"
	gitea_context "gitea.dev/services/context"
)

// A failed deploy on the history page, explained by the AI from what that
// attempt wrote to the build log and what the app printed around it — a
// deploy that fails its health check says so in the build log, while the
// traceback that caused it is in the app's own.

// deployDiagnoseLines bounds what one attempt contributes: the cause is at the
// end of a build's output, and pip can print thousands of lines before it.
const deployDiagnoseLines = 200

// deployAttemptOutput is everything appendBuildLog wrote for the attempt whose
// header carries this time, oldest line first.
func deployAttemptOutput(p appPaths, at int64) []string {
	files, _ := filepath.Glob(filepath.Join(p.logs, buildLogName+"*"))
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		lines := scanAttemptOutput(bufio.NewScanner(f), at)
		_ = f.Close()
		if lines != nil {
			return lines
		}
	}
	return nil
}

func scanAttemptOutput(scanner *bufio.Scanner, at int64) []string {
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var out []string
	inside := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(line), buildLogHeader) {
			if inside {
				break
			}
			fields := strings.Fields(strings.Trim(strings.TrimSpace(line), "= "))
			if len(fields) > 0 {
				if t, err := time.Parse(time.RFC3339, fields[0]); err == nil && t.Unix() == at {
					inside, out = true, []string{}
				}
			}
			continue
		}
		if inside {
			out = append(out, line)
		}
	}
	if len(out) > deployDiagnoseLines {
		out = out[len(out)-deployDiagnoseLines:]
	}
	return out
}

const deployDiagnosePrompt = `You are helping the administrator of an internal platform that builds and runs small Python web apps (FastAPI) for company departments. One deploy failed. Below is that attempt's build output and, if any, the error lines the app printed around the same time.

Reply in Korean, under 200 words, structured as:
1. 원인 — what made this deploy fail, in one or two sentences.
2. 근거 — the specific line(s) that show it.
3. 조치 — what to do: either what the department must change in its code or requirements.txt, or what the administrator can do on the platform (approve a package, raise a memory limit, allow an outbound host, retry when the package index was unreachable). Say which of the two it is.
If the output does not show the cause, say so plainly rather than guessing.`

// DiagnoseDeployFailure asks the platform AI why one attempt failed: it is an
// administrator's question about the platform, not one for their own key.
func DiagnoseDeployFailure(ctx context.Context, actor, owner, repo string, at int64) (string, error) {
	if AILogAccess() == AILogAccessOff {
		return "", userKeyError("company.logs.diagnose_policy_off")
	}
	if reason := PlatformAIUnavailableReason(ctx); reason != "" {
		return "", userKeyError(reason)
	}
	build := deployAttemptOutput(appPathsFor(owner, repo), at)
	if len(build) == 0 {
		return "", userKeyError("company.history.diagnose_no_output")
	}
	text := aiOutputText(owner, repo, strings.Join(build, "\n"))

	var appErrors []LogLine
	if lines, _, err := ReadAppLogs(owner, repo, LogQuery{Limit: logDiagnoseScan}); err == nil {
		for _, l := range lines {
			if l.At >= at-logFindingWindow && l.At <= at+60 {
				appErrors = append(appErrors, l)
			}
		}
		appErrors = aiLogLines(owner, repo, ErrorLines(appErrors))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "App: %s/%s\nDeploy attempted at %s\n\nBuild output:\n%s\n", owner, repo, time.Unix(at, 0).Format(time.RFC3339), text)
	if len(appErrors) > 0 {
		b.WriteString("\nApp error lines around that time:\n")
		for _, l := range appErrors {
			b.WriteString(strings.TrimRight(l.Text, "\r\n"))
			b.WriteByte('\n')
		}
	}
	return platformAIChat(ctx, aiPurposeDeployDiagnose, actor, owner+"/"+repo, deployDiagnosePrompt, b.String())
}

// AdminAppHistoryDiagnose answers the button on a failed row of the history.
func AdminAppHistoryDiagnose(ctx *gitea_context.Context) {
	owner, repo := ctx.PathParam("owner"), ctx.PathParam("repo")
	back := setting.AppSubURL + "/-/admin/company-deploys/" + owner + "/" + repo + "/history"
	at, _ := strconv.ParseInt(ctx.FormString("at"), 10, 64)
	answer, err := DiagnoseDeployFailure(ctx, ctx.Doer.Name, owner, repo, at)
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		ctx.Redirect(back + "?page=" + ctx.FormString("page"))
		return
	}
	// Rendered on this response: a Korean answer outgrows the 4 KB flash cookie, which the browser then drops.
	ctx.Flash.Info(ctx.Locale.TrString("company.history.diagnose_result", time.Unix(at, 0).In(setting.DefaultUILocation).Format("2006-01-02 15:04:05"))+"\n"+answer, true)
	AdminAppHistory(ctx)
}
