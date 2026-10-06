// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"html"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	system_model "gitea.dev/models/system"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
)

// Who hears about what.
//
// The platform knows when a department asks to deploy, when an administrator
// decides, and when a build fails. Until now it knew that privately: the
// people who needed to act on it found out by opening a page, which meant
// remembering to. A rule is the standing answer to "tell whom, when".
//
// One rule per event rather than a list of subscriptions, because the
// question an administrator asks is "who should hear about a failed deploy",
// not "what should this address hear about". The subject and body are theirs
// to write — a notification worded by the platform is a notification nobody
// reads twice.

const settingKeyMailRules = "company.mail.rules"

// platformRunning is set once the platform has started for real
// (company/appinit.go). Until then there is no database behind these reads.
var platformRunning atomic.Bool

// The events worth a message. Each is a moment where somebody outside the
// screen has to do something, or would want to know that they do not.
const (
	MailEventRequested = "deploy_requested"
	MailEventApproved  = "deploy_approved"
	MailEventRejected  = "deploy_rejected"
	MailEventFailed    = "deploy_failed"
	// MailEventSecurityAlert is sent by the platform AI itself, when its review
	// of a deploy request decides an administrator must hear about it now.
	MailEventSecurityAlert = "security_alert"
	// MailEventApprovalNeeded is sent when AI auto-approval holds a request —
	// above all one that asks for a permission — so a person is told to decide it.
	MailEventApprovalNeeded = "approval_needed"
)

// MailEvents is the order the settings page lists them in: the life of a
// request, start to finish, then what the platform AI raises on its own.
func MailEvents() []string {
	return []string{MailEventRequested, MailEventApprovalNeeded, MailEventApproved, MailEventRejected, MailEventFailed, MailEventSecurityAlert}
}

// MailFieldsFor names the placeholders a rule for event can use.
func MailFieldsFor(event string) []string {
	fields := []string{"app", "title", "requester", "actor", "reason", "link"}
	switch event {
	case MailEventSecurityAlert:
		fields = []string{"app", "title", "requester", "link", "severity", "summary", "details"}
	case MailEventApprovalNeeded:
		fields = []string{"app", "title", "requester", "reason", "permissions", "link"}
	}
	return fields
}

// A security alert rule starts with words already in it: the AI decides when
// it goes out, so a rule left blank would silently swallow the first alert.
const (
	securityAlertDefaultSubject = "[보안 경고 · {{severity}}] {{app}} — {{summary}}"
	securityAlertDefaultBody    = `<p>플랫폼 AI가 배포 요청을 검토하다 보안 문제를 발견해 이 메일을 보냈습니다.</p>
<p><b>앱</b>: {{app}}<br><b>요청</b>: {{title}} ({{requester}})<br><b>심각도</b>: {{severity}}</p>
<p><b>요약</b>: {{summary}}</p>
<p style="white-space:pre-wrap">{{details}}</p>
<p><a href="{{link}}">요청 열기</a></p>`

	approvalNeededDefaultSubject = "[승인 필요] {{app}} — {{title}}"
	approvalNeededDefaultBody    = `<p>AI 자동 승인이 이 배포 요청을 보류했습니다. 관리자가 확인하고 결정해야 합니다.</p>
<p><b>앱</b>: {{app}}<br><b>요청</b>: {{title}} ({{requester}})</p>
<p><b>보류 이유</b>: {{reason}}</p>
<p><b>요청된 권한</b>: {{permissions}}</p>
<p><a href="{{link}}">요청 열기</a></p>`
)

// MailRule is one standing instruction: when this happens, tell these people
// this. Several may watch the same event — the people who approve a deploy
// and the department that asked for it want different words.
type MailRule struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Event   string   `json:"event"`
	Enabled bool     `json:"enabled"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	// Body is HTML, written by an administrator. The values substituted into
	// it are escaped; the markup around them is theirs.
	Body string `json:"body"`
}

var (
	mailRulesMu     sync.RWMutex
	mailRulesLoaded bool
	mailRules       []MailRule
)

func loadMailRules(ctx context.Context) {
	mailRulesMu.RLock()
	loaded := mailRulesLoaded
	mailRulesMu.RUnlock()
	if loaded {
		return
	}
	_, all, err := system_model.GetAllSettings(ctx)
	if err != nil {
		log.Error("company: reading the mail rules: %v", err)
		return
	}
	var rules []MailRule
	if raw := all[settingKeyMailRules]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &rules); err != nil {
			log.Error("company: the stored mail rules are unreadable: %v", err)
		}
	}

	mailRulesMu.Lock()
	defer mailRulesMu.Unlock()
	mailRules, mailRulesLoaded = rules, true
}

// MailRules is every rule, in the order they were added.
func MailRules(ctx context.Context) []MailRule {
	loadMailRules(ctx)
	mailRulesMu.RLock()
	defer mailRulesMu.RUnlock()
	return slices.Clone(mailRules)
}

// MailRuleByID returns one, and whether it is there at all.
func MailRuleByID(ctx context.Context, id string) (MailRule, bool) {
	for _, r := range MailRules(ctx) {
		if r.ID == id {
			return r, true
		}
	}
	return MailRule{}, false
}

// saveMailRules replaces the whole set.
func saveMailRules(ctx context.Context, rules []MailRule) error {
	raw, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	if err := system_model.SetSettings(ctx, map[string]string{settingKeyMailRules: string(raw)}); err != nil {
		return err
	}
	mailRulesMu.Lock()
	mailRulesLoaded = false
	mailRulesMu.Unlock()
	return nil
}

// AddMailRule creates one, switched off, and returns its id. Off because a
// rule with nobody on it and nothing to say would otherwise start failing
// sends the moment it existed; the detail page is where it is filled in.
func AddMailRule(ctx context.Context, name, event string) (string, error) {
	if !slices.Contains(MailEvents(), event) {
		return "", userKeyError("company.mail.err_unknown_event")
	}
	rule := MailRule{ID: newMailRuleID(), Name: name, Event: event}
	if rule.Name == "" {
		rule.Name = event
	}
	switch event {
	case MailEventSecurityAlert:
		rule.Subject, rule.Body = securityAlertDefaultSubject, securityAlertDefaultBody
	case MailEventApprovalNeeded:
		rule.Subject, rule.Body = approvalNeededDefaultSubject, approvalNeededDefaultBody
	}
	return rule.ID, saveMailRules(ctx, append(MailRules(ctx), rule))
}

// UpdateMailRule replaces one by id, leaving the others alone.
func UpdateMailRule(ctx context.Context, rule MailRule) error {
	rules := MailRules(ctx)
	i := slices.IndexFunc(rules, func(r MailRule) bool { return r.ID == rule.ID })
	if i < 0 {
		return userKeyError("company.mail.err_no_rule")
	}
	rules[i] = rule
	return saveMailRules(ctx, rules)
}

// ToggleMailRule switches one on or off — the list's own control, so that
// silencing a notification does not mean opening it and clearing its
// recipients.
func ToggleMailRule(ctx context.Context, id string, enabled bool) error {
	rules := MailRules(ctx)
	i := slices.IndexFunc(rules, func(r MailRule) bool { return r.ID == id })
	if i < 0 {
		return userKeyError("company.mail.err_no_rule")
	}
	rules[i].Enabled = enabled
	return saveMailRules(ctx, rules)
}

// DeleteMailRule removes one.
func DeleteMailRule(ctx context.Context, id string) error {
	rules := slices.DeleteFunc(MailRules(ctx), func(r MailRule) bool { return r.ID == id })
	return saveMailRules(ctx, rules)
}

// newMailRuleID names a rule for the URL of its own page. Random rather than
// a position, so deleting one does not rename the rest.
func newMailRuleID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// MailFields are the values a rule's subject and body can name, as
// {{app}}, {{title}} and so on.
type MailFields map[string]string

// NotifyMail sends the message standing for this event, if there is one.
//
// Best-effort and never in the caller's way: this runs from the middle of a
// deploy request being submitted or a build failing, and a mail server that
// is down must not fail either. The failure is logged, with the event, so an
// administrator can find it in the platform log.
func NotifyMail(ctx context.Context, event string, fields MailFields) {
	notifyMail(ctx, event, fields)
}

// MailEventReady reports whether a message for event would reach anyone.
func MailEventReady(ctx context.Context, event string) bool {
	if !MailSettings(ctx).Configured() {
		return false
	}
	return slices.ContainsFunc(MailRules(ctx), func(r MailRule) bool {
		return r.Event == event && r.Enabled && len(r.To) > 0 && strings.TrimSpace(r.Subject) != "" && strings.TrimSpace(r.Body) != ""
	})
}

// notifyMail is NotifyMail that says how many rules actually sent.
func notifyMail(ctx context.Context, event string, fields MailFields) (sent int) {
	// Sending is a running-platform concern. The deploy worker calls this from
	// a failure path that unit tests exercise directly, where there is no
	// database to read a rule out of and nobody to send to.
	if !platformRunning.Load() {
		return 0
	}
	for _, rule := range MailRules(ctx) {
		if rule.Event != event || !rule.Enabled || len(rule.To) == 0 {
			continue
		}
		subject := renderMailText(rule.Subject, fields, false)
		body := renderMailText(rule.Body, fields, true)
		if strings.TrimSpace(subject) == "" || strings.TrimSpace(body) == "" {
			continue // a rule with nothing to say
		}
		if err := SendPlatformMail(ctx, rule.To, subject, body); err != nil {
			log.Error("company: the %q notification (%s) could not be sent: %v", rule.Name, event, err)
			continue
		}
		sent++
	}
	return sent
}

// renderMailText substitutes {{name}} placeholders.
//
// Not Go's template engine: an administrator writing a notification is
// writing prose with a few names in it, and a syntax error in that prose
// should not be able to stop a deploy. Values are HTML-escaped in the body —
// an app name is data, and the markup around it is what the administrator
// wrote.
func renderMailText(text string, fields MailFields, escape bool) string {
	if text == "" {
		return ""
	}
	pairs := make([]string, 0, len(fields)*2)
	for name, value := range fields {
		if escape {
			value = html.EscapeString(value)
		}
		pairs = append(pairs, "{{"+name+"}}", value)
	}
	return strings.NewReplacer(pairs...).Replace(text)
}
