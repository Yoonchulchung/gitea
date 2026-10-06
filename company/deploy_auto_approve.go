// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/process"
	issue_service "gitea.dev/services/issue"
	pull_service "gitea.dev/services/pull"
)

// AI auto-approval: a deploy request that changes only code — no permission,
// no package, no removal — is judged by the platform AI (company/platform_ai.go),
// and merged in the name of the administrator who turned it on when the verdict is safe. Anything else waits for a person.
// See docs/company/ai-agent.md.

// autoApproveDiffMaxChars is the largest change the AI is trusted to judge in
// one read; past it the review would be chunked, and no chunk sees the whole.
const autoApproveDiffMaxChars = 60000

// autoApproveFacts is what decides, before any AI is asked, whether a request
// may be decided without a person at all.
type autoApproveFacts struct {
	// StartupState is the submitted version's startup check; only "passed"
	// proves it builds with what is already approved, transitive packages included.
	StartupState string
	// VersionMismatch is a department that changed its code after asking: the
	// startup check then describes other files than the request's.
	VersionMismatch bool
	Removal         bool
	Permissions     int
	PendingPackages int
	DiffChars       int
	// RiskyCode is what riskyAdditions found; any of it needs a person,
	// whatever the AI concludes.
	RiskyCode []aiFinding
}

// autoApproveBlocker is why a request needs a person, or "" when the AI may decide.
func autoApproveBlocker(f autoApproveFacts) string {
	switch {
	case f.Permissions > 0:
		return fmt.Sprintf("권한 요청 %d건이 포함되어 있습니다. 권한은 관리자가 직접 결정합니다.", f.Permissions)
	case f.Removal:
		return "앱 삭제 요청입니다."
	case f.VersionMismatch:
		return "요청한 뒤 부서 저장소의 코드가 바뀌어, 시작 검사가 이 요청의 버전을 확인하지 못했습니다. 지금 코드로 배포 요청을 다시 내면 다시 검토합니다."
	case f.StartupState != "passed":
		return fmt.Sprintf("앱 시작 검사를 통과하지 못했습니다(상태: %s).", cmp.Or(f.StartupState, "없음"))
	case f.PendingPackages > 0:
		return fmt.Sprintf("아직 허용되지 않은 패키지 %d개가 필요합니다.", f.PendingPackages)
	case f.DiffChars == 0:
		return "변경 내용을 읽을 수 없습니다."
	case f.DiffChars > autoApproveDiffMaxChars:
		return fmt.Sprintf("변경이 너무 큽니다(%d자, 자동 판정 한도 %d자).", f.DiffChars, autoApproveDiffMaxChars)
	case len(f.RiskyCode) > 0:
		return "사람이 확인해야 하는 기능이 코드에 추가됐습니다: " + riskyLabels(f.RiskyCode) + ". 아래 표의 줄을 확인하세요."
	}
	return ""
}

type autoApproveVerdict struct {
	Safe     bool
	Purpose  string // optional: what the code does, for the comment
	Reason   string
	Findings []aiFinding // optional: what the reason points at, file by file
}

// parseAutoApproveVerdict reads the model's answer strictly: anything that is
// not the asked-for object, with both fields present, is not an approval.
func parseAutoApproveVerdict(reply string) (autoApproveVerdict, bool) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return autoApproveVerdict{}, false
	}
	var raw struct {
		Safe     *bool       `json:"safe"`
		Purpose  string      `json:"purpose"`
		Reason   string      `json:"reason"`
		Findings []aiFinding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &raw); err != nil || raw.Safe == nil || strings.TrimSpace(raw.Reason) == "" {
		return autoApproveVerdict{}, false
	}
	return autoApproveVerdict{Safe: *raw.Safe, Purpose: strings.TrimSpace(raw.Purpose), Reason: strings.TrimSpace(raw.Reason), Findings: raw.Findings}, true
}

// ScheduleDeployAIReview has the platform AI review prID in the background,
// when it has something to do: approve it (an administrator delegated that)
// or warn about it (a security alert would reach somebody). The request is
// already open; whatever happens here, a person can still decide it.
func ScheduleDeployAIReview(central *repo_model.Repository, centralOwner *user_model.User, prID int64, deptOwner, deptName string) {
	// Not the caller's request context: deciding whether to review must not
	// fail because the browser that submitted the request has gone.
	ctx := graceful.GetManager().ShutdownContext()
	delegate := AutoApproveDelegate(ctx)
	alerting := MailEventReady(ctx, MailEventSecurityAlert)
	if delegate == nil && !alerting {
		log.Info("company: deploy request %d: no platform AI review (auto-approval off, no security alert rule)", prID)
		return
	}
	var run *autoApproveRun
	if delegate != nil {
		var fresh bool
		if run, fresh = startAutoApproveRun(prID); !fresh {
			return // already being reviewed — by the submission or by the sweeper
		}
	}
	go func() {
		ctx, _, finished := process.GetManager().AddContext(graceful.GetManager().ShutdownContext(), fmt.Sprintf("company: platform AI review of deploy request %d", prID))
		defer finished()
		defer finishAutoApproveRun(prID)
		defer recoverBackground("platform AI review of deploy request %d", prID)
		reviewDeployRequest(ctx, delegate, alerting, run, central, centralOwner, prID, deptOwner, deptName)
	}()
}

func reviewDeployRequest(ctx context.Context, delegate *user_model.User, alerting bool, run *autoApproveRun, central *repo_model.Repository, centralOwner *user_model.User, prID int64, deptOwner, deptName string) {
	pr, err := waitForMergeCheck(ctx, prID)
	if err != nil {
		log.Error("company: platform AI review of deploy request %d: %v", prID, err)
		return
	}
	// Holds are about approval, so they are only said when approval was asked
	// for — and then a person has to hear about it, not only find it later.
	permissions := "없음"
	if requests := LoadPermissionRequests(deptOwner, deptName, pr.ID); len(requests) > 0 {
		permissions = describePermissionRequests(requests)
	}
	holdCard := func(card aiReviewCard) {
		if delegate == nil {
			return
		}
		log.Info("company: deploy request #%d for %s/%s left for an administrator: %s", pr.ID, deptOwner, deptName, card.Summary)
		card.Title = "자동 승인 보류"
		card.Footer = "관리자 검토가 필요합니다."
		if notifyMail(ctx, MailEventApprovalNeeded, MailFields{
			"app": deptOwner + "/" + deptName, "title": pr.Issue.Title, "requester": requesterName(ctx, pr),
			"reason": card.Summary, "permissions": permissions, "link": pr.Issue.HTMLURL(ctx),
		}) > 0 {
			card.Footer = "관리자 검토가 필요합니다. 관리자에게 확인 요청 메일을 보냈습니다."
		}
		postAutoApproveComment(ctx, delegate, central, pr.Issue, card)
	}
	hold := func(reason string) { holdCard(aiReviewCard{Summary: reason}) }
	if PlatformAIUnavailableReason(ctx) != "" {
		hold("플랫폼 AI 설정이 완료되지 않았습니다.")
		return
	}

	gitRepo, err := git.OpenRepository(ctx, central)
	if err != nil {
		hold("중앙 저장소를 열 수 없습니다.")
		return
	}
	defer gitRepo.Close()
	head, err := gitRepo.GetBranchCommit(ctx, pr.HeadBranch)
	if err != nil {
		return // superseded or withdrawn while waiting: nothing left to decide
	}
	headID := head.ID.String()

	var diff strings.Builder
	_ = gitRepo.GetDiff(ctx, central.DefaultBranch+"..."+headID, &diff) // an unreadable diff is DiffChars 0, which holds
	requests := LoadPermissionRequests(deptOwner, deptName, pr.ID)
	requirements := deployRequestFile(ctx, gitRepo, pr.HeadBranch, deployPathPrefix(deptOwner, deptName), "requirements.txt")
	appEnv, _, _ := AppEnvNames(deptOwner, deptName) // names the department set; reading them is the intended use
	startupState, versionMismatch := requestStartupState(ctx, head, gitRepo, deptOwner, deptName)
	facts := autoApproveFacts{
		StartupState:    startupState,
		VersionMismatch: versionMismatch,
		Removal:         isRemovalRequest(pr),
		Permissions:     len(requests),
		PendingPackages: len(pendingPackages(reviewPackages(requirements, SettingsFor(deptOwner, deptName), requests))),
		DiffChars:       diff.Len(),
		RiskyCode:       riskyAdditions(diff.String(), appEnv...),
	}
	blocker := autoApproveBlocker(facts)
	// A request that cannot be approved here is still worth warning about —
	// one asking for more access is the one to read most carefully.
	if blocker != "" && !alerting {
		holdCard(aiReviewCard{Summary: blocker, Security: facts.RiskyCode})
		return
	}

	var alertFields MailFields
	if alerting {
		alertFields = MailFields{"app": deptOwner + "/" + deptName, "title": pr.Issue.Title, "link": pr.Issue.HTMLURL(ctx), "requester": requesterName(ctx, pr)}
	}
	var trace []string
	verdict, alert, err := runSecurityAgent(ctx, gitRepo, pr, deptOwner, deptName, diff.String(), alertFields, &trace)
	// What the verdict rests on, under every comment it leads to.
	checked := append([]string{"요청에 포함된 변경 내용"}, trace...)
	model := PlatformAIModel(ctx)
	log.Info("company: platform AI review of deploy request #%d for %s/%s: safe=%v err=%v looked at %v — %s", pr.ID, deptOwner, deptName, verdict.Safe, err, trace, verdict.Reason)
	if alert != nil {
		commenter := delegate
		if commenter == nil {
			commenter = centralOwner
		}
		card := aiReviewCard{Title: "보안 경고 (" + alert.Severity + ")", Purpose: verdict.Purpose, Summary: alert.Summary, Security: verdict.Findings, Model: model, Checked: checked, Footer: "관리자에게 경고 메일을 보냈습니다."}
		if alert.Sent == 0 {
			card.Footer = "경고 메일을 보내지 못했습니다. 메일 서버 설정을 확인하세요."
		}
		postAutoApproveComment(ctx, commenter, central, pr.Issue, card)
	}
	if delegate == nil {
		if err != nil {
			log.Error("company: platform AI review of deploy request %d: %v", pr.ID, err)
		}
		return
	}
	if blocker != "" {
		holdCard(aiReviewCard{Summary: blocker, Security: facts.RiskyCode})
		return
	}
	if err != nil {
		log.Error("company: AI auto-approval of deploy request %d: %v", pr.ID, err)
		hold("AI 판정을 받지 못했습니다.")
		return
	}
	if !verdict.Safe || alert != nil {
		holdCard(aiReviewCard{Purpose: verdict.Purpose, Summary: verdict.Reason, Security: verdict.Findings, Model: model, Checked: checked})
		return
	}

	// The request page counts down and can stop it (company/auto_approve_wait.go).
	if stoppedBy, ok := run.waitBeforeMerge(ctx, AutoApproveDelay(ctx)); !ok {
		if stoppedBy != "" {
			log.Info("company: AI auto-approval of deploy request #%d stopped by %s", pr.ID, stoppedBy)
			postAutoApproveComment(ctx, delegate, central, pr.Issue, aiReviewCard{Title: "자동 승인 중지", Summary: "@" + stoppedBy + " 님이 자동 배포를 멈췄습니다. 관리자가 직접 결정합니다.", Purpose: verdict.Purpose, Model: model, Checked: checked})
		}
		return
	}

	// Re-read: the request may have been decided or withdrawn while the AI was reading it.
	if pr, err = issues_model.GetPullRequestByID(ctx, pr.ID); err != nil {
		return
	}
	perm, err := access_model.GetDoerRepoPermission(ctx, central, delegate)
	if err != nil {
		hold("위임한 관리자의 저장소 권한을 확인할 수 없습니다.")
		return
	}
	if err := pull_service.CheckPullMergeable(ctx, delegate, &perm, pr, pull_service.MergeCheckTypeGeneral, repo_model.MergeStyleMerge, false); err != nil {
		if !errors.Is(err, pull_service.ErrIsClosed) && !errors.Is(err, pull_service.ErrHasMerged) {
			hold("지금은 병합할 수 없는 상태입니다: " + err.Error())
		}
		return
	}
	_, body, err := pull_service.GetDefaultMergeMessage(ctx, gitRepo, pr, repo_model.MergeStyleMerge)
	if err != nil {
		hold("병합 메시지를 만들 수 없습니다.")
		return
	}
	postAutoApproveComment(ctx, delegate, central, pr.Issue, aiReviewCard{Title: "자동 승인", Purpose: verdict.Purpose, Summary: verdict.Reason, Footer: "보안상 문제가 발견되지 않아 배포를 승인했습니다.", Model: model, Checked: checked})
	mergeMessage := pr.Issue.Title + "\n\n승인자: AI 자동 승인 (위임: " + delegate.Name + ")\n" + body
	// headID pins the merge to the exact change the AI read; a newer push fails it.
	if err := pull_service.Merge(pr, delegate, repo_model.MergeStyleMerge, headID, mergeMessage, false); err != nil {
		log.Error("company: AI auto-approval of deploy request %d: merge: %v", pr.ID, err)
		hold("승인 후 병합에 실패했습니다. 관리자가 직접 승인해 주세요.")
		return
	}
	log.Info("company: deploy request #%d for %s/%s approved by AI on behalf of %s", pr.ID, deptOwner, deptName, delegate.Name)
}

// waitForMergeCheck returns the request once Gitea's conflict check, queued
// when it was opened, has finished — a request still being checked cannot merge.
func waitForMergeCheck(ctx context.Context, prID int64) (*issues_model.PullRequest, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	timeout := time.After(2 * time.Minute)
	for {
		pr, err := issues_model.GetPullRequestByID(ctx, prID)
		if err != nil {
			return nil, err
		}
		if !pr.IsChecking() {
			if err := pr.LoadIssue(ctx); err != nil {
				return nil, err
			}
			return pr, pr.Issue.LoadRepo(ctx) // HTMLURL needs it; without it the link panicked and took Gitea down
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, errors.New("conflict check did not finish")
		case <-ticker.C:
		}
	}
}

func postAutoApproveComment(ctx context.Context, doer *user_model.User, central *repo_model.Repository, issue *issues_model.Issue, card aiReviewCard) {
	// The card's marker keeps it out of the department's "why was this rejected" list.
	if _, err := issue_service.CreateIssueComment(ctx, doer, central, issue, card.Markdown(), nil); err != nil {
		log.Error("company: AI auto-approval: post comment: %v", err)
	}
}

// describePermissionRequests lists what a request asks to be allowed, for a mail.
func describePermissionRequests(requests []PermissionRequest) string {
	parts := make([]string, 0, len(requests))
	for _, r := range requests {
		part := r.Kind + ": " + r.Value
		if r.Reason != "" {
			part += " (" + r.Reason + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}
