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

## An administrator's copy for testing

Personal namespaces are closed to departments, and testing on a
department's own repository touches what it is serving. So the admin app
page (`/-/admin/company-deploys/{owner}/{repo}`) copies a department
repository to any department or the administrator's own namespace
(`company/admin_copy.go`): every branch, tag and LFS object, and optionally
the app's database and environment variables. A running app's database is
copied from a fresh snapshot, never as a live file. The apps.yml policy is
not copied — the copy gets its own through a deploy request.

## Departments from the directory

With a department attribute named in Admin → company settings (usually
`department` in Active Directory), every LDAP login puts the person in that
department (`company/ldap_department.go`, hooked into the login search — see
[patches.md](patches.md)). An organization is found by its name or its
display name; if there is none it is created, owned by the first
administrator, named `dept-<hash>` when the department name cannot be an
organization name (Korean ones cannot), with the department as its display
name. Renaming an existing organization's display name to the directory's
department name maps it. A person whose department changes in the directory
is moved at their next login, out of the department the directory last put
them in and no other; an owner is never removed.
