// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"context"
	"fmt"

	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
	files_service "gitea.dev/services/repository/files"

	"go.yaml.in/yaml/v4"
)

// Approving a permission means committing it to `.company/apps.yml` in the
// central deploy repository, with the approving admin as the commit author.
//
// That commit *is* the audit record. "Who allowed this app to reach
// erp.internal, and when" is a question git answers on its own and cannot
// quietly rewrite, whereas a log file written by the Gitea account is only
// as trustworthy as that account. The same reasoning is why the policy has
// one source of truth — what an admin sees on screen and what is actually
// in force cannot drift apart if there is only the file.
//
// Admins never hand-edit the YAML: the approval button writes it.

const appsConfigPath = ".company/apps.yml"

// commitRetries covers the case of two admins approving different requests
// at the same moment. The loser re-reads and re-applies rather than
// clobbering, because both approvals are legitimate and neither should be
// silently lost.
const commitRetries = 3

// ApplyPermissionsOnMerge folds a merged deploy request's approved
// permissions into apps.yml.
//
// Best-effort by design: it runs from the merge notification, and the merge
// has already happened. Failing loudly here would leave an admin looking at
// a merged request with an error, unable to tell what did and did not apply.
// A failure is logged and the permissions simply stay unapplied — the app
// keeps its previous policy, which is the safe direction.
func ApplyPermissionsOnMerge(ctx context.Context, doer *user_model.User, owner, repo string, prID int64) {
	requests := LoadPermissionRequests(owner, repo, prID)
	if len(requests) == 0 {
		return
	}
	// Merging the deploy request is the approval — there is no separate
	// per-item decision, so everything the requester submitted and the admin
	// merged counts as approved.
	for i := range requests {
		requests[i].Decision = "approve"
	}

	var lastErr error
	for attempt := range commitRetries {
		err := commitPermissionUpdate(ctx, doer, owner, repo, requests, attempt)
		if err == nil {
			return
		}
		lastErr = err
	}
	log.Error("company: applying approved permissions for %s/%s after %d attempts: %v",
		owner, repo, commitRetries, lastErr)
}

// commitPermissionUpdate reads apps.yml, applies the approved items, and
// commits the result. Re-reads on every attempt so a concurrent approval by
// another admin is merged with rather than overwritten.
func commitPermissionUpdate(ctx context.Context, doer *user_model.User, owner, repo string, requests []PermissionRequest, attempt int) error {
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return err
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		return err
	}

	current, existed, err := readAppsConfigFile(ctx, central)
	if err != nil {
		return err
	}
	key := owner + "/" + repo
	if current.Apps == nil {
		current.Apps = map[string]*AppSettings{}
	}
	// Only this app's own block is touched. Approving pandas for one
	// department must not open it for every other one, which is why the
	// package list written below is `allowExtra` rather than `allow`.
	entry := current.Apps[key]
	if entry == nil {
		entry = &AppSettings{}
	}
	updated := ApplyApprovedPermissions(*entry, requests)
	current.Apps[key] = &updated

	body, err := yaml.Marshal(current)
	if err != nil {
		return err
	}

	operation := "update"
	if !existed {
		// The file is created on first approval rather than shipped empty, so
		// an admin never has to set anything up before the platform works.
		operation = "create"
	}
	message := fmt.Sprintf("chore(apps): approve permissions for %s\n\nApproved by %s.", key, doer.Name)
	if attempt > 0 {
		message += fmt.Sprintf("\n\n(retry %d after a concurrent update)", attempt)
	}

	_, err = files_service.ChangeRepoFiles(ctx, central, doer, &files_service.ChangeRepoFilesOptions{
		OldBranch: central.DefaultBranch,
		NewBranch: central.DefaultBranch,
		Message:   message,
		Files: []*files_service.ChangeRepoFile{{
			Operation:     operation,
			TreePath:      appsConfigPath,
			ContentReader: bytes.NewReader(body),
		}},
	})
	if err != nil {
		return err
	}

	// Put the new policy into force immediately. Without this the commit
	// lands but nothing changes until the next restart, and the admin who
	// just approved a package would watch the deploy fail for the same
	// reason all over again.
	if parsed, perr := ParseAppsConfig(body); perr == nil {
		SetAppsConfig(parsed)
	} else {
		SetAppsConfigError(perr)
	}
	return nil
}

// readAppsConfigFile loads the policy file from the central repo's default
// branch. A missing file is not an error — it is the state before the first
// approval.
func readAppsConfigFile(ctx context.Context, central *repo_model.Repository) (*AppsConfig, bool, error) {
	gitRepo, err := git.OpenRepository(ctx, central)
	if err != nil {
		return nil, false, err
	}
	defer gitRepo.Close()

	commit, err := gitRepo.GetBranchCommit(ctx, central.DefaultBranch)
	if err != nil {
		return &AppsConfig{Version: 1}, false, nil //nolint:nilerr // an empty repo has no policy yet
	}
	entry, err := commit.GetTreeEntryByPath(ctx, gitRepo, appsConfigPath)
	if err != nil {
		return &AppsConfig{Version: 1}, false, nil //nolint:nilerr // no file yet is the pre-first-approval state
	}
	blob := entry.Blob(gitRepo)
	body, err := blob.GetBlobBytes(ctx, blob.Size(ctx))
	if err != nil {
		return nil, false, err
	}
	cfg, err := ParseAppsConfig(body)
	if err != nil {
		// Refusing here is deliberate: rewriting a file we could not parse
		// would drop whatever an admin had hand-written in it.
		return nil, true, fmt.Errorf("the current apps.yml could not be parsed, so it was not modified: %w", err)
	}
	return cfg, true, nil
}

// LoadAppsConfigFromRepo refreshes the in-force policy from the central
// repository. Called at startup so a restart picks up whatever is committed.
func LoadAppsConfigFromRepo(ctx context.Context) {
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		// No central repo configured yet — the built-in defaults apply, which
		// are the safe ones (no network, no downloads, no packages).
		return
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		log.Warn("company: central deploy repo %s/%s not found; using built-in app defaults", centralOwner, centralName)
		return
	}
	cfg, _, err := readAppsConfigFile(ctx, central)
	if err != nil {
		SetAppsConfigError(err)
		return
	}
	SetAppsConfig(cfg)
}
