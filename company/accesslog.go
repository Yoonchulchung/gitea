// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/log"
	gitea_context "gitea.dev/services/context"
)

// Who reached an app, request by request.
//
// The metrics buckets answer "how busy was it"; they cannot answer "who was
// that at 02:14 and what did they ask for", which is the question after an
// incident. Nor could the proxy guard: it refused and moved on, and a refusal
// nobody can see later is indistinguishable from one that never happened.
//
// So every request the proxy decides on is written twice. Once to a bounded
// ring in memory, which is what the admin screen reads — the last few
// hundred per app, newest first, gone on restart. And once to access.log
// beside the app's other logs, rotated like them, which is what survives a
// restart and what an operator greps at 02:14. The broker already keeps the
// same kind of record for the outbound side (broker.go); this is its
// inbound half.

const (
	accessRingSize = 500
	accessLogName  = "access.log"
)

// AccessRecord is one decision the proxy made.
type AccessRecord struct {
	At      time.Time
	IP      string // the client address — behind a trusted proxy, the real one
	User    string // account name when signed in, else ""
	Method  string
	Path    string
	Status  int
	Blocked string // why the guard refused it; "" when it reached the app
}

// Client is the one identity to show: the account when there is one, the
// address otherwise — the same rule the request log and the guard key by.
func (r AccessRecord) Client() string {
	if r.User != "" {
		return r.User
	}
	return r.IP
}

type accessRing struct {
	mu   sync.Mutex
	recs []AccessRecord // newest last; read in reverse
	next int
	full bool
}

var accessRings sync.Map // appKey -> *accessRing

func accessRingFor(owner, repo string) *accessRing {
	key := appKey(owner, repo)
	if v, ok := accessRings.Load(key); ok {
		if r, ok := v.(*accessRing); ok {
			return r
		}
	}
	fresh := &accessRing{recs: make([]AccessRecord, accessRingSize)}
	v, _ := accessRings.LoadOrStore(key, fresh)
	if r, ok := v.(*accessRing); ok {
		return r
	}
	return fresh
}

// RecordAccess writes one decision. Called from the proxy for every request
// it answers, refused or served — the refused ones are the point.
func RecordAccess(ctx *gitea_context.Context, ref AppRef, status int, blocked string) {
	rec := AccessRecord{
		At:      time.Now(),
		IP:      clientIP(ctx.Req),
		Method:  ctx.Req.Method,
		Path:    ctx.Req.URL.RequestURI(),
		Status:  status,
		Blocked: blocked,
	}
	if ctx.IsSigned && ctx.Doer != nil {
		rec.User = ctx.Doer.Name
	}

	ring := accessRingFor(ref.Owner, ref.Repo)
	ring.mu.Lock()
	ring.recs[ring.next] = rec
	ring.next = (ring.next + 1) % accessRingSize
	if ring.next == 0 {
		ring.full = true
	}
	ring.mu.Unlock()

	appendAccessLog(ref, rec)
}

// appendAccessLog is the durable half. Best-effort and quiet on failure
// beyond one log line: a full disk must not turn every request into an
// error, and the ring still has the record.
func appendAccessLog(ref AppRef, rec AccessRecord) {
	w := accessLogWriterFor(ref)
	w.mu.Lock()
	defer w.mu.Unlock()
	// Opened once and kept, rotated here when it fills: every request used to
	// stat, open and close the file, and two at the threshold rotated it
	// twice.
	if w.f == nil || w.size >= appLogMaxBytes {
		if w.f != nil {
			_ = w.f.Close()
			w.f = nil
		}
		f, err := openRotatingLog(appPathsFor(ref.Owner, ref.Repo).logs, accessLogName)
		if err != nil {
			log.Warn("company: access log for %s/%s: %v", ref.Owner, ref.Repo, err)
			return
		}
		w.f, w.size = f, 0
		if fi, err := f.Stat(); err == nil {
			w.size = fi.Size()
		}
	}
	blocked := "-"
	if rec.Blocked != "" {
		blocked = strings.ReplaceAll(rec.Blocked, " ", "_")
	}
	user := rec.User
	if user == "" {
		user = "-"
	}
	// One line, space-separated, no quoting games: what an operator greps.
	n, _ := fmt.Fprintf(w.f, "%s %s %s %s %s %d %s\n",
		rec.At.UTC().Format(time.RFC3339), rec.IP, user, rec.Method, rec.Path, rec.Status, blocked)
	w.size += int64(n)
}

type accessLogWriter struct {
	mu   sync.Mutex
	f    *os.File
	size int64
}

var accessLogWriters sync.Map // appKey -> *accessLogWriter

func accessLogWriterFor(ref AppRef) *accessLogWriter {
	w, _ := accessLogWriters.LoadOrStore(appKey(ref.Owner, ref.Repo), &accessLogWriter{})
	return w.(*accessLogWriter) //nolint:forcetypeassert // only this function stores here
}

// RecentAccess returns the last records for an app, newest first, and how
// many of them the guard refused — the number the page leads with.
func RecentAccess(owner, repo string, limit int) (recs []AccessRecord, blocked int) {
	v, ok := accessRings.Load(appKey(owner, repo))
	if !ok {
		return nil, 0
	}
	ring, ok := v.(*accessRing)
	if !ok {
		return nil, 0
	}
	ring.mu.Lock()
	defer ring.mu.Unlock()
	n := ring.next
	if ring.full {
		n = accessRingSize
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	recs = make([]AccessRecord, 0, limit)
	for i := 1; i <= limit; i++ {
		idx := (ring.next - i + accessRingSize) % accessRingSize
		recs = append(recs, ring.recs[idx])
	}
	for i := 0; i < n; i++ {
		if ring.recs[i].Blocked != "" {
			blocked++
		}
	}
	return recs, blocked
}

// clientIP is the address to record. req.RemoteAddr is already the real
// client when REVERSE_PROXY_TRUSTED_PROXIES names the balancer in front, and
// the balancer itself otherwise — either way it is what the guard keyed on,
// so the record and the refusal agree.
func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// BannedVisitor is one address or account the guard is currently refusing.
type BannedVisitor struct {
	Key     string // "a:<ip>" or "u:<userid>", as the guard keys it
	Until   time.Time
	Strikes int
}

// ListBannedVisitors is what the admin screen shows under "blocked". Bans
// only — a visitor with strikes but no ban is not yet anybody's business.
func ListBannedVisitors() []BannedVisitor {
	now := time.Now()
	var out []BannedVisitor
	guardVisitors.Range(func(k, val any) bool {
		v, ok := val.(*guardVisitor)
		if !ok {
			return true
		}
		v.mu.Lock()
		if now.Before(v.bannedTo) {
			out = append(out, BannedVisitor{Key: fmt.Sprint(k), Until: v.bannedTo, Strikes: v.strikes})
		}
		v.mu.Unlock()
		return true
	})
	return out
}

// UnbanVisitor lifts a ban by hand. The strikes go with it: an administrator
// who decided this visitor is fine has decided the whole history is.
func UnbanVisitor(key, actor string) bool {
	val, ok := guardVisitors.Load(key)
	if !ok {
		return false
	}
	v, ok := val.(*guardVisitor)
	if !ok {
		return false
	}
	v.mu.Lock()
	wasBanned := time.Now().Before(v.bannedTo)
	v.bannedTo, v.strikes = time.Time{}, 0
	v.mu.Unlock()
	if wasBanned {
		log.Info("company: %s lifted the proxy guard ban on %s", actor, key)
	}
	return wasBanned
}
