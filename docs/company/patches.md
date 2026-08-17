# Patch log

Every touch point in stock Gitea source, however small. Check this list
after each upstream rebase/merge — everything else in `docs/company/` is
config or additive files and needs no review.

| File | What | Why | Status |
|---|---|---|---|
| `routers/init.go` | `r.Mount("/company", company.Routes())` | Attach the user-facing FE's routes | planned |
| `routers/web/web.go` | `company.RegisterAdminRoutes(m)` inside the existing `/-/admin` group | Cross-org activity view in the admin panel | planned |
| `routers/web/web.go` | `mid = append(mid, company.GateNonAdminUI)` in `Routes()`, right after the auth middleware is appended | Default-deny whitelist so non-admins can't reach native Gitea screens by typing a URL — see [ui-gate.md](ui-gate.md) | planned |

Three lines total, all in files that only change when Gitea's own routing
skeleton changes — rare, and each is a self-contained `append`/call, easy to
re-locate even if the surrounding lines shift.

Nothing else is planned. Config (`app.ini`), `custom/` overrides, and new
files under `company/` are not listed here — they don't touch tracked core
source, so an upstream rebase never has anything to reconcile for them.
