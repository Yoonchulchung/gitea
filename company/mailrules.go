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
)

// MailEvents is the order the settings page lists them in: the life of a
// request, start to finish.
func MailEvents() []string {
	return []string{MailEventRequested, MailEventApproved, MailEventRejected, MailEventFailed}
}

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
	// Sending is a running-platform concern. The deploy worker calls this from
	// a failure path that unit tests exercise directly, where there is no
	// database to read a rule out of and nobody to send to.
	if !platformRunning.Load() {
		return
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
		}
	}
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
