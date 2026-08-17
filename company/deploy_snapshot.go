// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// deployBranchCleanupNotifier (company/deploy_notifier.go) deletes a Deploy
// Request's branch the moment its PR closes, so nothing keeps piling up on
// central-deploy — but that also means the branch ref DeployRequestFiles
// used to resolve "what did this PR actually contain" (company/deployrequestfiles.go)
// is gone by the time anyone looks at a closed request's file list later.
// The underlying commit objects usually survive for a good while after
// their ref is deleted (nothing prunes them immediately), so recording the
// branch's own head commit SHA right before deletion is enough to keep the
// diff resolvable purely by SHA — no ref needed. One tiny JSON file per PR,
// permanent (a closed PR's own historical record doesn't expire the way an
// in-progress draft does — see workspace_tmp.go for that different case).

type deploySnapshotRecord struct {
	HeadCommitID string `json:"headCommitId"`
}

func deploySnapshotFile(prID int64) string {
	return filepath.Join(setting.AppDataPath, "company-deploy-snapshots", fmt.Sprintf("%d.json", prID))
}

// saveDeploySnapshot records pr's branch's current head commit — call this
// while the branch still exists, right before deleting it. Best-effort: a
// failure here only costs DeployRequestFiles its post-deletion fallback for
// this one PR, nothing else.
func saveDeploySnapshot(prID int64, headCommitID string) {
	file := deploySnapshotFile(prID)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		log.Error("company: saveDeploySnapshot: mkdir: %v", err)
		return
	}
	body, err := json.Marshal(deploySnapshotRecord{HeadCommitID: headCommitID})
	if err != nil {
		log.Error("company: saveDeploySnapshot: marshal: %v", err)
		return
	}
	if err := os.WriteFile(file, body, 0o600); err != nil {
		log.Error("company: saveDeploySnapshot: write: %v", err)
	}
}

// loadDeploySnapshot returns the head commit SHA saveDeploySnapshot
// recorded for prID, if any — ok is false if this PR's branch was never
// snapshotted (e.g. it's still open, or predates this feature).
func loadDeploySnapshot(prID int64) (headCommitID string, ok bool) {
	body, err := os.ReadFile(deploySnapshotFile(prID))
	if err != nil {
		return "", false
	}
	var rec deploySnapshotRecord
	if json.Unmarshal(body, &rec) != nil || rec.HeadCommitID == "" {
		return "", false
	}
	return rec.HeadCommitID, true
}
