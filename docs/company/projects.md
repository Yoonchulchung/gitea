# Projects, not departments

A project belongs to the company, not to a department. Everyone signs in with
their company ID, creates projects, and works on them with whoever they need,
whatever department those people are in. Deploys still go through the
approval flow in the central repository, unchanged.

## Where a project lives

Every project is a repository in one organization, `[company] PROJECT_ORG`
(default `projects`), created at the first start with the first administrator
as its owner and *limited* visibility — Gitea hides an organization's
repositories from anyone who may not see the organization itself, and nobody
but administrators is a member of it. The organization is then followed by
id (`company.project_org_id` in the settings table), so renaming it does not
lose the projects.

One namespace for all projects is what keeps addresses stable: the project's
URL and its app's `/apps/{owner}/{repo}` never change with who owns it, and
the app's policy in apps.yml, its data and its releases, all keyed by that
name, never have to move.

## Owner and members

- **Access** is Gitea's own collaboration: the owner is the repository's
  admin collaborator — which is what lets them delete it once its app has
  left the platform (`company/appremoval.go`) — and every member a write
  collaborator. Nothing else grants it.
- **Ownership** is one row in `company_project_owner` (repo id → user id),
  registered with `db.RegisterModel` so Gitea creates the table itself. It
  says which collaborator answers for the project; it grants nothing on its
  own.
- The owner adds and removes members from the project's sidebar
  (`POST /company/project/{owner}/{repo}/members`); an administrator can too.
  The owner cannot be removed — a new owner is named first.
- `company/project.go` holds the model, `company/project_pages.go` the
  new-project page (`/company/projects/new`) and the sidebar panel.

## Naming a new owner — any time, including after someone left

`/-/admin/company-projects` lists every project with its owner and leads
with the ones whose owner is gone: deleted, deactivated, or barred from
signing in. Any project's owner can be changed there, by username. The
previous owner stays on as a write member unless "remove previous" is
ticked, so a handover never locks out somebody still working on it.

On an administrator's user page (`/-/admin/users/{id}/edit`) the projects
that person owns are listed with one form that hands all of them to someone
else — what an administrator needs on someone's last day.

Account deletion is switched off on this platform (deactivate instead), but
if an account does disappear, its projects stay where they are and show up
as needing an owner.

## Moving the old department repositories

Repositories created before this model are still owned by their department
organizations. The lower half of `/-/admin/company-projects` lists them with
what moving each one means, and one button moves every one that can move
(`company/project_migrate.go`):

1. The repository is transferred into the projects organization as it is —
   same name, history, branches, LFS. The transfer notifiers move the app with
   it (`company/apprename.go`): its releases, data, environment, access
   choice and its apps.yml block follow the new name, and a running app is
   redeployed from the commit it was serving.
2. Every member of the department becomes a project member, so nobody loses
   access; administrators are skipped, they reach everything anyway.
3. Whoever created the repository becomes the owner, or — when that was an
   administrator setting it up — whoever last asked to deploy it. If neither
   is somebody who can still sign in, the project is listed as needing an
   owner.

A repository is left where it is when a project of the same name already
exists (rename one first), when it has a deploy request waiting for a
decision (its snapshot names the department; approved after the move it would
deploy to an app that is no longer there), or while it is being created or
transferred. Running the migration again picks up whatever was skipped.

The old addresses keep working: Gitea redirects the repository, and
`AppProxy` forwards `/apps/{department}/{repo}` to the app's new address.
The department organizations are left in place, empty; an administrator may
delete them.

## Sign-in only, no sign-up

Accounts come from the company directory. `InitAppPlatform` forces
`DISABLE_REGISTRATION` on whatever app.ini says, the gate answers
`/user/sign_up` with a 404 for anonymous visitors, and the sign-in page and
navbar carry no registration link. The username field is labelled
"Company ID".

## Admin sees every project

No setting needed — it's unconditional. `GetIndividualUserRepoPermission`
short-circuits to full owner access for any `IsAdmin` user.

## An administrator's copy for testing

Testing on a project itself touches what it is serving. So the admin app page
(`/-/admin/company-deploys/{owner}/{repo}`) copies a project to the projects
organization or the administrator's own namespace (`company/admin_copy.go`):
every branch, tag and LFS object, and optionally the app's database and
environment variables. A copy among the projects is owned by the
administrator until they name somebody else. A running app's database is
copied from a fresh snapshot, never as a live file. The apps.yml policy is
not copied — the copy gets its own through a deploy request.
