// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/graceful"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	gitea_context "gitea.dev/services/context"
)

// Security events: the moments that look like someone attacking the
// platform, written to one log the moment they happen so "were we attacked"
// is answered by one page rather than every app's logs. Each one was also
// refused or failed; this records that it was tried. See
// docs/company/security-events.md.
const (
	SecLoginFailed     = "login_failed"
	SecBan             = "ban"
	SecScanner         = "scanner"
	SecProbe           = "probe"
	SecRateLimit       = "rate_limit"
	SecCrossSite       = "cross_site"
	SecBodyTooLarge    = "body_too_large"
	SecForbidden       = "forbidden"
	SecOutboundRefused = "outbound_refused"
	SecDownloadBlocked = "download_blocked"
)

// SecurityKinds is the order the page lists them in.
var SecurityKinds = []string{
	SecLoginFailed, SecBan, SecScanner, SecProbe, SecRateLimit, SecCrossSite,
	SecForbidden, SecBodyTooLarge, SecOutboundRefused, SecDownloadBlocked,
}

type SecurityEvent struct {
	At     time.Time `json:"t"`
	Kind   string    `json:"k"`
	Source string    `json:"s,omitempty"` // the account or address it came from; empty for an app's own traffic
	App    string    `json:"a,omitempty"`
	Detail string    `json:"d,omitempty"`
}

const (
	securityLogName   = "events.log"
	securityRecentCap = 2000
	// An alert covers this window, and is sent at most once per cooldown.
	securityAlertWindow   = 10 * time.Minute
	securityAlertCooldown = time.Hour
	// Thresholds within one window. A single failed sign-in or a stray probe
	// is the internet's background noise; these are what is not.
	securityAlertTotal  = 30
	securityAlertLogins = 10
)

var securityLog struct {
	mu        sync.Mutex
	f         *os.File
	size      int64
	recent    []SecurityEvent // newest last, for the alert
	lastAlert time.Time
}

func securityLogDir() string { return filepath.Join(setting.AppDataPath, "company-security") }

// RecordSecurityEvent writes one event. Best-effort: a full disk must not
// turn a refused request into an error.
func RecordSecurityEvent(ev SecurityEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	securityLog.mu.Lock()
	defer securityLog.mu.Unlock()
	securityLog.recent = append(securityLog.recent, ev)
	if n := len(securityLog.recent); n > securityRecentCap {
		securityLog.recent = securityLog.recent[n-securityRecentCap:]
	}
	if securityLog.f == nil || securityLog.size >= appLogMaxBytes {
		if securityLog.f != nil {
			_ = securityLog.f.Close()
			securityLog.f = nil
		}
		if err := os.MkdirAll(securityLogDir(), 0o700); err != nil {
			log.Warn("company: security log: %v", err)
			return
		}
		f, err := openRotatingLog(securityLogDir(), securityLogName)
		if err != nil {
			log.Warn("company: security log: %v", err)
			return
		}
		securityLog.f, securityLog.size = f, 0
		if fi, err := f.Stat(); err == nil {
			securityLog.size = fi.Size()
		}
	}
	n, _ := securityLog.f.Write(append(line, '\n'))
	securityLog.size += int64(n)
	maybeSendSecurityAlert(ev.At)
}

// securityKindForBlock names the event a proxy refusal stands for, or ""
// for refusals that are the platform working normally (a sign-in redirect).
func securityKindForBlock(reason string) (kind, detail string) {
	switch {
	case reason == "scanner user agent":
		return SecScanner, ""
	case strings.HasPrefix(reason, "probe for "):
		return SecProbe, appRelativePath(strings.TrimPrefix(reason, "probe for ")) // the same probe counts once across apps
	case reason == "rate limit":
		return SecRateLimit, ""
	case reason == "cross-site request":
		return SecCrossSite, ""
	case reason == "body over limit":
		return SecBodyTooLarge, ""
	case strings.HasPrefix(reason, "not a member of "):
		return SecForbidden, ""
	}
	return "", ""
}

// ReadSecurityEvents returns the events since a time, oldest first, from the
// log and its rotated copies.
func ReadSecurityEvents(since time.Time) []SecurityEvent {
	var out []SecurityEvent
	base := filepath.Join(securityLogDir(), securityLogName)
	files := []string{base}
	for i := 1; i <= 5; i++ { // openRotatingLog keeps five
		files = append([]string{base + "." + strconv.Itoa(i)}, files...)
	}
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var ev SecurityEvent
			if json.Unmarshal(sc.Bytes(), &ev) == nil && !ev.At.Before(since) {
				out = append(out, ev)
			}
		}
		_ = f.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

type securityCount struct {
	Name   string
	Count  int
	Kinds  []string
	LastAt time.Time
}

type securityBucket struct {
	Label string
	Count int
	Pct   int
}

// SecuritySummary is what the page shows for one period.
type SecuritySummary struct {
	Total   int
	ByKind  map[string]int
	Sources []securityCount
	Paths   []securityCount
	Apps    []securityCount
	Buckets []securityBucket
	Recent  []SecurityEvent // newest first
}

// Attacked is whether the period holds anything beyond background noise.
func (s SecuritySummary) Attacked() bool {
	return s.ByKind[SecBan] > 0 || s.ByKind[SecScanner] > 0 || s.ByKind[SecProbe] > 0 ||
		s.ByKind[SecLoginFailed] >= securityAlertLogins || s.ByKind[SecOutboundRefused] > 0
}

func summarizeSecurity(events []SecurityEvent, since, now time.Time, bucket time.Duration, label string) SecuritySummary {
	s := SecuritySummary{Total: len(events), ByKind: map[string]int{}}
	sources, paths, apps := map[string]*securityCount{}, map[string]*securityCount{}, map[string]*securityCount{}
	add := func(m map[string]*securityCount, name, kind string, at time.Time) {
		if name == "" {
			return
		}
		c := m[name]
		if c == nil {
			c = &securityCount{Name: name}
			m[name] = c
		}
		c.Count++
		if !strings.Contains(" "+strings.Join(c.Kinds, " ")+" ", " "+kind+" ") {
			c.Kinds = append(c.Kinds, kind)
		}
		if at.After(c.LastAt) {
			c.LastAt = at
		}
	}
	n := int(now.Sub(since)/bucket) + 1 // the last bucket is the one still filling
	counts := make([]int, n)
	for _, ev := range events {
		s.ByKind[ev.Kind]++
		add(sources, ev.Source, ev.Kind, ev.At)
		add(apps, ev.App, ev.Kind, ev.At)
		if ev.Kind == SecProbe {
			add(paths, ev.Detail, ev.Kind, ev.At)
		}
		if i := int(ev.At.Sub(since) / bucket); i >= 0 && i < n {
			counts[i]++
		}
	}
	top := func(m map[string]*securityCount) []securityCount {
		out := make([]securityCount, 0, len(m))
		for _, c := range m {
			out = append(out, *c)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Count != out[j].Count {
				return out[i].Count > out[j].Count
			}
			return out[i].Name < out[j].Name
		})
		return out[:min(len(out), 10)]
	}
	s.Sources, s.Paths, s.Apps = top(sources), top(paths), top(apps)
	peak := 0
	for _, c := range counts {
		peak = max(peak, c)
	}
	for i, c := range counts {
		b := securityBucket{Label: since.Add(time.Duration(i) * bucket).Format(label), Count: c}
		if peak > 0 {
			b.Pct = c * 100 / peak
		}
		s.Buckets = append(s.Buckets, b)
	}
	for i := len(events) - 1; i >= 0 && len(s.Recent) < 100; i-- {
		s.Recent = append(s.Recent, events[i])
	}
	return s
}

// maybeSendSecurityAlert mails administrators when the last window crosses a
// threshold. Caller holds securityLog.mu.
func maybeSendSecurityAlert(now time.Time) {
	if now.Sub(securityLog.lastAlert) < securityAlertCooldown {
		return
	}
	since := now.Add(-securityAlertWindow)
	var window []SecurityEvent
	for i := len(securityLog.recent) - 1; i >= 0 && !securityLog.recent[i].At.Before(since); i-- {
		window = append(window, securityLog.recent[i])
	}
	s := summarizeSecurity(window, since, now, securityAlertWindow, "15:04")
	if !securityAlertDue(s) {
		return
	}
	securityLog.lastAlert = now
	fields := securityAlertFields(s)
	go func() {
		ctx := graceful.GetManager().ShutdownContext()
		if notifyMail(ctx, MailEventAttack, fields) == 0 {
			log.Warn("company: possible attack (%s events in %s) but no mail rule sent an alert", fields["total"], securityAlertWindow)
		}
	}()
}

func securityAlertDue(s SecuritySummary) bool {
	return s.ByKind[SecBan] > 0 || s.Total >= securityAlertTotal || s.ByKind[SecLoginFailed] >= securityAlertLogins
}

func securityAlertFields(s SecuritySummary) MailFields {
	var sources []string
	for _, c := range s.Sources[:min(len(s.Sources), 5)] {
		sources = append(sources, fmt.Sprintf("%s (%d)", c.Name, c.Count))
	}
	return MailFields{
		"window":      strconv.Itoa(int(securityAlertWindow.Minutes())),
		"total":       strconv.Itoa(s.Total),
		"logins":      strconv.Itoa(s.ByKind[SecLoginFailed]),
		"bans":        strconv.Itoa(s.ByKind[SecBan]),
		"probes":      strconv.Itoa(s.ByKind[SecProbe] + s.ByKind[SecScanner]),
		"outbound":    strconv.Itoa(s.ByKind[SecOutboundRefused]),
		"top_sources": strings.Join(sources, ", "),
		"link":        strings.TrimSuffix(setting.AppURL, "/") + "/-/admin/company-security",
	}
}

// watchSignIn records a failed sign-in. Gitea answers a failed one by
// showing the form again and a successful one with a redirect, so the
// response is read just before it goes out rather than patching Gitea's
// sign-in handler.
func watchSignIn(ctx *gitea_context.Context) {
	ctx.Resp.Before(func(w gitea_context.ResponseWriter) {
		if w.Header().Get("Location") != "" {
			return
		}
		RecordSecurityEvent(SecurityEvent{Kind: SecLoginFailed, Source: clientIP(ctx.Req), Detail: ctx.Req.PostFormValue("user_name")})
	})
}
