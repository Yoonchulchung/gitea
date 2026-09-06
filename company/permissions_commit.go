// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

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

// CommitBasePackages replaces the platform-provided package list.
//
// Written to the same file, by the same route, as an approved permission:
// policy lives in git so that "who changed what every app installs, and
// when" is a question git answers on its own. An admin never edits the YAML —
// the form does it for them.
func CommitBasePackages(ctx context.Context, doer *user_model.User, packages []string) error {
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return err
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := range commitRetries {
		current, existed, err := readAppsConfigFile(ctx, central)
		if err != nil {
			return err
		}
		current.Version = 1
		current.Defaults.BasePackages = packages

		body, marshalErr := yaml.Marshal(current)
		if marshalErr != nil {
			return marshalErr
		}
		operation := "update"
		if !existed {
			operation = "create"
		}
		message := fmt.Sprintf("chore(apps): set base packages\n\nChanged by %s: %s.",
			doer.Name, strings.Join(packages, ", "))
		if attempt > 0 {
			message += fmt.Sprintf("\n\n(retry %d after a concurrent update)", attempt)
		}

		_, lastErr = files_service.ChangeRepoFiles(ctx, central, doer, &files_service.ChangeRepoFilesOptions{
			OldBranch: central.DefaultBranch,
			NewBranch: central.DefaultBranch,
			Message:   message,
			Files: []*files_service.ChangeRepoFile{{
				Operation:     operation,
				TreePath:      appsConfigPath,
				ContentReader: bytes.NewReader(body),
			}},
		})
		if lastErr == nil {
			// In force immediately: without this the commit lands and nothing
			// changes until a restart, and the admin who just added a package
			// would watch the next deploy fail without it.
			if parsed, perr := ParseAppsConfig(body); perr == nil {
				SetAppsConfig(parsed)
			} else {
				SetAppsConfigError(perr)
			}
			return nil
		}
	}
	return lastErr
}

// ApproveAppPackages grants packages to one app without a Deploy Request.
//
// The normal route is a department asking and an admin merging, and it stays
// the normal route: the request carries a reason, and the PR puts the code
// and the permission in front of the admin together. But it depends on a
// department being able to raise one, and there are states where they cannot
// — a dependency the platform discovered after their own packages were all
// approved leaves them with nothing to ask for, and an admin watching a
// broken app should not have to talk someone through submitting a form to
// unblock it.
//
// Deliberately the same commit path as an approval, not a side channel: it
// lands in apps.yml, authored by the admin who did it, so the git history
// still answers "who allowed this, and when". The commit message says it was
// granted directly, because that is the part a later reader needs — there is
// no request to look up.
func ApproveAppPackages(ctx context.Context, doer *user_model.User, owner, repo string, packages []string) error {
	requests := make([]PermissionRequest, 0, len(packages))
	for _, name := range packages {
		requests = append(requests, PermissionRequest{
			Kind:     PermKindPackage,
			Value:    name,
			Label:    "패키지 추가",
			Detail:   name,
			Decision: "approve",
			Reason:   "관리자가 직접 승인",
		})
	}
	if len(requests) == 0 {
		return userErrorf("승인할 패키지를 선택하거나 입력해 주세요")
	}

	var lastErr error
	for attempt := range commitRetries {
		if lastErr = commitPermissionUpdate(ctx, doer, owner, repo, requests, attempt); lastErr == nil {
			// The build failed on exactly these, and they are now allowed, so
			// the record of what is outstanding has to stop saying otherwise.
			clearMissingPackages(owner, repo, packages)
			return nil
		}
	}
	return lastErr
}

// clearMissingPackages drops the names that have just been approved.
func clearMissingPackages(owner, repo string, approved []string) {
	granted := make(map[string]bool, len(approved))
	for _, name := range approved {
		granted[normalizePackageName(name)] = true
	}
	if err := MutateAppState(owner, repo, func(st *AppState) bool {
		kept := slices.DeleteFunc(slices.Clone(st.MissingPackages), func(name string) bool {
			return granted[normalizePackageName(name)]
		})
		if slices.Equal(kept, st.MissingPackages) {
			return false
		}
		st.MissingPackages = kept
		return true
	}); err != nil {
		log.Error("company: %s/%s: clearing approved packages: %v", owner, repo, err)
	}
}
