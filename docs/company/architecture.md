# Architecture

## Flow

1. **User** (no git knowledge, pre-assigned to a department) drags files into
   the `company` FE.
2. **`company` backend** talks to Gitea's REST API on the user's behalf:
   fork (if needed) → commit files to a branch → open a pull request.
3. **Admin** reviews the PR in Gitea's own, unmodified pull-request UI (diff,
   comments, required-approval rules, Actions status checks).
4. On merge into the default branch, **Gitea itself builds and runs the app**:
   the deploy worker installs its environment, swaps the release, health-checks
   it, and serves it at `/apps/{owner}/{repo}` through an in-process reverse
   proxy. There is no push mirror and no external git server — the plan to
   mirror outward was replaced by the app platform, which is documented in
   [app-platform.md](app-platform.md).

```
user (workspace editor) ----> department repo (direct commits)
      [Deploy Request] ----> central-deploy PR (snapshot + request log)
admin ---------------------> PR review (stock UI + permission items)
                                   |
                              merge -> apps.yml updated -> build -> swap
                                              |
                                    /apps/{owner}/{repo}  (Gitea as proxy)
```

Regular users never see Gitea's own web UI. Admins do — they already know
git vocabulary, so the stock PR screen needs no relabeling.

## Requirement → implementation

| Requirement | How | Notes |
|---|---|---|
| Upload without a git identity | `company` FE + Gitea REST API (`CreateFork`, `contents` create-file, `CreatePullRequest`) | `routers/api/v1/api.go:1385,1588,1514` |
| Admin approval required before merge | Branch protection `RequiredApprovals` + approver whitelist | `models/git/protected_branch.go:56-59`, config only |
| CI must pass before merge | Branch protection `EnableStatusCheck` + `StatusCheckContexts`, backed by Actions | `models/git/protected_branch.go:54-55`, config only |
| Approved code actually runs | Deploy worker builds a venv, swaps the release, health-checks, auto-rolls-back; Gitea reverse-proxies `/apps/{owner}/{repo}` to the app's unix socket | `company/deployworker.go`, `company/proxy.go` — see [app-platform.md](app-platform.md) |
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
