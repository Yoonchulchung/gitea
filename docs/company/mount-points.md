# Attaching `company/` to Gitea's router

Gitea already mounts independent route trees this way — the API and
internal-API routers are the precedent to follow, not a new pattern:

```go
// routers/init.go:183-185
r.Mount("/", web_routers.Routes())
r.Mount("/api/v1", apiv1.Routes())
r.Mount("/api/internal", private.Routes())
```

Both `apiv1.Routes()` (`routers/api/v1/api.go:1015`) and `private.Routes()`
(`routers/private/internal.go:67`) are `func Routes() *web.Router` defined
entirely in their own package. `*web.Router` (`modules/web/router.go:75`) is
Gitea's own wrapper with public `Group`/`Get`/`Post`/`Mount` methods
(`router.go:92-322`) — any package holding a reference can build routes on
it, not just `web.go`.

## Two attachment points this plan uses

**New top-level namespace for the user-facing FE** — zero edits to
`routers/web/web.go`:

```go
// company/routes.go
func Routes() *web.Router {
    r := web.NewRouter()
    // upload, submission-status, etc. — entirely inside this package
    return r
}
```
```go
// routers/init.go — one added line
r.Mount("/company", company.Routes())
```

**Cross-org admin activity view, nested inside the existing admin panel** —
one added line inside an existing closure, no other change:

```go
// routers/web/web.go:780-899, m.Group("/-/admin", func() { ... })
// added just before the closing brace at line 899:
company.RegisterAdminRoutes(m)
```

Placed inside that closure, it inherits the `adminReq` middleware already
applied to the whole `/-/admin` group for free — no separate auth wiring.

Both are logged in [patches.md](patches.md): one line each, in files that
change rarely (top-level router wiring), so upgrade risk is low and the diff
is trivial to re-apply even if it doesn't auto-merge.
