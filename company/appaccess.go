// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"sync"

	"gitea.dev/modules/log"
)

// "Who can open our app" is the department's decision to make narrower and an
// administrator's to make wider. Narrowing exposes nobody to anything and
// waiting for an admin to approve it would mean an app stays over-shared for
// as long as that takes; widening puts their data in front of more people and
// is a Deploy Request.
//
// Which is why this is not a second policy file. apps.yml is the admin's, and
// what a department sets here can only ever restrict below it — the merge in
// effectiveAccess takes the *narrower* of the two, so no value stored here can
// grant access that policy did not already allow. An admin tightening apps.yml
// therefore takes effect immediately regardless of what a department chose
// earlier, which is the behaviour an admin has to be able to rely on.
//
// Kept in memory as well as on disk because the proxy consults it on every
// request and reading a file there would put filesystem latency in front of
// every page an app serves (docs/company/app-platform-impl.md). The state file
// remains the record: this map is rebuilt from it at startup and is never the
// only copy.

var departmentAccess sync.Map // appKey -> access mode

// loadDepartmentAccess seeds the map from the state files at startup.
func loadDepartmentAccess() {
	for _, st := range ListAppStates() {
		if st.AccessChoice != "" {
			departmentAccess.Store(appKey(st.Owner, st.Repo), st.AccessChoice)
		}
	}
}

// effectiveAccess narrows the configured mode by the department's choice.
// Never widens: an unknown or wider stored value is ignored rather than
// trusted.
func effectiveAccess(owner, repo, configured string) string {
	if configured == "" {
		configured = AccessPublic
	}
	v, ok := departmentAccess.Load(appKey(owner, repo))
	if !ok {
		return configured
	}
	chosen, ok := v.(string)
	if !ok {
		return configured
	}
	if accessRank[chosen] < accessRank[configured] {
		return chosen
	}
	return configured
}

// SetDepartmentAccess records a department's choice.
//
// Refuses anything that is not narrower than what policy allows, so the
// handler cannot be talked into widening by a crafted form value — the check
// is here rather than only in the template, because the template is not what
// the request has to get past.
func SetDepartmentAccess(owner, repo, actor, mode string) error {
	if _, known := accessRank[mode]; !known {
		return userErrorf("알 수 없는 접근 범위입니다")
	}
	if accessRank[mode] > accessRank[configuredAccess(owner, repo)] {
		return userErrorf("접근 범위를 넓히려면 배포 요청으로 승인을 받아야 합니다")
	}

	if err := MutateAppState(owner, repo, func(st *AppState) bool {
		if st.AccessChoice == mode {
			return false
		}
		st.AccessChoice = mode
		st.AppendHistory(AppHistoryEntry{
			Status: st.Actual, Actor: actor,
			Reason: ReasonAccessChanged + ":" + mode,
		})
		return true
	}); err != nil {
		return err
	}
	departmentAccess.Store(appKey(owner, repo), mode)
	log.Info("company: %s/%s access set to %s by %s", owner, repo, mode, actor)
	return nil
}
