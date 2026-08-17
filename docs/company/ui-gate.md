# Whitelist gate for the native Gitea UI

Redirecting the *default* landing page to `/company` (see
[routing.md](routing.md)) doesn't stop a non-admin user from reaching any
Gitea screen they already have permission for by typing the URL directly —
that's a UX gap, not a security one (Gitea's own permission checks still
apply either way). Closing it needs an actual gate, default-deny by path.

Default-deny (whitelist) instead of blocklisting `/explore/repos`,
`/milestones`, etc. one by one: the whitelist only grows when `company`
intentionally exposes a native page, so a new Gitea screen in a future
release is blocked automatically instead of needing to be added to a
blocklist. This is what keeps the operational surface small.

## Mechanism

One line in `routers/web/web.go`'s `Routes()`, in the same middleware slice
that already resolves the signed-in user:

```go
// routers/web/web.go:302-318 (existing)
mid = append(mid, common.MustInitSessioner(), context.Contexter())
webAuth := newWebAuthMiddleware()
mid = append(mid, webAuth.MiddlewareHandler)   // ctx.Doer is populated here
mid = append(mid, company.GateNonAdminUI)      // <-- the one added line
mid = append(mid, goGet)
...
webRoutes.AfterRouting(mid...)
registerWebRoutes(webRoutes, webAuth)
```

`AfterRouting` (`modules/web/router.go:100`) runs for every route registered
on that same `webRoutes` router — repo pages, explore, milestones, admin,
everything in `registerWebRoutes` — so this single insertion covers the
entire native web UI in one place. It does **not** cover `/api/v1` or
`/api/internal`, which are separate `web.Router` mounts (`routers/init.go`)
with their own, token-based auth — the `company` backend keeps calling those
directly, ungated.

All matching logic — the whitelist, the redirect target, the admin bypass —
lives in `company/`. `web.go` only knows one function name.

## Gate logic

```go
// company/gate.go
func GateNonAdminUI(ctx *context.Context) {
    if ctx.Doer != nil && ctx.Doer.IsAdmin {
        return // admins keep full native access — see departments.md
    }
    if isExempt(ctx.Req.URL.Path) {
        return
    }
    ctx.Redirect(setting.AppSubURL + "/company")
}
```

`isExempt` must never send an HTML redirect to a git client — the following
are exempt unconditionally, not just for regular users:

- Git smart-HTTP: `/{owner}/{repo}/git-upload-pack`, `/git-receive-pack`,
  `/info/refs`, and LFS paths (`routers/web/githttp.go:12-17`, registered on
  the same router — confirmed these are swept up by the gate unless
  explicitly excluded). A redirect response here just breaks the git client.
- Auth flow: `/user/login`, `/user/logout`, password reset.
- Static assets (avatars, `/assets`, manifest, favicon) — anything the login
  page and `/company` itself need to render.
- `company`'s own tree (`/company/...`), obviously.

## Whitelist (starting point — adjust in `company/`, not here)

Prefer the org-scoped variant over the global/personal one wherever both
exist — Gitea already restricts the org-scoped routes to that org's members
via `context.OrgAssignment(RequireMember: true, RequireTeamMember: true)`
(`routers/web/web.go:987`), so whitelisting them doesn't leak across
departments even though the path itself doesn't encode that check.

| Path | Allowed for non-admins? | Why |
|---|---|---|
| `/{username}/{reponame}` (repo browsing, Code tab only) | yes | |
| `/repo/create` | yes | |
| `/org/{org}/pulls`, `/org/{org}/milestones`, `/org/{org}/dashboard`, `/org/{org}/issues` | yes | scoped to the caller's own department by Gitea's own org-membership check (`web.go:974-987`) |
| `/pulls`, `/milestones` (global personal dashboard, `web.go:576-577`) | no → `/company` | aggregates across every repo the user can reach — redundant with, and less scoped than, the `/org/{org}/...` equivalents above |
| `/{username}/{reponame}/pulls` (`web.go:1321`), `/actions` (`web.go:1549`), `/releases` (`web.go:1493`), `/activity` (`web.go:1605`) | no → `/company` | unit stays enabled (needed internally, or can't be disabled — see [repo-ui.md](repo-ui.md)), so the route is only closed off by this gate, not by config |
| `/explore/repos` | no → `/company` | |
| everything else not listed | no → `/company` | |

Issues/Wiki/Projects/Packages routes don't need a gate entry — disabling
those units (`repo-ui.md`) already 404s them for every role, admin included,
so there's nothing to whitelist or block.

Keep this table and the actual Go slice in sync; the table is the
operational reference, the slice is the enforcement.

## Consequence for routing.md

This closes the gap noted in [routing.md](routing.md) (bare `/user/login`
landing a regular user on Gitea's own dashboard): `/` itself isn't
whitelisted, so the gate redirects to `/company` regardless of how the user
reached a signed-in state. The optional `Home()` patch described there is no
longer needed — see [patches.md](patches.md).
