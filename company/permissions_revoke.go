// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"strings"

	"gitea.dev/modules/log"
	"gitea.dev/services/context"
)

// Handing a permission back.
//
// The platform's rule is direction, not rank: widening needs an approval,
// narrowing applies immediately, because there is no reason to make somebody
// wait to be safer (company/permissions.go). Until now only half of it was
// built — a department could ask for more and only an administrator could
// take anything away, so "we do not need that any more" had to go through a
// person, and mostly did not go at all. An approval flow that can only ever
// add is a ratchet.
//
// The commit is the same one an administrator's change makes, authored by
// whoever pressed the button: `.company/apps.yml` in the central repository,
// so "who closed this, and when" is still a question git answers on its own
// (company/permissions_commit.go). Note that ChangeRepoFiles enforces branch
// protection but not repository write access, so this route's own check is
// the one that matters — see docs/company/permission-lifecycle.md.
//
// Inbound access is not here: a department already narrows it on this same
// page, and it lives in app state rather than the policy file
// (SetDepartmentAccess, company/appaccess.go).

// RevokeOwnPermission withdraws one of this app's own approved permissions.
//
// Mounted behind reqRepoCodeWriter (routers/web/web.go), so the person
// pressing it can already change the code this permission is for.
func RevokeOwnPermission(ctx *context.Context) {
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name
	value := strings.TrimSpace(ctx.FormString("value"))

	var err error
	switch ctx.FormString("kind") {
	case PermKindPackage:
		err = RevokeAppPackage(ctx, ctx.Doer, owner, name, value)
	case PermKindNetwork:
		if value == "" {
			ctx.HTTPError(http.StatusBadRequest, "no host given")
			return
		}
		err = SetAppNetworkPolicy(ctx, ctx.Doer, owner, name, "withdraw outbound to "+value, removeOutbound(value))
	case PermKindDownload:
		err = SetAppNetworkPolicy(ctx, ctx.Doer, owner, name, "block file downloads", setDownload(false))
	default:
		ctx.HTTPError(http.StatusBadRequest, "unknown permission")
		return
	}

	if err != nil {
		// Never err.Error(): an os error carries the path it failed on, which
		// is inside Gitea's data directory (company/usererror.go).
		ctx.Flash.Error(DepartmentSafeErrorL(ctx.Locale, "revoking a permission for "+owner+"/"+name, err))
	} else {
		log.Info("company: %s/%s: %s handed back by %s", owner, name, ctx.FormString("kind"), ctx.Doer.Name)
		ctx.Flash.Success(ctx.Locale.TrString("company.flash.perm_revoked"))
	}
	ctx.Redirect(ctx.Repo.RepoLink + "/_app")
}
