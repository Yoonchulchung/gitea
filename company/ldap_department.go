// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"

	"gitea.dev/models/db"
	org_model "gitea.dev/models/organization"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/services/auth/source/ldap"
	org_service "gitea.dev/services/org"
)

// The directory already knows which department a person works in. Reading it
// at every LDAP login puts them in that department — created on first sight —
// so nobody waits on an administrator to be let in, and a move in the
// directory is a move here at the next login.

// settingKeyLDAPDepartmentAttr is the LDAP attribute holding the department
// name; empty leaves membership to administrators.
const settingKeyLDAPDepartmentAttr = "company.ldap.department_attribute"

// userSettingLDAPDepartment remembers which department the directory put
// someone in, so a move takes them out of that one and of no other.
const userSettingLDAPDepartment = "company.ldap_department_org"

func init() {
	ldap.ExtraAttributes = func() []string {
		if attr := LDAPDepartmentAttribute(context.Background()); attr != "" {
			return []string{attr}
		}
		return nil
	}
	ldap.AfterAuthenticate = func(ctx context.Context, u *user_model.User, sr *ldap.SearchResult) {
		attr := LDAPDepartmentAttribute(ctx)
		if attr == "" || u == nil {
			return
		}
		// A failure here must not refuse a login the directory accepted.
		if err := syncLDAPDepartment(ctx, u, sr.Extra[strings.ToLower(attr)]); err != nil {
			log.Error("company: placing %s in their directory department: %v", u.Name, err)
		}
	}
}

// LDAPDepartmentAttribute is the attribute an administrator named, or "".
func LDAPDepartmentAttribute(ctx context.Context) string {
	loadPlatformSettings(ctx)
	platformSettingsMu.RLock()
	defer platformSettingsMu.RUnlock()
	return ldapDepartmentAttr
}

func syncLDAPDepartment(ctx context.Context, u *user_model.User, department string) error {
	department = strings.TrimSpace(department)
	if department == "" {
		log.Warn("company: %s logged in with no department in the directory; left for an administrator", u.Name)
		return nil
	}
	org, err := departmentOrgFor(ctx, department)
	if err != nil {
		return err
	}

	isMember, err := org_model.IsOrganizationMember(ctx, org.ID, u.ID)
	if err != nil {
		return err
	}
	if !isMember {
		if err := org_model.AddOrgUser(ctx, org.ID, u.ID); err != nil {
			return err
		}
		log.Info("company: %s joined %s (%q in the directory)", u.Name, org.Name, department)
	}

	prev, err := user_model.GetUserSetting(ctx, u.ID, userSettingLDAPDepartment)
	if err != nil {
		return err
	}
	if prevID, _ := strconv.ParseInt(prev, 10, 64); prevID != 0 && prevID != org.ID {
		leaveDirectoryDepartment(ctx, u, prevID)
	}
	return user_model.SetUserSetting(ctx, u.ID, userSettingLDAPDepartment, strconv.FormatInt(org.ID, 10))
}

// leaveDirectoryDepartment undoes an earlier placement after a move. An
// owner stays: ownership is something an administrator gave, not the
// directory.
func leaveDirectoryDepartment(ctx context.Context, u *user_model.User, orgID int64) {
	org, err := org_model.GetOrgByID(ctx, orgID)
	if err != nil {
		return // deleted since
	}
	if isOwner, err := org_model.IsOrganizationOwner(ctx, orgID, u.ID); err != nil || isOwner {
		return
	}
	if err := org_service.RemoveOrgUser(ctx, org, u); err != nil {
		log.Error("company: taking %s out of %s after a directory move: %v", u.Name, org.Name, err)
		return
	}
	log.Info("company: %s left %s, which the directory no longer names", u.Name, org.Name)
}

var departmentCreateMu sync.Mutex

// departmentOrgFor finds the organization for a directory department, by its
// name or its display name, and creates it when there is none. Department
// names are usually Korean, which an organization name cannot be, so a
// created one is named after a hash and carries the department as its
// display name — which is also how an administrator maps an existing one.
func departmentOrgFor(ctx context.Context, department string) (*org_model.Organization, error) {
	departmentCreateMu.Lock()
	defer departmentCreateMu.Unlock()

	if user_model.IsUsableUsername(department) == nil {
		if org, err := org_model.GetOrgByName(ctx, department); err == nil {
			return org, nil
		}
	}
	var found user_model.User
	has, err := db.GetEngine(ctx).
		Where("type = ? AND full_name = ?", user_model.UserTypeOrganization, department).
		Asc("id").Get(&found)
	if err != nil {
		return nil, err
	}
	if has {
		return org_model.OrgFromUser(&found), nil
	}

	owner, err := user_model.GetAdminUser(ctx)
	if err != nil {
		return nil, err
	}
	org := &org_model.Organization{
		Name:       departmentOrgName(ctx, department),
		FullName:   department,
		Visibility: setting.Service.DefaultOrgVisibilityMode,
	}
	if err := org_model.CreateOrganization(ctx, org, owner); err != nil {
		return nil, err
	}
	log.Info("company: created department %s for %q from the directory (owner: %s)", org.Name, department, owner.Name)
	return org, nil
}

func departmentOrgName(ctx context.Context, department string) string {
	if user_model.IsUsableUsername(department) == nil {
		if exists, err := user_model.IsUserExist(ctx, 0, department); err == nil && !exists {
			return department
		}
	}
	sum := sha256.Sum256([]byte(department))
	return "dept-" + hex.EncodeToString(sum[:4])
}
