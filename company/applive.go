// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
)

// The deploy's health check proves an app came up; nothing proved it stayed
// up. An app whose event loop is stuck keeps its process, its socket and its
// green badge while every request hangs, and the first to notice is whoever
// is using it. This is the rest of what a container runtime gives — a
// periodic health check and a restart policy — and like the deploy's check
// it needs nothing from the app (see probeHealth).

const (
	livenessInterval = 30 * time.Second
	livenessTimeout  = 5 * time.Second
	// One slow answer is a busy app, not a stuck one.
	livenessFailuresBeforeRestart = 3
	// Restarted this often inside the window, the app is stuck for a reason a
	// restart does not fix, and restarting it forever would only hide that.
	livenessRestartLimit  = 3
	livenessRestartWindow = 15 * time.Minute
	// A just-started app is still importing; the deploy's own check covers
	// that stretch.
	livenessStartGrace = time.Minute

	livenessRestartReason = "unresponsive_restart"
)

// healthProbeQuery marks the platform's own probes so they can be left out of
// the log a person reads: at two a minute they would otherwise be most of it.
// Frameworks ignore a query parameter a route does not declare.
const healthProbeQuery = "platform-health-check=1"

// healthProbeLine is uvicorn's access line for a probe that passed. A failing
// one stays in the log, where it is part of the story.
var healthProbeLine = regexp.MustCompile(`"GET [^" ]*[?&]` + healthProbeQuery + ` HTTP/[\d.]+" [1-4]\d\d\b`)

func healthClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

// probeHealth asks the app once whether it is up. An app that serves its own
// health path is judged by what it answers, so its code has the last word;
// one that does not (404) is judged by having answered at all, which is
// everything the platform can know without its help.
func probeHealth(ctx context.Context, client *http.Client, path string) (own bool, err error) {
	if path == "" {
		path = "/"
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://app"+path+sep+healthProbeQuery, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	own = resp.StatusCode != http.StatusNotFound
	if resp.StatusCode >= 500 {
		return own, fmt.Errorf("the app answered with HTTP %d", resp.StatusCode)
	}
	return own, nil
}

type livenessRecord struct {
	health   AppHealth // the latest probe; the state file has only the latest change
	failures int
	restarts []time.Time
}

var (
	livenessMu sync.Mutex
	liveness   = map[string]*livenessRecord{}
)

func livenessFor(key string) *livenessRecord {
	rec := liveness[key]
	if rec == nil {
		rec = &livenessRecord{}
		liveness[key] = rec
	}
	return rec
}

// allowRestart records an automatic restart, or reports that the app has had
// its share and should be left down.
func (rec *livenessRecord) allowRestart(now time.Time) bool {
	rec.restarts = slices.DeleteFunc(rec.restarts, func(t time.Time) bool { return now.Sub(t) > livenessRestartWindow })
	if len(rec.restarts) >= livenessRestartLimit {
		rec.restarts = nil
		return false
	}
	rec.restarts = append(rec.restarts, now)
	return true
}

// CurrentHealth is an app's latest probe result. The state file is written
// only when the result changes — every write moves UpdatedAt, which screens
// read as "last changed" — so a running app's fresh result lives here.
func CurrentHealth(st *AppState) AppHealth {
	if st.Actual != AppStateRunning {
		return st.Health
	}
	livenessMu.Lock()
	defer livenessMu.Unlock()
	if rec := liveness[appKey(st.Owner, st.Repo)]; rec != nil && rec.health.CheckedAt != 0 {
		return rec.health
	}
	return st.Health
}

// StartLivenessChecks runs the periodic health check. Called once at startup.
func StartLivenessChecks() {
	ctx := graceful.GetManager().ShutdownContext()
	go func() {
		ticker := time.NewTicker(livenessInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var wg sync.WaitGroup
				appRegistry.Range(func(_, v any) bool {
					if ref, ok := v.(AppRef); ok {
						wg.Go(func() { checkLiveness(ctx, ref.Owner, ref.Repo) })
					}
					return true
				})
				wg.Wait() // a slow round delays the next one rather than overlapping it
			}
		}
	}()
}

func checkLiveness(ctx context.Context, owner, repo string) {
	key := appKey(owner, repo)
	s := lookupSupervisor(owner, repo)
	st := LoadAppState(owner, repo)
	if s == nil || s.pid() == 0 || st.Actual != AppStateRunning || time.Since(time.Unix(st.StartedAt, 0)) < livenessStartGrace {
		livenessMu.Lock()
		livenessFor(key).failures = 0
		livenessMu.Unlock()
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, livenessTimeout)
	own, err := probeHealth(probeCtx, healthClient(s.paths.socket), SettingsFor(owner, repo).HealthPath)
	cancel()
	if ctx.Err() != nil {
		return // Gitea is shutting down, which says nothing about the app
	}

	livenessMu.Lock()
	rec := livenessFor(key)
	if err == nil {
		rec.failures = 0
		rec.health = AppHealth{State: "up", Own: own, CheckedAt: time.Now().Unix()}
	} else if rec.failures++; rec.failures >= livenessFailuresBeforeRestart {
		rec.health = AppHealth{State: "down", Detail: err.Error(), CheckedAt: time.Now().Unix()}
	}
	health, failures := rec.health, rec.failures
	livenessMu.Unlock()

	if health.CheckedAt != 0 && (health.State != st.Health.State || health.Own != st.Health.Own) {
		_ = MutateAppState(owner, repo, func(st *AppState) bool {
			if st.Actual != AppStateRunning {
				return false // stopped while this probe was out
			}
			st.Health = health
			return true
		})
	}
	if failures >= livenessFailuresBeforeRestart {
		restartUnresponsive(owner, repo, s, err)
	}
}

func restartUnresponsive(owner, repo string, s *appSupervisor, cause error) {
	// A deploy, rollback or removal is replacing this process anyway, and a
	// restart underneath it would race its swap.
	if !s.deployMu.TryLock() {
		return
	}
	defer s.deployMu.Unlock()
	if LoadAppState(owner, repo).Actual != AppStateRunning {
		return
	}

	now := time.Now()
	livenessMu.Lock()
	rec := livenessFor(appKey(owner, repo))
	rec.failures = 0
	rec.health = AppHealth{}
	allowed := rec.allowRestart(now)
	livenessMu.Unlock()

	if !allowed {
		log.Warn("company: %s/%s stopped answering its health check again after %d automatic restarts; leaving it stopped: %v",
			owner, repo, livenessRestartLimit, cause)
		if err := s.Stop("platform", AppStateFailed, ReasonUnresponsive); err != nil {
			log.Error("company: stopping unresponsive %s/%s: %v", owner, repo, err)
		}
		_ = MutateAppState(owner, repo, func(st *AppState) bool {
			st.FailedAt = now.Unix()
			st.Message = fmt.Sprintf("the app stopped answering its health check (%v), and %d automatic restarts within %s did not help",
				cause, livenessRestartLimit, livenessRestartWindow)
			st.UserMessage = ""
			st.Health = AppHealth{State: "down", Detail: cause.Error(), CheckedAt: now.Unix()}
			// As with the memory watchdog: the platform stopped it, not the
			// department, and they can start it again once it is fixed.
			st.Desired = AppStateRunning
			return true
		})
		return
	}

	log.Warn("company: %s/%s stopped answering its health check; restarting it: %v", owner, repo, cause)
	if err := s.Restart(); err != nil {
		log.Error("company: restarting unresponsive %s/%s: %v", owner, repo, err)
		return
	}
	RecordRestart(owner, repo)
	_ = MutateAppState(owner, repo, func(st *AppState) bool {
		st.AppendHistory(AppHistoryEntry{Status: AppStateRunning, Actor: "platform", Reason: livenessRestartReason})
		return true
	})
}
