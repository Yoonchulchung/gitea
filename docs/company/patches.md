# Patch log

Every touch point in stock Gitea source, however small. Check this list
after each upstream rebase/merge — everything else in `docs/company/` is
config or additive files and needs no review.

| File | What | Why | Status |
|---|---|---|---|
| `routers/init.go` | `r.Mount("/company", company.Routes())` | Attach the user-facing FE's routes | planned |
| `routers/web/web.go` | `company.RegisterAdminRoutes(m)` inside the existing `/-/admin` group | Cross-org activity view in the admin panel | planned |
| `routers/web/web.go` | `mid = append(mid, company.GateNonAdminUI)` in `Routes()`, right after the auth middleware is appended | Default-deny whitelist so non-admins can't reach native Gitea screens by typing a URL — see [ui-gate.md](ui-gate.md) | planned |
| `routers/web/web.go` | `company.RedirectToWorkspaceIfEmpty` inserted into the bare `/{username}/{reponame}` route's middleware chain, right before `repo.Home` | Send a brand-new empty repo straight to the workspace editor instead of Gitea's native "empty repository" git-clone-instructions page — see `company/workspace.go` | done |
| `templates/base/head.tmpl` | `?v=2` query on the two favicon `<link>` hrefs | Browsers cache favicons per-origin and never refetch on reload — origins visited before the custom AUMOVIO favicon landed (e.g. `0.0.0.0:3000`) kept showing the stock Gitea icon forever; changing the URL forces one refetch. Bump the number if the icon artwork ever changes again | done |

Four lines total, all in files that only change when Gitea's own routing
skeleton changes — rare, and each is a self-contained `append`/call, easy to
re-locate even if the surrounding lines shift.

Nothing else is planned. Config (`app.ini`), `custom/` overrides, and new
files under `company/` are not listed here — they don't touch tracked core
source, so an upstream rebase never has anything to reconcile for them.
`APP_NAME` in `custom/conf/app.ini` is what controls the "Gitea: Git with a
cup of tea" suffix in page titles — set to a real, non-empty product name
rather than blank, since `go-ini`'s `MustString` can't tell "explicitly
blank" from "absent" apart and would've silently kept the default either
way, which would've needed a core patch to work around.
