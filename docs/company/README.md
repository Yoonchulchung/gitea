# Company customization

Internal notes for adapting this Gitea fork into a code-intake platform for
non-technical staff: users without git experience upload files, an admin
reviews and approves, and approval pushes to the real external Git server.

This is a living design record, not upstream documentation — everything here
describes changes layered on top of stock Gitea, structured so upgrades stay
simple: either the change lives outside anything upstream will ever touch
(`custom/`, `company/`), or it's a single tracked line logged in
[patches.md](patches.md).

This fork tracks `custom/templates` in git (both genuine overrides of
upstream template files, and `custom/templates/company/`'s own brand-new
pages — neither is upstream source, so neither can conflict with an
upstream rebase). Only `custom/conf` (local secrets/paths) stays
`.gitignore`d — see `.gitignore`.

## Contents

- [architecture.md](architecture.md) — end-to-end flow and which parts reuse
  existing Gitea features vs. need new code
- [departments.md](departments.md) — organizations-as-departments model,
  personal-repo restriction, auto team assignment
- [admin-activity.md](admin-activity.md) — cross-organization activity feed
  for admins
- [mount-points.md](mount-points.md) — how the `company/` package attaches to
  Gitea's router without editing existing route logic
- [routing.md](routing.md) — anonymous → login → department dashboard landing behavior
- [repo-ui.md](repo-ui.md) — which repo tabs are disabled, hidden, or
  gate-blocked, and why they need different treatment
- [ui-gate.md](ui-gate.md) — whitelist gate blocking arbitrary-URL access to
  native Gitea screens for non-admin users
- [patches.md](patches.md) — the running list of every touch point in stock
  Gitea source, however small, with file/line and reason
- [app-platform.md](app-platform.md) — app deployment platform: architecture,
  security model, threat analysis, and the decisions behind them
- [app-platform-impl.md](app-platform-impl.md) — implementation contract for
  the platform: concurrency, hot-path performance, fail-open/closed rules,
  security rules, test requirements — read before writing code
- [app-platform-tasks.md](app-platform-tasks.md) — phased `[ ]` checklist
  tracking implementation progress

## Guiding principle

Prefer, in this order:
1. Existing Gitea feature, configured via `app.ini` (`custom/conf`, not
   tracked) — no source touched.
2. `custom/templates` / `custom/public` / `custom/options` — layered at
   runtime over the built-in assets, tracked in git for this fork. Covers
   two different things, both kept here: genuine overrides of an existing
   upstream template (diff it on every upgrade — see
   [architecture.md](architecture.md)), and `company/`'s own brand-new
   pages (never existed upstream, nothing to diff).
3. New, additive Go files under `company/` (or `docs/company/`) — never
   edits an existing file, so nothing to rebase.
4. A single-line wiring change in an existing core file, only when there is
   no other way to attach — always logged in [patches.md](patches.md).

Editing the body of an existing core file is a last resort and, so far, has
not been necessary for anything in this plan.
