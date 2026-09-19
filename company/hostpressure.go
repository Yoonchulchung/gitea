// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strconv"
	"sync"
	"time"

	"gitea.dev/modules/log"
)

// When the whole machine runs short of memory, the kernel picks a victim
// — and every app is marked a better one than Gitea (deprioritizeAndProtect),
// so it is an app, chosen by the kernel, killed with SIGKILL and nothing
// written down. This acts first: while there is still a floor of memory
// left, the app holding the most is stopped the ordinary way, SIGTERM
// first, with its reason recorded and its start button live. One app per
// sample round, and a pause between, so a spike that passes on its own
// costs one app and not several.

const (
	hostMemoryFloorMBDefault = 512
	hostPressureCooldown     = time.Minute
)

var (
	hostPressureMu   sync.Mutex
	hostPressureLast time.Time
)

// checkHostPressure runs once per sample round.
func checkHostPressure() {
	host, ok := hostMemory()
	if !ok || !host.HasAvailable {
		return
	}
	floor := int64(companySettingPositiveInt("HOST_MEMORY_FLOOR_MB", hostMemoryFloorMBDefault)) << 20
	if host.AvailableBytes >= floor {
		return
	}
	hostPressureMu.Lock()
	if time.Since(hostPressureLast) < hostPressureCooldown {
		hostPressureMu.Unlock()
		return
	}
	hostPressureLast = time.Now()
	hostPressureMu.Unlock()

	owner, repo, mb := heaviestRunningApp()
	if owner == "" {
		log.Error("company: the server has %d MB of memory left and no app is running to stop", host.AvailableBytes>>20)
		return
	}
	log.Error("company: the server has %d MB of memory left; stopping %s/%s (%d MB) before the kernel picks something itself",
		host.AvailableBytes>>20, owner, repo, mb)
	if err := supervisorFor(owner, repo).Stop("platform", AppStateFailed, ReasonHostMemory); err != nil {
		log.Error("company: stopping %s/%s under memory pressure: %v", owner, repo, err)
	}
	_ = MutateAppState(owner, repo, func(st *AppState) bool {
		st.FailedAt = time.Now().Unix()
		st.Reason = ReasonHostMemory
		st.Message = "the app was stopped because the server itself was running out of memory (" +
			strconv.FormatInt(host.AvailableBytes>>20, 10) + " MB left); it was the app holding the most (" + strconv.Itoa(mb) + " MB)"
		st.UserMessage = ""
		st.UserMessageKey = "company.sample.host_memory_stopped"
		st.UserMessageArg = mb
		st.Desired = AppStateRunning
		return true
	})
}

// heaviestRunningApp is the running app with the largest last memory
// reading, or "" when none is running.
func heaviestRunningApp() (owner, repo string, mb int) {
	for _, st := range ListAppStates() {
		if st.Actual != AppStateRunning || isPreviewRepo(st.Repo) {
			continue
		}
		if used := CurrentMemoryMB(st.Owner, st.Repo); used > mb || owner == "" {
			owner, repo, mb = st.Owner, st.Repo, used
		}
	}
	return owner, repo, mb
}
