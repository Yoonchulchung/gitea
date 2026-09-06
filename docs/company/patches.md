# Patch log

Every touch point in stock Gitea source, however small. Check this list
after each upstream rebase/merge — everything else in `docs/company/` is
config or additive files and needs no review.

| File | What | Why | Status |
|---|---|---|---|
| `routers/web/web.go:366` | `company.RegisterRoutes(m)` inside `registerWebRoutes` | Attach the user-facing POST endpoints. **Mounted here, not `routers/init.go`** as [mount-points.md](mount-points.md) originally described — inside `registerWebRoutes` it inherits the session/auth middleware, which the routes need | done |
| `routers/web/web.go:914` | `company.RegisterAdminRoutes(m)` inside the existing `/-/admin` group | Cross-org activity view in the admin panel; inherits `adminReq` | done |
| `routers/web/web.go:315` | `mid = append(mid, company.GateNonAdminUI)` in `Routes()`, right after the auth middleware is appended | Default-deny whitelist so non-admins can't reach native Gitea screens by typing a URL — see [ui-gate.md](ui-gate.md) | done |
| `routers/web/web.go` | `company.RedirectToWorkspaceIfEmpty` inserted into the bare `/{username}/{reponame}` route's middleware chain, right before `repo.Home` | Send a brand-new empty repo straight to the workspace editor instead of Gitea's native "empty repository" git-clone-instructions page — see `company/workspace.go` | done |
| `templates/base/head.tmpl` | `?v=2` query on the two favicon `<link>` hrefs | Browsers cache favicons per-origin and never refetch on reload — origins visited before the custom AUMOVIO favicon landed (e.g. `0.0.0.0:3000`) kept showing the stock Gitea icon forever; changing the URL forces one refetch. Bump the number if the icon artwork ever changes again | done |
| `web_src/js/features/notification.ts` | Both background-poll `GET` calls now send `X-Gitea-Fetch-Action: 1` | Without it, a session that expires between polls gets 303'd to `/user/login?redirect_to=/notifications/...`; the login page's GET handler stashes that into the `redirect_to` cookie (`modules/web/middleware/cookie.go`), so the *next* real login silently lands on the raw notifications JSON/HTML fragment instead of wherever the person actually was — this is what "로그인하면 이쪽으로 리다이렉팅 될 때 있어" was. Upstream bug, not something we introduced | done |
| `routers/web/web.go` | `company.SetDeployRequestAIReviewData` inserted into the native `/{owner}/{repo}/pulls/{index}` view's middleware chain, right before `repo.ViewIssue` | Sets the template data an "AI 코드 리뷰" button on the native PR page needs to render — Deploy Request PRs on the central deploy repo only, admin-only, only when the viewer has their own AI settings configured — see `company/pull_ai_review.go` | done |
| `templates/repo/issue/view_content.tmpl` | Full `custom/` override adding one conditional `<form>` (the AI review button) into the native comment/status-button footer | Same feature as above — the button itself; `.ShowDeployAIReview`/`.DeployAIReviewURL` come from the middleware entry just above | done |
| `routers/init.go` | One `company.InitAppPlatform()` call added at the end of `GlobalInitInstalled`, just before `cron.Init(ctx)` | Starts the deployed-app platform: probes the sandbox, launches the deploy workers, and reconciles apps that were running before the restart. It cannot be a package `init()` — it needs settings and the database, which are only up by this point. One call rather than several so an upstream merge has one line to reconcile, not four — see `company/appinit.go` | done |
| `routers/web/web.go` | Three routes added next to the existing `/deploy` ones: `GET /{owner}/{repo}/_app`, `POST …/_app/env`, `POST …/_app/{verb}` | The department's own app screen — start/stop/restart/rollback, environment variables, access mode. Named `_app` rather than `_deploy` because `/deploy` already exists one underscore away; the prefix follows the existing `_edits`/`_edits_ai` convention. Reuses the group's `reqRepoCodeWriter` for writes | done |
| `routers/web/web.go` | `company.SetAppPermissionData` inserted into the `/{username}/{reponame}` repo-home middleware chain, after `RedirectToWorkspaceIfEmpty` | Supplies the repository sidebar's app panel — status, what the app is and is not allowed to do, and why. Reads only in-memory state, so it adds no query or file read to the most-visited page. Same injection pattern as the two entries above | done |
| `templates/admin/navbar.tmpl` | Full `custom/` override adding one `<details>` group with two links (app deployments, activity) | The admin menu had no entry for either company screen, so `/-/admin/company-activity` could only be reached by typing its URL. Gitea has no partial-template insertion, so a full copy is the only option — accepted here because this file is a list of links with almost no Go-symbol surface, and the worst drift outcome is a missing upstream menu item rather than a broken page | done |
| `cmd/main.go` | `company.NewDeptAppExecCommand()` added to `subCmdStandalone` | Registers the hidden `deptapp-exec` subcommand that department apps are started through on hosts where bubblewrap cannot run. It has to be a subcommand of this binary because Landlock and seccomp must be entered between fork and exec, and Go's `os/exec` has no hook there — the child locks itself down and then `execve`s the app. Listed as standalone because it reads no config and opens no database | done |
| `routers/web/web.go` | `company.SetDashboardApps` inserted into the two `/{org}/dashboard` routes | Supplies the dashboard's deployed-app panel. A repository is not an app — most never become one, and the list says nothing about whether the ones that did are up — so "is our thing running?" took opening repositories one at a time. Same injection pattern as the entries above; scoped to organizations the viewer already belongs to | done |
| `templates/user/dashboard/dashboard.tmpl` | Full `custom/` override adding one line after the repo list | Renders the panel above. A 17-line structural file with no Go-symbol surface, which is why a full copy is acceptable here — the drift check (`docs/company/scripts/check-template-drift.sh`) covers it either way | done |

Several of these are files that only change when Gitea's own routing skeleton
changes — rare, and each is a self-contained `append`/call, easy to
re-locate even if the surrounding lines shift. The template override is a
full-file copy (Gitea's overlay mechanism has no partial-template diffing),
so it needs re-diffing against upstream on every rebase that touches
`templates/repo/issue/view_content.tmpl` — the same tradeoff already
accepted for `custom/templates/repo/view_content.tmpl` and the fully custom
`workspace.tmpl`/`deploy.tmpl`.

Nothing else is planned. Config (`app.ini`), `custom/` overrides, and new
files under `company/` are not listed here — they don't touch tracked core
source, so an upstream rebase never has anything to reconcile for them.
`APP_NAME` in `custom/conf/app.ini` is what controls the "Gitea: Git with a
cup of tea" suffix in page titles — set to a real, non-empty product name
rather than blank, since `go-ini`'s `MustString` can't tell "explicitly
blank" from "absent" apart and would've silently kept the default either
way, which would've needed a core patch to work around.
