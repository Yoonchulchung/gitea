# Architecture

## Flow

1. **User** (no git knowledge, pre-assigned to a department) drags files into
   the `company` FE.
2. **`company` backend** talks to Gitea's REST API on the user's behalf:
   fork (if needed) → commit files to a branch → open a pull request.
3. **Admin** reviews the PR in Gitea's own, unmodified pull-request UI (diff,
   comments, required-approval rules, Actions status checks).
4. On merge into the default branch, a **push mirror** syncs the commit to
   the real external Git server.

```
user (company FE) --API--> Gitea (fork + commit + PR)
admin ------------------------------> Gitea PR review (stock UI)
                                              |
                                         merge -> push mirror -> external git
```

Regular users never see Gitea's own web UI. Admins do — they already know
git vocabulary, so the stock PR screen needs no relabeling.

## Requirement → implementation

| Requirement | How | Notes |
|---|---|---|
| Upload without a git identity | `company` FE + Gitea REST API (`CreateFork`, `contents` create-file, `CreatePullRequest`) | `routers/api/v1/api.go:1385,1588,1514` |
| Admin approval required before merge | Branch protection `RequiredApprovals` + approver whitelist | `models/git/protected_branch.go:56-59`, config only |
| CI must pass before merge | Branch protection `EnableStatusCheck` + `StatusCheckContexts`, backed by Actions | `models/git/protected_branch.go:54-55`, config only |
| Approved code reaches the real Git server | Push mirror, triggered on merge via webhook → sync API | `models/repo/pushmirror.go`, `routers/api/v1/repo/mirror.go:80` (`POST /repos/{owner}/{repo}/push_mirrors-sync`) |
| Unneeded features removed | `[repository] DISABLED_REPO_UNITS`, `[packages] ENABLED=false` | `modules/setting/repository.go:51`, `packages.go:17` — config only, Actions kept enabled |

See [departments.md](departments.md) for the organization/department model
and [admin-activity.md](admin-activity.md) for the cross-org admin view.

## Why a separate FE instead of reskinning Gitea's own pages

Reusing Gitea's own templates for the user-facing flow means editing
`templates/repo/create.tmpl`, PR list/create pages, etc. Those overrides
don't git-conflict (`custom/templates` isn't tracked), but they can silently
rot: if a future Gitea version changes what a handler passes into a
template, an old override keeps rendering without error.

The REST API is a more stable surface than internal template variables —
Gitea maintains it with real backward-compatibility discipline. Building the
user-facing screens against the API in `company/` avoids editing or shadowing
any Gitea template at all.
