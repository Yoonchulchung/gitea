# Cross-organization activity feed for admins

Not achievable via config — Gitea has no built-in view that aggregates
activity across all organizations. New code is required, but it's additive
(new query + new route), not an edit to an existing file.

## Why the built-in dashboard doesn't do this

`user.Dashboard` (`routers/web/user/home.go:78`) — used for both a user's
own dashboard and an org's `/org/{org}/dashboard` — always scopes the feed
to one `RequestedUser` (`models/activities/action.go:534-535`:
`cond.And(builder.Eq{"user_id": opts.RequestedUser.ID})`). This holds even
for `IsAdmin` actors; admin status only lets the *visibility* filter be
skipped (`action.go:512`), not the single-org scoping.

Admins can already open any single org's dashboard without being a member —
`services/context/org.go:111-116` forces `IsMember`/`IsOwner` true for
`IsAdmin` doers — but that's one org at a time, not aggregated.

## What a cross-org feed needs

The `Action` table (`models/activities/action.go:135-151`) has `RepoID`,
not `OrgID`, but repos link to their owning org via `repository.owner_id`.
A new query analogous to `AccessibleRepoIDsQuery`
(`models/repo/repo_list.go:746-748`) — "all actions whose repo's owner is an
organization" — covers it without any per-org membership check.

## Where it lives

Keep this out of Gitea entirely: the `company` backend (already calling
Gitea's API/DB for the upload flow) aggregates org list → org repos →
activity, and renders it in the `company` admin view. Gitea source stays
untouched. See [mount-points.md](mount-points.md) for how `company`'s admin
routes attach.
