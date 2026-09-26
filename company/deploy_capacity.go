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
//
// A sentence with four numbers in it is arithmetic the approver still has to
// do. Title, Figure and Fill are the same answer drawn: how full the server
// would be if this were approved, at the width of a bar. Text stays, under
// it, for the reader who wants the numbers.
type capacityFinding struct {
	State string // "ok", "warn" or "no"
	// Kind is the permission this answers for, so the gauge can be drawn
	// under the item it is about rather than in a panel of its own — the
	// approver is deciding on "512MB → 1024MB", and what that does to the
	// server belongs on that line.
	Kind   string
	Figure string
	// Fill is the bar's width in percent, 0-100. Over-commitment is clamped
	// to a full bar — past the end there is nothing left to draw, and the
	// colour and the figure both already say it.
	Fill int
	// BaseFill is how much of that was already committed before this request,
	// and AddFill is what approving it would add. Drawn as two shades of the
	// same bar, because the question in front of the approver is not "how full
	// is the server" — they can read that anywhere — but "how much fuller does
	// this make it".
	BaseFill int
	AddFill  int
	// BaseText and AddText name those two parts for the legend, or "" when
	// this request adds nothing and there is only one number to say.
	BaseText string
	AddText  string
	Text     string
	// RecordedAt is when these figures were taken, 0 when they describe the
	// server as it is now.
	RecordedAt int64
}

// barFill turns a part and a whole into a bar width, clamped.
func barFill(part, whole int) int {
	if whole <= 0 {
		return 100 // nothing to give: a full bar is the honest picture
	}
	return min(part*100/whole, 100)
}

// splitFill divides the bar into what was already committed and what this
// request adds.
//
// The added part is what is left of the total after the base, not its own
// share of the whole: both are clamped to the bar, and two clamped numbers
// that do not add up would draw a bar longer than full.
func splitFill(before, after, whole int) (total, base, add int) {
	total = barFill(after, whole)
	base = min(barFill(before, whole), total)
	return total, base, total - base
}

// CapacitySnapshot is the arithmetic behind one gauge, with no words in it.
//
// Stored on a request when it is decided (company/permissions.go), because
// the gauge answers "can the server carry this" and the server it could or
// could not carry is the one that was there at the time. Re-running the sums
// against today's fleet under a request from last month is not the same
// question, and would quietly answer a different one.
//
// Numbers rather than the rendered sentences: a record written in whichever
// language the page happened to be in is a record the next person cannot
// read — the same reason a request's own Detail stores mode codes.
type CapacitySnapshot struct {
	Kind string `json:"kind"`
	// Own is what this app itself would hold or be given.
	Own int `json:"own"`
	// Before and After are the whole server's commitment either side of this
	// request, and Whole is what there is to commit.
	Before int `json:"before"`
	After  int `json:"after"`
	Whole  int `json:"whole"`
}

// capacityNumbers works out the memory and storage a request implies.
func capacityNumbers(owner, repo string, requests []PermissionRequest) []CapacitySnapshot {
	var out []CapacitySnapshot
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
		others := memoryCommitmentMB(owner, repo)
		out = append(out, CapacitySnapshot{
			Kind:   PermKindMemory,
			Own:    memoryMB,
			Before: others + settings.Limits.MemoryMB,
			After:  others + memoryMB,
			Whole:  int(host.TotalBytes >> 20),
		})
	}

	if dataMB > 0 {
		if free, ok := freeBytesOn(appDataRoot()); ok {
			floorMB := companySettingPositiveInt("APP_DATA_HOST_FLOOR_MB", appDataHostFloorMBDefault)
			out = append(out, CapacitySnapshot{
				Kind:   PermKindData,
				Own:    dataMB,
				Before: min(settings.Limits.DataMB, dataMB),
				After:  dataMB,
				Whole:  int(free>>20) - floorMB,
			})
		}
	}
	return out
}

// renderCapacity says those numbers in words and bars.
//
// recordedAt is when they were taken, 0 for figures read just now. A gauge
// under a decided request has to say which it is — the same bar means two
// different things depending on whether it describes the server now or the
// server somebody approved against.
func renderCapacity(locale translation.Locale, snaps []CapacitySnapshot, recordedAt int64) []capacityFinding {
	out := make([]capacityFinding, 0, len(snaps))
	for _, s := range snaps {
		if s.Whole <= 0 && s.Kind == PermKindMemory {
			continue // no server figure to compare against
		}
		f := capacityFinding{Kind: s.Kind, State: capacityState(s), RecordedAt: recordedAt}
		switch s.Kind {
		case PermKindMemory:
			pct := s.After * 100 / s.Whole
			f.Figure = locale.TrString("company.review.capacity_of_total", s.After, s.Whole, pct)
			f.Text = locale.TrString("company.review.capacity_memory", s.Own, s.After, s.Whole, pct)
		case PermKindData:
			f.Figure = locale.TrString("company.review.capacity_of_room", s.Own, max(s.Whole, 0))
			f.Text = locale.TrString("company.review.capacity_data", s.Own, max(s.Whole, 0))
		}
		f.Fill, f.BaseFill, f.AddFill = splitFill(s.Before, s.After, s.Whole)
		if added := s.After - s.Before; added > 0 {
			f.BaseText = locale.TrString("company.review.capacity_base", s.Before)
			f.AddText = locale.TrString("company.review.capacity_added", added)
		}
		out = append(out, f)
	}
	return out
}

// capacityState is the verdict: green while there is room, amber as it runs
// out, red once the request would not fit.
func capacityState(s CapacitySnapshot) string {
	switch s.Kind {
	case PermKindMemory:
		switch pct := s.After * 100 / s.Whole; {
		case pct >= 100:
			return "no"
		case pct >= memoryCommitWarnPct:
			return "warn"
		}
	case PermKindData:
		switch {
		case s.Own > s.Whole:
			return "no"
		case s.Own*2 > s.Whole:
			return "warn"
		}
	}
	return "ok"
}

// capacityCheck is both halves against the server as it is right now.
func capacityCheck(locale translation.Locale, owner, repo string, requests []PermissionRequest) []capacityFinding {
	return renderCapacity(locale, capacityNumbers(owner, repo, requests), 0)
}
