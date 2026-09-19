// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strconv"

	"gitea.dev/modules/translation"
)

// Whether the server can carry what a request asks for is a question the
// approver had to answer by arithmetic on two other screens. It is answered
// on the request itself: the memory this app would hold against what the
// running apps already commit, and the storage it asks for against what the
// volume has left. Said, not enforced — the approver decides, with the
// numbers in front of them.

// capacityFinding is one line of that answer.
type capacityFinding struct {
	State string // "ok", "warn" or "no"
	Text  string
}

// capacityCheck works out the memory and storage a request implies.
func capacityCheck(locale translation.Locale, owner, repo string, requests []PermissionRequest) []capacityFinding {
	var out []capacityFinding
	settings := SettingsFor(owner, repo)

	memoryMB := settings.Limits.MemoryMB
	dataMB := 0
	for _, r := range requests {
		switch r.Kind {
		case PermKindMemory:
			if v, err := strconv.Atoi(r.Value); err == nil && v > 0 {
				memoryMB = v
			}
		case PermKindData:
			if v, err := strconv.Atoi(r.Value); err == nil && v > 0 {
				dataMB = v
			}
		}
	}

	if host, ok := hostMemory(); ok && host.TotalBytes > 0 && memoryMB > 0 {
		totalMB := int(host.TotalBytes >> 20)
		after := memoryCommitmentMB(owner, repo) + memoryMB
		pct := after * 100 / totalMB
		state := "ok"
		if pct >= 100 {
			state = "no"
		} else if pct >= memoryCommitWarnPct {
			state = "warn"
		}
		out = append(out, capacityFinding{State: state, Text: locale.TrString("company.review.capacity_memory", memoryMB, after, totalMB, pct)})
	}

	if dataMB > 0 {
		if free, ok := freeBytesOn(appDataRoot()); ok {
			floorMB := companySettingPositiveInt("APP_DATA_HOST_FLOOR_MB", appDataHostFloorMBDefault)
			roomMB := int(free>>20) - floorMB
			state := "ok"
			if dataMB > roomMB {
				state = "no"
			} else if dataMB*2 > roomMB {
				state = "warn"
			}
			out = append(out, capacityFinding{State: state, Text: locale.TrString("company.review.capacity_data", dataMB, max(roomMB, 0))})
		}
	}
	return out
}
