// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
	gitea_context "gitea.dev/services/context"
)

// What stands between the internet and a department's app.
//
// The apps behind this proxy are the weakest code on the host by design:
// written by people who are not developers, never load-tested, and one
// crafted request away from a stack trace. Gitea itself is hardened; they are
// not. So the proxy absorbs what a bot would otherwise land on them — the
// scanner probing for /.env, the loop hammering one endpoint, the client
// posting a gigabyte — before any of it wakes the app.
//
// Five layers, each cheap and each answering one question, in the order a
// request is cheapest to refuse. Every knob is an app.ini [company] setting
// (docs/company/app-platform.md), because the right numbers depend on how
// many people use an app and that is not something to compile in.
//
// The identity a limit is keyed by is the signed-in account, else the client
// address — the same one the request log already uses (visitorKey). Behind a
// load balancer that address is only right when REVERSE_PROXY_TRUSTED_PROXIES
// names the balancer; without it every visitor shares one address and the
// limits over-fire. That failure is the safe direction and it is an
// operator's configuration to fix, not something to guess around here.

const (
	guardRatePerMinDefault = 300 // sustained requests per visitor
	guardBurstDefault      = 60  // what a page load with its assets needs at once
	guardBanStrikesDefault = 5   // probes or limit trips before a visitor is cut off
	guardBanMinutesDefault = 15
	guardMaxBodyMBDefault  = 32

	// Memory is the one resource a flood of fresh addresses can exhaust
	// here. Past this many tracked visitors the idle ones go first, and a
	// visitor evicted mid-window simply starts a fresh bucket — over-lenient
	// for one address for one minute, which is nothing next to the
	// alternative.
	guardMaxVisitors = 50000
	guardSweepEvery  = time.Minute
	guardIdleEvict   = 10 * time.Minute
)

// guardProbePaths are what a scanner asks for before it knows what it is
// talking to. None of them is a page any department app serves, so a request
// for one is answered here with a 404 the app never sees — and remembered,
// because one is a mistake and five is a scan.
var guardProbePaths = []string{
	"/.env", "/.git/", "/.git", "/.svn/", "/.hg/", "/.DS_Store",
	"/wp-admin", "/wp-login.php", "/wp-content/", "/xmlrpc.php",
	"/phpmyadmin", "/pma/", "/adminer", "/mysql/",
	"/cgi-bin/", "/.aws/", "/.ssh/", "/config.php", "/web.config",
	"/etc/passwd", "/actuator/", "/console/", "/solr/", "/jenkins/",
	"/vendor/phpunit", "/telescope", "/_ignition",
}

// guardBotAgents are user agents that only ever belong to a scanner. The
// list is deliberately short and unambiguous: a broad one ("python", "curl")
// would block the department's own scripts, and a blocked scanner just
// changes its string anyway. This catches the ones that do not bother.
var guardBotAgents = []string{
	"sqlmap", "nikto", "nmap", "masscan", "zgrab", "nuclei", "wpscan",
	"dirbuster", "gobuster", "ffuf", "acunetix", "nessus", "openvas",
	"netsparker", "burpcollaborator", "hydra",
}

// guardVisitor is one tracked identity: a token bucket for rate, a strike
// count for behaviour, and a ban that outlasts both.
type guardVisitor struct {
	mu       sync.Mutex
	tokens   float64
	refilled time.Time
	strikes  int
	strikeAt time.Time
	bannedTo time.Time
	seen     time.Time
}

var (
	guardVisitors sync.Map // visitor key -> *guardVisitor
	guardCount    int64    // approximate; guarded by guardCountMu
	guardCountMu  sync.Mutex
)

// proxyGuardEnabled is the master switch. On unless an operator says
// otherwise: the apps behind this proxy are the reason it exists.
func proxyGuardEnabled() bool { return companySetting("APP_PROXY_GUARD") != "false" }

type guardVerdict struct {
	status int
	body   string
	retry  time.Duration // for Retry-After, when the refusal is temporary
}

// guardRequest decides whether a request reaches the app.
//
// Returns nil to let it through. Administrators are never refused: they are
// not the threat this exists for, and an admin probing their own app to see
// what it does must not lock themselves out of it.
func guardRequest(ctx *gitea_context.Context, ref AppRef) *guardVerdict {
	if !proxyGuardEnabled() {
		return nil
	}
	if ctx.IsSigned && ctx.Doer != nil && ctx.Doer.IsAdmin {
		return nil
	}

	key := visitorKey(ctx)
	v := guardVisitorFor(key)
	now := time.Now()

	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = now

	if now.Before(v.bannedTo) {
		return &guardVerdict{
			status: http.StatusTooManyRequests,
			body:   "Too many requests. Try again later.", retry: v.bannedTo.Sub(now),
		}
	}

	// Cheapest refusals first. A scanner's user agent and a probe path are
	// both known before a single byte of body is read.
	if ua := strings.ToLower(ctx.Req.UserAgent()); ua != "" {
		for _, bot := range guardBotAgents {
			if strings.Contains(ua, bot) {
				guardStrike(v, now, key, ref, "scanner user agent")
				return &guardVerdict{status: http.StatusForbidden, body: "Forbidden."}
			}
		}
	}
	if isProbePath(appRelativePath(ctx.Req.URL.Path)) {
		guardStrike(v, now, key, ref, "probe for "+ctx.Req.URL.Path)
		// The same 404 the app would give, so a scanner learns nothing from
		// the difference between "blocked" and "absent".
		return &guardVerdict{status: http.StatusNotFound, body: "This app does not exist."}
	}

	// Token bucket, refilled on read rather than by a timer per visitor:
	// fifty thousand timers is its own denial of service.
	rate := float64(companySettingPositiveInt("APP_PROXY_RATE_PER_MIN", guardRatePerMinDefault)) / 60
	burst := float64(companySettingPositiveInt("APP_PROXY_BURST", guardBurstDefault))
	if v.refilled.IsZero() {
		v.tokens, v.refilled = burst, now
	} else {
		v.tokens = min(burst, v.tokens+now.Sub(v.refilled).Seconds()*rate)
		v.refilled = now
	}
	if v.tokens < 1 {
		guardStrike(v, now, key, ref, "rate limit")
		return &guardVerdict{
			status: http.StatusTooManyRequests,
			body:   "Too many requests. Slow down.", retry: time.Second,
		}
	}
	v.tokens--
	return nil
}

// guardStrike records misbehaviour and bans on the threshold. Caller holds
// v.mu. Logged once at the moment of banning, never per blocked request —
// a log line per request is how a flood turns a full disk into its second
// victim.
func guardStrike(v *guardVisitor, now time.Time, key string, ref AppRef, why string) {
	window := time.Duration(companySettingPositiveInt("APP_PROXY_BAN_MINUTES", guardBanMinutesDefault)) * time.Minute
	if now.Sub(v.strikeAt) > window {
		v.strikes = 0 // strikes decay with the same clock as the ban
	}
	v.strikes++
	v.strikeAt = now
	if v.strikes < companySettingPositiveInt("APP_PROXY_BAN_STRIKES", guardBanStrikesDefault) {
		return
	}
	v.bannedTo = now.Add(window)
	v.strikes = 0
	log.Warn("company: proxy guard blocked %s from %s/%s for %s (last: %s)", key, ref.Owner, ref.Repo, window, why)
}

// isProbePath matches against the app-relative path, so the mount prefix a
// department's app lives under never shadows a probe hidden after it.
func isProbePath(rel string) bool {
	lower := strings.ToLower(rel)
	for _, p := range guardProbePaths {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

func guardVisitorFor(key string) *guardVisitor {
	if val, ok := guardVisitors.Load(key); ok {
		if v, ok := val.(*guardVisitor); ok {
			return v
		}
	}
	fresh := &guardVisitor{}
	val, loaded := guardVisitors.LoadOrStore(key, fresh)
	if !loaded {
		guardCountMu.Lock()
		guardCount++
		over := guardCount > guardMaxVisitors
		guardCountMu.Unlock()
		if over {
			sweepGuardVisitors(time.Now(), 0) // shed idle entries now, not at the next tick
		}
	}
	// The map only ever holds *guardVisitor; anything else is a programming
	// error, and a fresh bucket is the safe answer to one.
	if v, ok := val.(*guardVisitor); ok {
		return v
	}
	return fresh
}

// sweepGuardVisitors drops entries idle for longer than idle. A zero idle
// drops everything not seen in the last minute, which is the emergency
// setting for when the map is over its cap.
func sweepGuardVisitors(now time.Time, idle time.Duration) {
	if idle == 0 {
		idle = time.Minute
	}
	guardVisitors.Range(func(k, val any) bool {
		v, ok := val.(*guardVisitor)
		if !ok {
			guardVisitors.Delete(k) // not ours; nothing to keep
			return true
		}
		v.mu.Lock()
		stale := now.Sub(v.seen) > idle && now.After(v.bannedTo) // a ban is worth keeping
		v.mu.Unlock()
		if stale {
			guardVisitors.Delete(k)
			guardCountMu.Lock()
			guardCount--
			guardCountMu.Unlock()
		}
		return true
	})
}

// StartProxyGuard runs the periodic sweep for as long as this process lives.
func StartProxyGuard() {
	ctx := graceful.GetManager().ShutdownContext()
	go func() {
		ticker := time.NewTicker(guardSweepEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweepGuardVisitors(time.Now(), guardIdleEvict)
			}
		}
	}()
}

// guardBodyLimit caps what a request may carry to the app. The size is
// applied both ways: a declared Content-Length over the cap is refused before
// anything is read, and a body that lies about its size is cut off by the
// reader as it streams.
func guardBodyLimit(ctx *gitea_context.Context) bool {
	if !proxyGuardEnabled() {
		return true
	}
	limit := int64(companySettingPositiveInt("APP_PROXY_MAX_BODY_MB", guardMaxBodyMBDefault)) << 20
	if ctx.Req.ContentLength > limit {
		ctx.PlainText(http.StatusRequestEntityTooLarge, "Request body too large.")
		return false
	}
	if ctx.Req.Body != nil && ctx.Req.Body != http.NoBody {
		ctx.Req.Body = http.MaxBytesReader(ctx.Resp, ctx.Req.Body, limit)
	}
	return true
}
