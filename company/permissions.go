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
	// Methods applies to an outbound request: which HTTP methods the host may
	// be reached with. Its own field because Value is the host and the two
	// were previously squeezed into one string, where the methods were lost.
	Methods []string `json:"methods,omitempty"`
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
			Label:  "company.perm.kind.package",
			Detail: denied.Name + " (" + denied.Version + ")",
			// The department did not type this into a form; it is what their
			// own requirements.txt asks for. Saying so tells the admin the
			// request is real rather than speculative.
			Evidence: "requirements.txt 에 있으나 아직 승인되지 않았습니다",
		})
	}

	// What the last build resolved but could not install: transitive
	// dependencies of packages that *are* approved. They appear in no file
	// anyone wrote, so without this the department has nothing to tick and
	// the build stays blocked on a package they cannot ask for.
	for _, name := range st.MissingPackages {
		if slices.ContainsFunc(out, func(r PermissionRequest) bool {
			return r.Kind == PermKindPackage && normalizePackageName(r.Value) == normalizePackageName(name)
		}) {
			continue
		}
		out = append(out, PermissionRequest{
			Kind:     PermKindPackage,
			Value:    name,
			Label:    "company.perm.kind.package",
			Detail:   name,
			Evidence: "직전 배포가 이 패키지에서 멈췄습니다 — 승인한 패키지가 필요로 하는 의존성입니다",
		})
	}

	// Access, only when it widens. Narrowing is applied without asking.
	if desiredAccess != "" && desiredAccess != settings.Access &&
		accessRank[desiredAccess] > accessRank[settings.Access] {
		out = append(out, PermissionRequest{
			Kind:  PermKindAccess,
			Value: desiredAccess,
			Label: "company.perm.kind.access",
			// Mode codes, not translated names: this string is stored in the
			// request record, and a record written in whatever language the
			// form happened to render in is a record someone else cannot read.
			Detail: orDefault(settings.Access, AccessPublic) + " → " + desiredAccess,
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
			Label:    "company.perm.kind.memory",
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

// LoadPermissionRequestSet returns whatever request is on file for this app,
// whichever PR it belongs to. Used by the app screen, which is asking "is
// something of ours waiting on an admin?" rather than about one PR.
func LoadPermissionRequestSet(owner, repo string) *PermissionRequestSet {
	body, err := readFileIfExists(permissionFile(owner, repo))
	if err != nil || body == nil {
		return nil
	}
	var set PermissionRequestSet
	if err := json.Unmarshal(body, &set); err != nil {
		log.Error("company: permission requests for %s/%s are unreadable: %v", owner, repo, err)
		return nil
	}
	return &set
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
			// Through the same helper an admin's direct change uses, so an
			// approved host and a hand-added one cannot end up meaning
			// different things. It also replaces a host already on the list
			// rather than appending a second, contradicting rule for it.
			host, methods := outboundFromRequest(r)
			addOutbound(host, methods)(&updated)
		}
	}
	return updated
}

// outboundFromRequest reads the host and methods out of a network request.
//
// The space-separated form is what requests written before Methods existed
// carry, and one may be sitting in a file waiting for an admin right now —
// so it is still understood rather than silently dropping the methods it
// encodes.
func outboundFromRequest(r PermissionRequest) (host string, methods []string) {
	host, legacy, _ := strings.Cut(r.Value, " ")
	switch {
	case len(r.Methods) > 0:
		methods = r.Methods
	case legacy != "":
		methods = strings.Split(legacy, ",")
	default:
		// Least privilege: an approval that says nothing about methods grants
		// reading, not writing.
		methods = []string{"GET"}
	}
	return host, methods
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
