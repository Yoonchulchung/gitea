// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// Permission requests ride on the Deploy Request that already exists. No
// second approval queue, no second screen for an admin to remember to check
// — the code and the permissions it needs arrive together, which is also the
// only way an admin can judge them: "why does this app suddenly need to reach
// erp.internal?" is answerable next to the diff and nowhere else.
//
// Which way a change goes is decided by direction, not by kind:
//
//   - Narrowing (fewer people can reach the app) applies immediately. There
//     is no reason to make someone wait to be safer.
//   - Widening (more exposure, more resources, more packages, outbound
//     access) needs approval.
//
// And requests are filled in *for* the department. Handing a non-developer a
// blank form produces either nothing or something unusable; the platform
// already knows which packages are new and how often the memory limit was
// hit, so it proposes the items and the person supplies only the reason.
// See docs/company/app-platform.md.

// PermissionKind identifies what is being asked for.
const (
	PermKindPackage  = "package"
	PermKindNetwork  = "network"
	PermKindAccess   = "access"
	PermKindMemory   = "memory"
	PermKindDownload = "download"
)

// PermissionRequest is one item on a Deploy Request.
type PermissionRequest struct {
	Kind string `json:"kind"`
	// Value is what is being asked for, in the form the policy file uses —
	// a package name, a hostname, an access mode, a memory figure in MB.
	Value string `json:"value"`
	// Label and Detail are the same thing said for a human.
	Label  string `json:"label"`
	Detail string `json:"detail"`
	// Evidence is why the platform is proposing this, where it has grounds:
	// "the memory limit was reached 4 times last week". Without it an admin
	// is approving on assertion alone.
	Evidence string `json:"evidence,omitempty"`
	Reason   string `json:"reason,omitempty"` // supplied by the requester
	Decision string `json:"decision,omitempty"`
}

// PermissionRequestSet is what a Deploy Request carries, stored alongside
// app state so it survives a restart and is visible on both screens.
type PermissionRequestSet struct {
	PRID     int64               `json:"prID"`
	At       int64               `json:"at"`
	Requests []PermissionRequest `json:"requests"`
}

// DetectPermissionRequests works out what this deploy needs that the app is
// not currently allowed, so the request form can propose it.
//
// requirements is the content of the department's requirements.txt at the
// version being deployed; desiredAccess is what they picked on the form.
func DetectPermissionRequests(owner, repo, requirements, desiredAccess string) []PermissionRequest {
	settings := SettingsFor(owner, repo)
	st := LoadAppState(owner, repo)
	var out []PermissionRequest

	// New packages. Parse errors are not raised here — the deploy will
	// report them properly — because a malformed line is a mistake to fix,
	// not a permission to request.
	reqs, _ := ParseRequirements(requirements)
	for _, denied := range DeniedPackages(reqs, settings.AllowedPackages()) {
		out = append(out, PermissionRequest{
			Kind:   PermKindPackage,
			Value:  denied.Name,
			Label:  "패키지 추가",
			Detail: denied.Name + " (" + denied.Version + ")",
			// The department did not type this into a form; it is what their
			// own requirements.txt asks for. Saying so tells the admin the
			// request is real rather than speculative.
			Evidence: "requirements.txt 에 있으나 아직 승인되지 않았습니다",
		})
	}

	// Access, only when it widens. Narrowing is applied without asking.
	if desiredAccess != "" && desiredAccess != settings.Access &&
		accessRank[desiredAccess] > accessRank[settings.Access] {
		out = append(out, PermissionRequest{
			Kind:   PermKindAccess,
			Value:  desiredAccess,
			Label:  "접근 권한 변경",
			Detail: accessLabel(settings.Access) + " → " + accessLabel(desiredAccess),
		})
	}

	// A memory increase is proposed only with evidence. Asking for more
	// because someone feels like it is exactly what this design is trying to
	// avoid; asking because the app hit the ceiling four times is a fact an
	// admin can act on.
	if hits := memoryLimitHits(st); hits > 0 {
		doubled := settings.Limits.MemoryMB * 2
		out = append(out, PermissionRequest{
			Kind:     PermKindMemory,
			Value:    strconv.Itoa(doubled),
			Label:    "메모리 한도 상향",
			Detail:   strconv.Itoa(settings.Limits.MemoryMB) + "MB → " + strconv.Itoa(doubled) + "MB",
			Evidence: "최근 한도에 " + strconv.Itoa(hits) + "회 도달했습니다",
		})
	}
	return out
}

// memoryLimitHits counts recorded memory stops in the app's history.
func memoryLimitHits(st *AppState) int {
	n := 0
	for _, h := range st.History {
		if h.Reason == ReasonOOM {
			n++
		}
	}
	return n
}

// permissionFile is where a Deploy Request's items live until it is decided.
//
// Its own directory, not alongside the state files: ListAppStates lists
// every *.json in the state directory, so a permissions file sharing it
// would be read as an app with no owner and show up on the admin dashboard
// as a phantom row.
func permissionFile(owner, repo string) string {
	return filepath.Join(setting.AppDataPath, "company-permissions", appKey(owner, repo)+".json")
}

// SavePermissionRequests records what a Deploy Request is asking for.
func SavePermissionRequests(owner, repo string, prID int64, requests []PermissionRequest) error {
	set := PermissionRequestSet{PRID: prID, At: time.Now().Unix(), Requests: requests}
	body, err := json.Marshal(set)
	if err != nil {
		return err
	}
	return writeFileAtomic(permissionFile(owner, repo), body)
}

// LoadPermissionRequests returns the items attached to a Deploy Request, or
// nil if this PR has none. Keyed by PR id so a stale file from a previous,
// abandoned request is never shown against a new one.
func LoadPermissionRequests(owner, repo string, prID int64) []PermissionRequest {
	body, err := readFileIfExists(permissionFile(owner, repo))
	if err != nil || body == nil {
		return nil
	}
	var set PermissionRequestSet
	if err := json.Unmarshal(body, &set); err != nil {
		log.Error("company: permission requests for %s/%s are unreadable: %v", owner, repo, err)
		return nil
	}
	if set.PRID != prID {
		return nil
	}
	return set.Requests
}

// ApplyApprovedPermissions folds approved items into an apps.yml settings
// block for one app.
//
// It returns the updated settings rather than writing anything: the caller
// commits them to the central deploy repository, so the approval lands as a
// git commit whose author is the approving admin. That commit *is* the audit
// record — "who approved outbound access to erp.internal, and when" is a
// question git answers on its own, and a log file the Gitea account can
// rewrite does not. See docs/company/app-platform.md.
func ApplyApprovedPermissions(current AppSettings, requests []PermissionRequest) AppSettings {
	updated := current
	for _, r := range requests {
		if r.Decision != "approve" {
			continue
		}
		switch r.Kind {
		case PermKindPackage:
			name := normalizePackageName(r.Value)
			// allowExtra rather than allow: approving pandas for one
			// department must not quietly open it for every other app.
			if !slices.ContainsFunc(updated.Dependencies.AllowExtra,
				func(p string) bool { return normalizePackageName(p) == name }) {
				updated.Dependencies.AllowExtra = append(updated.Dependencies.AllowExtra, r.Value)
			}
		case PermKindAccess:
			updated.Access = r.Value
		case PermKindMemory:
			if mb, err := strconv.Atoi(r.Value); err == nil && mb > 0 {
				updated.Limits.MemoryMB = mb
			}
		case PermKindDownload:
			updated.Download.Policy = "allow"
		case PermKindNetwork:
			if updated.Network.Mode == NetworkNone {
				updated.Network.Mode = NetworkBroker
			}
			host, methods, _ := strings.Cut(r.Value, " ")
			rule := AppNetworkRule{Host: host}
			if methods != "" {
				rule.Methods = strings.Split(methods, ",")
			}
			updated.Network.Allow = append(updated.Network.Allow, rule)
		}
	}
	return updated
}
