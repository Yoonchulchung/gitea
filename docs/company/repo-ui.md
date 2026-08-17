# Repo page nav — what's on, what's hidden, what's blocked

Target: only **Code** stays as a real, native tab. Everything else in the
screenshot below (Issues, Pull Requests, Actions, Packages, Projects,
Releases, Wiki, Activity) is hidden from nav for non-admins, and the
underlying route is blocked too — not just visually hidden.

```
PO / --
Code  Issues  Pull Requests  Actions  Packages  Projects  Releases  Wiki  Activity
```

Three different mechanisms are needed, because not every tab behaves the
same way underneath.

## Fully disabled — config only, tab and route both gone for everyone

```ini
[repository]
DISABLED_REPO_UNITS = repo.issues,repo.wiki,repo.projects,repo.packages
```

Issues, Wiki, Projects, Packages aren't needed anywhere in this workflow.
Disabling the unit removes the tab (`templates/repo/header.tmpl` reads the
same `DisabledRepoUnits` list) and 404s the route for every role, admin
included. No template edit needed for these four.

## Must stay functionally alive — tab hidden, route gate-blocked, not disabled

**Pull Requests** and **Actions** cannot go in `DISABLED_REPO_UNITS` — they're
load-bearing:

- Pull Requests is the actual mechanism behind Request Deploy → admin review
  (`architecture.md`). Disabling the unit would break PR creation entirely.
- Actions is the CI gate a required status check depends on before an admin
  can approve (per the earlier decision to keep it enabled).

So for these two: unit stays enabled, but
1. the nav `<a>` for the tab only renders for admins in the
   `custom/templates/repo/header.tmpl` override (`header.tmpl:108` for Pull
   Requests, `header.tmpl:117-118` for Actions), and
2. the [whitelist gate](ui-gate.md) blocks the underlying route
   (`/{owner}/{repo}/pulls`, `/{owner}/{repo}/actions`) for non-admins, while
   admins keep full access — that's how they actually review PRs and read CI
   status.

The admin-only Pull Requests link points at `/org/{owner}/pulls` — the
department-wide PR list (`user.Pulls`, `routers/web/web.go:981`) — rather
than `{{.RepoLink}}/pulls` (this one repo's PRs only). An admin reviewing
deploy requests wants every submission across the department, not repo by
repo; the org-scoped route is already gate-whitelisted
([ui-gate.md](ui-gate.md)) and restricted to that org's members by Gitea's
own membership check either way.

## Can't be disabled at all — Gitea limitation, not our choice

**Releases**: `app.example.ini:1085-1086` states outright that Code and
Releases "can currently not be deactivated" via `DISABLED_REPO_UNITS`, even
though the unit type exists and is checked the same way as the others. The
only way to hide it is the same two-part treatment as Pull Requests/Actions
above — tab removed in the `header.tmpl` override, route
(`/{owner}/{repo}/releases`) blocked by the gate — even though we don't
actually need the Releases feature for anything.

## Activity — not a single unit, needs both layers too

`templates/repo/header.tmpl:163-164` shows the Activity tab if the viewer can
read *any* of Pull Requests, Issues, Releases, or Code
(`Permission.CanReadAny(...)`) — there's no dedicated "Activity" unit to
disable. Remove the tab in the same `header.tmpl` override, and block
`/{owner}/{repo}/activity` in the gate; the route's own comment
("activity has its own permission checks", `routers/web/web.go:1605-1607`)
confirms it doesn't rely on a single blanket unit check we could disable
instead.

## Net result

| Tab | Mechanism | Admin still has it? |
|---|---|---|
| Code | untouched | yes |
| Issues, Wiki, Projects, Packages | `DISABLED_REPO_UNITS` | no — gone for everyone |
| Pull Requests, Actions | tab hidden (`header.tmpl` override) + route gate-blocked | yes — needed for review/CI |
| Releases, Activity | tab hidden (`header.tmpl` override) + route gate-blocked | yes, but unused in practice |

One `custom/templates/repo/header.tmpl` override does the tab-hiding for all
four of the bottom two rows in one file (this is also where the "Request
Deploy" button gets added, per [architecture.md](architecture.md)). The
[whitelist gate](ui-gate.md) does the route-blocking for the same set, plus
the earlier global `/pulls`, `/milestones` dashboard entries.
