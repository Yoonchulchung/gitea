// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"slices"
	"time"

	"gitea.dev/modules/graceful"
	"gitea.dev/modules/log"
	"gitea.dev/modules/timeutil"
)

const (
	autoSnapshotDefaultDays = 7
	autoSnapshotCheckEvery  = time.Hour
	autoSnapshotFirstCheck  = 5 * time.Minute // after startup's own work
)

var autoSnapshotDayChoices = []int{1, 3, 7, 14, 30}

// AutoSnapshotView is the schedule as the data page shows it.
type AutoSnapshotView struct {
	On      bool
	Days    int
	NextAt  timeutil.TimeStamp // 0 while the first one has not been taken yet
	Choices []int
}

func (st *AppState) autoSnapshotDays() int {
	if slices.Contains(autoSnapshotDayChoices, st.AutoSnapshotDays) {
		return st.AutoSnapshotDays
	}
	return autoSnapshotDefaultDays
}

func autoSnapshotViewFor(st *AppState) AutoSnapshotView {
	v := AutoSnapshotView{On: st.AutoSnapshot, Days: st.autoSnapshotDays(), Choices: autoSnapshotDayChoices}
	if st.AutoSnapshotAt > 0 {
		v.NextAt = timeutil.TimeStamp(st.AutoSnapshotAt + int64(v.Days)*24*60*60)
	}
	return v
}

// autoSnapshotDue is whether st's schedule calls for a snapshot at now.
func autoSnapshotDue(st *AppState, now time.Time) bool {
	if !st.AutoSnapshot || !st.HasRelease {
		return false // the helper runs on the release's interpreter
	}
	return now.Unix()-st.AutoSnapshotAt >= int64(st.autoSnapshotDays())*24*60*60
}

func setAutoSnapshot(owner, repo string, on bool, days int) error {
	if !slices.Contains(autoSnapshotDayChoices, days) {
		days = autoSnapshotDefaultDays
	}
	return MutateAppState(owner, repo, func(st *AppState) bool {
		st.AutoSnapshot, st.AutoSnapshotDays = on, days
		return true
	})
}

// StartAutoSnapshots takes each app's scheduled snapshots. A failed one is
// tried again at the next check rather than a whole period later.
func StartAutoSnapshots() {
	if !AppDataEnabled() {
		return
	}
	ctx := graceful.GetManager().ShutdownContext()
	go func() {
		defer recoverBackground("automatic snapshots")
		timer := time.NewTimer(autoSnapshotFirstCheck)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			now := time.Now()
			for _, st := range ListAppStates() {
				if !autoSnapshotDue(st, now) {
					continue
				}
				name, err := CreateSnapshot(ctx, st.Owner, st.Repo, "auto")
				if err != nil {
					log.Warn("company: %s/%s: automatic snapshot: %v", st.Owner, st.Repo, err)
					continue
				}
				log.Info("company: %s/%s: automatic snapshot %s", st.Owner, st.Repo, name)
				_ = MutateAppState(st.Owner, st.Repo, func(s *AppState) bool {
					s.AutoSnapshotAt = now.Unix()
					return true
				})
			}
			timer.Reset(autoSnapshotCheckEvery)
		}
	}()
}
