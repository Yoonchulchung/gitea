# Anonymous → login, login → `/company`

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

## Logged-in regular user → their department's dashboard

There is no `GET /company` landing page. Whatever URL a signed-in,
non-admin user lands on after login — `/`, a bookmarked link, anything not
on the [whitelist](ui-gate.md) — the gate (`company/gate.go`,
`redirectToDepartment`) resolves their org/team membership (the same
assignment LDAP/OAuth2 group sync applies at login,
[departments.md](departments.md)) and sends them straight to
`/org/{their-org}/dashboard`. One department → one redirect, no intermediate
page. Zero or multiple departments falls back to a plain-text list — an edge
case, not the common path.

This also means a plain `/user/login` (no `redirect_to` at all) works fine:
whatever page a signed-in non-admin lands on afterward, if it isn't
whitelisted, the gate immediately forwards them to their department
dashboard. No separate `Home()` edit or landing route was ever needed for
this — the gate's default action *is* the redirect.

Anonymous visitors are a separate, simpler path: `REQUIRE_SIGNIN_VIEW = true`
+ `LANDING_PAGE = login` (both config, `app.ini`) send them straight to
`/user/login` before they see anything — the gate skips anonymous requests
entirely (`ctx.Doer == nil`) so there's no extra hop through this package at
all for that case.

