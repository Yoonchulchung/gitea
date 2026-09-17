// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"strings"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	gitea_context "gitea.dev/services/context"
	"gitea.dev/services/mailer"
	sender_service "gitea.dev/services/mailer/sender"
)

// A deploy request that nobody notices is a department waiting on somebody
// who does not know they are being waited on.
//
// The platform already shows the queue — on the admin dashboard, on the org's
// own page — but both of those need somebody to go and look. The one thing
// that reaches a person who is not looking is mail, and the address it goes
// to is the same one the department gate shows, set in admin settings, so
// there is one answer to "who is responsible" rather than two that drift.

// NotifyDeployRequest tells the contact that a department is waiting.
//
// Best-effort by construction: the request is already registered by the time
// this runs, and failing the submission because a mail server was slow would
// lose work over a notification. Everything here reports by logging.
func NotifyDeployRequest(ctx *gitea_context.Context, deptRepo *repo_model.Repository, actor string, issue *issues_model.Issue) {
	to := SupportEmail(ctx)
	if to == "" {
		return // nobody named; the dashboards still have it
	}
	if setting.MailService == nil {
		// Said once per request rather than never: an administrator who set
		// a contact address is expecting mail, and silence here would look
		// like the feature working.
		log.Warn("company: a deploy request for %s was not mailed to %s — mail is switched off ([mailer] ENABLED)",
			deptRepo.FullName(), to)
		return
	}

	link := setting.AppURL + strings.TrimPrefix(deptRepo.Link()+"/deploy", "/")
	if reviewLink := deployRequestReviewLink(issue); reviewLink != "" {
		link = reviewLink
	}

	subject := fmt.Sprintf("[%s] Deploy request: %s", deptRepo.OwnerName, issue.Title)
	// Plain text, and short. Whoever opens this is deciding whether to go and
	// look now, so it answers that and nothing else: who, which app, and
	// where to go.
	body := fmt.Sprintf(
		"%s asked to deploy %s.\n\n%s\n\nReview it here:\n%s\n",
		actor, deptRepo.FullName(), issue.Title, link)

	mailer.SendAsync(sender_service.NewMessage(to, subject, body))
	log.Info("company: deploy request for %s mailed to %s", deptRepo.FullName(), to)
}

// deployRequestReviewLink is where the reviewer should land, or "" when the
// request has no number yet to point at.
func deployRequestReviewLink(issue *issues_model.Issue) string {
	if issue == nil || issue.Repo == nil || issue.Index == 0 {
		return ""
	}
	return fmt.Sprintf("%s%s/pulls/%d", setting.AppURL, strings.TrimPrefix(issue.Repo.Link(), "/"), issue.Index)
}
