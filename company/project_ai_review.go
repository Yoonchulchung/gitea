// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"strings"

	"gitea.dev/models/db"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
)

// The platform AI looks at a new project request when an administrator has
// delegated that: one that plausibly serves the company's work is approved in
// their name, anything else stays for a person with the AI's reason beside it.

const projectReviewPrompt = `You review requests to create a project on a company's internal app platform. Employees build small business tools here: forms, dashboards, reports, internal lookups, spreadsheet helpers, automations.

Decide whether the project plausibly serves the company's work. Be lenient: approve anything that could reasonably be a work tool, even if the name or description is short, informal, a test, or vague. Only hold a request back when it is clearly unrelated to work (a personal hobby, a game, entertainment), clearly inappropriate or abusive, or meaningless filler.

Reply with only a JSON object: {"approve": true or false, "reason": "one short sentence in Korean"}`

type projectReviewVerdict struct {
	Approve bool
	Reason  string
}

// parseProjectReviewVerdict reads the answer strictly: anything but the asked-for object is no approval.
func parseProjectReviewVerdict(reply string) (projectReviewVerdict, bool) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return projectReviewVerdict{}, false
	}
	var raw struct {
		Approve *bool  `json:"approve"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &raw); err != nil || raw.Approve == nil || strings.TrimSpace(raw.Reason) == "" {
		return projectReviewVerdict{}, false
	}
	return projectReviewVerdict{Approve: *raw.Approve, Reason: strings.TrimSpace(raw.Reason)}, true
}

// ScheduleProjectAIReview has the platform AI decide req in the background,
// when an administrator delegated that. A person can still decide it meanwhile.
func ScheduleProjectAIReview(req *ApprovalRequest, requester *user_model.User) {
	ctx := graceful.GetManager().ShutdownContext()
	delegate := ProjectAIDelegate(ctx)
	if delegate == nil {
		return
	}
	go func() {
		defer recoverBackground("AI review of project request %d", req.ID)
		reviewProjectRequest(ctx, delegate, req, requester)
	}()
}

func reviewProjectRequest(ctx context.Context, delegate *user_model.User, req *ApprovalRequest, requester *user_model.User) {
	prompt := fmt.Sprintf("Project name: %s\nDescription: %s\nRequested by: %s", req.Name, orDefault(req.Description, "(none)"), requester.Name)
	reply, err := platformAIChat(ctx, aiPurposeProjectReview, delegate.Name, req.Name, projectReviewPrompt, prompt)
	if err != nil {
		log.Warn("company: AI review of project request %d: %v", req.ID, err)
		return // stays for a person
	}
	verdict, ok := parseProjectReviewVerdict(reply)
	if !ok {
		log.Warn("company: AI review of project request %d: unreadable answer", req.ID)
		return
	}
	if _, err := db.GetEngine(ctx).ID(req.ID).Cols("ai_note").Update(&ApprovalRequest{AINote: verdict.Reason}); err != nil {
		log.Error("company: recording the AI note on project request %d: %v", req.ID, err)
	}
	if !verdict.Approve {
		log.Info("company: AI held project request %d (%s): %s", req.ID, req.Name, verdict.Reason)
		return
	}
	if _, repo, err := ApproveProjectRequest(ctx, delegate, req.ID); err != nil {
		log.Warn("company: AI approval of project request %d: %v", req.ID, err) // decided meanwhile, or the name was taken
	} else {
		log.Info("company: AI approved project request %d on behalf of %s, creating %s", req.ID, delegate.Name, repo.FullName())
	}
}
