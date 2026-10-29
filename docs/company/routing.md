# Anonymous → login, login → dashboard

## Anonymous user hits `/` → send to `/user/login`

Config only, zero code:

```ini
[server]
LANDING_PAGE = login
```

`LandingPageURL` resolves `login` to `/user/login` and is applied in the
anonymous branch of `Home()` (`routers/web/home.go:51-52`). It can also be an
arbitrary path (`LANDING_PAGE = /company`), but see the next section for why
that alone isn't the right lever for logged-in users.

`LANDING_PAGE` **only affects anonymous visitors.** `Home()` branches on
`ctx.IsSigned` (`routers/web/home.go:33`); the signed-in branch always ends
at `user.Dashboard(ctx)` (`home.go:47`) and never consults this setting —
confirmed by reading the branch, not inferred.

## Logged-in regular user → their own dashboard

There is no `GET /company` landing page. Whatever URL a signed-in,
non-admin user lands on after login — `/`, a bookmarked link, anything not
on the [whitelist](ui-gate.md) — the gate (`company/gate.go`) sends them to
`/`, their own dashboard: the projects they own or are a member of, whatever
department they are in ([projects.md](projects.md)), with their apps beside
them. Nothing about the person has to be looked up first, so there is no
"not assigned yet" page either.

This also means a plain `/user/login` (no `redirect_to` at all) works fine:
whatever page a signed-in non-admin lands on afterward, if it isn't
whitelisted, the gate immediately forwards them to their dashboard. No
separate `Home()` edit or landing route was ever needed for this — the
gate's default action *is* the redirect.

Anonymous visitors are a separate, simpler path: `REQUIRE_SIGNIN_VIEW = true`
+ `LANDING_PAGE = login` (both config, `app.ini`) send them straight to
`/user/login` before they see anything — the gate skips anonymous requests
entirely (`ctx.Doer == nil`) so there's no extra hop through this package at
all for that case.

