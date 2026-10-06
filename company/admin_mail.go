// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"net/mail"
	"strings"

	"gitea.dev/modules/setting"
	"gitea.dev/services/context"
)

const (
	tplAdminMail     = "company/admin_mail"
	tplAdminMailRule = "company/admin_mail_rule"
)

// The mail screen: where the platform's messages go out through, and who
// hears about what. See company/mail.go and company/mailrules.go.

// AdminMail renders the list: the server, and one row per notification.
//
// The rows carry only what a list is for — what it is called, what it watches,
// whether it is on, and a way to remove it. Who receives it and what it says
// are on its own page: a list that tries to be an editor is neither.
func AdminMail(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Locale.TrString("company.mail.title")
	ctx.Data["MailServer"] = MailSettings(ctx)
	ctx.Data["MailPasswordSet"] = MailPasswordSet(ctx)
	ctx.Data["MailConfigured"] = MailSettings(ctx).Configured()
	ctx.Data["MailRules"] = MailRules(ctx)
	ctx.Data["MailEventList"] = MailEvents()
	ctx.HTML(http.StatusOK, tplAdminMail)
}

// AdminMailRule renders one notification: who hears it, and what it says.
func AdminMailRule(ctx *context.Context) {
	rule, found := MailRuleByID(ctx, ctx.PathParam("id"))
	if !found {
		ctx.NotFound(nil)
		return
	}
	ctx.Data["Title"] = rule.Name
	ctx.Data["Rule"] = rule
	ctx.Data["RuleTo"] = strings.Join(rule.To, "\n")
	ctx.Data["MailFields"] = MailFieldsFor(rule.Event)
	ctx.Data["MailConfigured"] = MailSettings(ctx).Configured()
	ctx.HTML(http.StatusOK, tplAdminMailRule)
}

// AdminMailRuleAdd creates one and opens it, because an empty rule on a list
// is a thing somebody has to remember to go and fill in.
func AdminMailRuleAdd(ctx *context.Context) {
	id, err := AddMailRule(ctx, strings.TrimSpace(ctx.FormString("name")), ctx.FormString("event"))
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		ctx.Redirect(mailPage())
		return
	}
	ctx.Redirect(mailPage() + "/" + id)
}

// AdminMailRuleSave stores what its own page says.
func AdminMailRuleSave(ctx *context.Context) {
	rule, found := MailRuleByID(ctx, ctx.PathParam("id"))
	if !found {
		ctx.NotFound(nil)
		return
	}
	rule.Name = strings.TrimSpace(ctx.FormString("name"))
	if rule.Name == "" {
		rule.Name = rule.Event
	}
	// One value per chip, so the list arrives as a list. SplitAddresses still
	// runs over it: it drops blanks and repeats, and a pasted "a@x, b@x" typed
	// into one chip is still two addresses.
	rule.To = SplitAddresses(strings.Join(ctx.Req.Form["to"], "\n"))
	rule.Subject = strings.TrimSpace(ctx.FormString("subject"))
	rule.Body = ctx.FormString("body")
	if err := UpdateMailRule(ctx, rule); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	} else {
		ctx.Flash.Success(ctx.Locale.TrString("company.mail.rules_saved"))
	}
	ctx.Redirect(mailPage() + "/" + rule.ID)
}

// AdminMailRulePreview renders a body the way a mail client would see it.
//
// Rendered here rather than in the browser so the preview goes through the
// same substitution the real message does (company/mailrules.go) — a preview
// with its own copy of that logic is a preview that can be wrong about the
// one thing it exists to show.
//
// Returned as JSON rather than as a page: this is an administrator's own
// HTML, and an endpoint that served it as a document would be a place to
// park a script and send somebody a link to it. The caller puts it in a
// sandboxed iframe.
func AdminMailRulePreview(ctx *context.Context) {
	ctx.JSON(http.StatusOK, map[string]string{
		"html": renderMailText(ctx.FormString("body"), mailPreviewFields(ctx), true),
	})
}

// mailPreviewFields are stand-ins, so the preview shows the shape of the
// message rather than empty gaps where the values will be.
func mailPreviewFields(ctx *context.Context) MailFields {
	return MailFields{
		"app":         "PO/report",
		"title":       ctx.Locale.TrString("company.mail.preview_title"),
		"requester":   ctx.Locale.TrString("company.mail.preview_requester"),
		"actor":       ctx.Locale.TrString("company.mail.preview_actor"),
		"reason":      ctx.Locale.TrString("company.mail.preview_reason"),
		"link":        setting.AppURL + "yoonchul/central-deploy/pulls/12",
		"severity":    "high",
		"permissions": ctx.Locale.TrString("company.mail.preview_permissions"),
		"summary":     ctx.Locale.TrString("company.mail.preview_summary"),
		"details":     ctx.Locale.TrString("company.mail.preview_details"),
	}
}

// AdminMailRuleToggle switches one on or off from the list.
func AdminMailRuleToggle(ctx *context.Context) {
	if err := ToggleMailRule(ctx, ctx.PathParam("id"), ctx.FormBool("enabled")); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	}
	ctx.Redirect(mailPage())
}

// AdminMailRuleDelete removes one.
func AdminMailRuleDelete(ctx *context.Context) {
	if err := DeleteMailRule(ctx, ctx.PathParam("id")); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	} else {
		ctx.Flash.Success(ctx.Locale.TrString("company.mail.rule_deleted"))
	}
	ctx.Redirect(mailPage())
}

// defaultMailPort is the standard submission port for each security mode.
func defaultMailPort(security string) int {
	switch security {
	case mailSecurityTLS:
		return 465
	case mailSecurityNone:
		return 25
	}
	return 587
}

func mailPage() string { return setting.AppSubURL + "/-/admin/company-mail" }

// AdminMailServerPost saves where mail goes out through.
func AdminMailServerPost(ctx *context.Context) {
	server := MailServer{
		Host:       strings.TrimSpace(ctx.FormString("host")),
		Port:       ctx.FormInt("port"),
		Username:   strings.TrimSpace(ctx.FormString("username")),
		From:       strings.TrimSpace(ctx.FormString("from")),
		FromName:   strings.TrimSpace(ctx.FormString("from_name")),
		Security:   ctx.FormString("security"),
		SkipVerify: ctx.FormBool("skip_verify"),
	}
	switch server.Security {
	case mailSecurityNone, mailSecurityStartTLS, mailSecurityTLS:
	default:
		server.Security = mailSecurityStartTLS
	}
	if server.Port == 0 {
		server.Port = defaultMailPort(server.Security) // the field's placeholder is not a value
	}
	// Parsed rather than pattern-matched, for the same reason the support
	// address is: every message the platform sends carries this, and a typo
	// here is a bounce nobody sees.
	if server.From != "" {
		if _, err := mail.ParseAddress(server.From); err != nil {
			ctx.Flash.Error(ctx.Locale.TrString("company.adminsettings.email_invalid", server.From))
			ctx.Redirect(mailPage())
			return
		}
	}
	if server.Host != "" && server.Sender() == "" {
		ctx.Flash.Error(ctx.Locale.TrString("company.mail.from_required"))
		ctx.Redirect(mailPage())
		return
	}
	if ctx.FormBool("clear_password") {
		if err := ClearMailPassword(ctx); err != nil {
			ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
			ctx.Redirect(mailPage())
			return
		}
	}
	if err := SaveMailSettings(ctx, server, ctx.FormString("password")); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	} else {
		ctx.Flash.Success(ctx.Locale.TrString("company.mail.saved"))
	}
	ctx.Redirect(mailPage())
}

// AdminMailTest sends one message to whoever asked for it.
//
// To the administrator pressing the button, not to a rule's recipients: the
// question this answers is "does the server work", and answering it should
// not mail a department.
func AdminMailTest(ctx *context.Context) {
	to := SplitAddresses(ctx.FormString("test_to"))
	if len(to) == 0 && ctx.Doer != nil {
		to = SplitAddresses(ctx.Doer.Email)
	}
	err := SendPlatformMail(ctx, to,
		ctx.Locale.TrString("company.mail.test_subject"),
		ctx.Locale.TrString("company.mail.test_body"))
	if err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	} else {
		ctx.Flash.Success(ctx.Locale.TrString("company.mail.test_sent", strings.Join(to, ", ")))
	}
	ctx.Redirect(mailPage())
}
