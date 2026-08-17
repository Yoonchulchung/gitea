# Organizations as departments

Each Gitea organization = one department. Repos live under the
organization, never under a personal user namespace.

## Block personal-namespace repo creation

Repo-creation limits are keyed on the **owner** of the target namespace, so
the personal and organization limits are independent:

```ini
[repository]
USER_MAX_CREATION_LIMIT = 0   ; blocks personal-namespace creation only
; ORG_MAX_CREATION_LIMIT left at its default — org creation unaffected
```

`CanCreateRepoIn(owner)` (`models/user/user.go:261-274`) checks
`owner.NumRepos < owner.MaxCreationLimit()`; for a personal-namespace create,
`owner` is the user, so this setting alone does the job. Config only.

A per-user override also exists (`User.MaxRepoCreation`,
`models/user/user.go:117`, editable in Admin → Users), for exceptions.

**Gap:** setting this doesn't remove the personal-namespace option from the
repo-creation dropdown — it stays visible with a "can't create" warning
(`templates/repo/create.tmpl:19-26`). Hiding it outright needs a
`custom/templates/repo/create.tmpl` override (tier 2, not a core edit).

## Auto-assign a department on first login

LDAP and OAuth2/OIDC auth sources both support group→team sync, applied at
login/re-login time:

- Core: `services/auth/source/source_group_sync.go` (`SyncGroupsToTeams`)
- LDAP fields: `GroupsEnabled`, `GroupTeamMap`, `GroupTeamMapRemoval`
  (`services/auth/source/ldap/source.go:54,58-59`)
- OAuth2 fields: `GroupTeamMap`, `GroupTeamMapRemoval`
  (`services/auth/source/oauth2/source.go:27-28`)
- Configured entirely in Admin → Authentication Sources (JSON map of
  `external-group -> {org: [teams]}`)

Config only, no code. **The organization and team must already exist** —
sync only manages membership, not org/team creation, so departments need to
be seeded once up front.

## Admin sees every department's repos

No setting needed — it's unconditional. `GetIndividualUserRepoPermission`
short-circuits to full owner access for any `IsAdmin` user, regardless of
org membership or collaborator status:

```go
// models/perm/access/repo_permission.go:442-445
if user.IsAdmin || user.ID == repo.OwnerID {
    perm.AccessMode = perm_model.AccessModeOwner
    return perm, nil
}
```
