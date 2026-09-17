// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"

	"gitea.dev/modules/log"
)

// What a deployed app may accept, and how far it may be exposed.
//
// The outbound side asks "what may an app reach". This side asks the
// question a platform that lets people deploy code has to answer too: what
// may an app *be* — because an app with the network is one `uvicorn
// --host 0.0.0.0` away from being somebody's own server on the company
// network, reachable by anyone who can route to the host, answering to
// nobody's access policy.
//
// Two instance-wide ceilings, both app.ini [company] settings, both applied
// at three points: when an administrator changes a setting, when apps.yml is
// loaded from git, and — because a setting committed before the ceiling was
// lowered is still in the file — at request time, where the ceiling simply
// wins.

const (
	// accessRank orders the modes so "wider than" has a meaning.
	accessRankOrg = iota
	accessRankLogin
	accessRankPublic
)

var accessRanks = map[string]int{AccessOrg: accessRankOrg, AccessLogin: accessRankLogin, AccessPublic: accessRankPublic}

// NetworkOpenAllowed reports whether any app may run with unrestricted
// network access. Off unless an operator turns it on: `open` is the one mode
// in which an app can bind a port and become a server, and that is the thing
// this file exists to forbid by default.
func NetworkOpenAllowed() bool { return companySetting("APP_NETWORK_ALLOW_OPEN") == "true" }

// MaxAppAccess is the widest exposure any app may have. Defaults to public,
// which is what every app had before this existed — lowering it is the
// operator's decision, and the page says what it is.
func MaxAppAccess() string {
	switch v := companySetting("APP_MAX_ACCESS"); v {
	case AccessOrg, AccessLogin, AccessPublic:
		return v
	case "":
		return AccessPublic
	default:
		log.Warn("company: APP_MAX_ACCESS=%q is not org, login or public; treating as public", v)
		return AccessPublic
	}
}

// clampAccess returns the narrower of the app's own setting and the ceiling.
// Applied at request time, so a mode committed before the ceiling was
// lowered does not stay in force just because it is still written down.
func clampAccess(access string) string {
	if access == "" {
		access = AccessPublic
	}
	if accessRanks[access] > accessRanks[MaxAppAccess()] {
		return MaxAppAccess()
	}
	return access
}

// accessWiderThanCeiling is the check at setting time — a refusal with a
// reason, rather than a silent clamp, because the person changing the
// setting is looking at the screen and should be told.
func accessWiderThanCeiling(access string) bool {
	return accessRanks[access] > accessRanks[MaxAppAccess()]
}

// InboundPolicy is the summary the network page leads with.
type InboundPolicy struct {
	OpenAllowed bool
	MaxAccess   string
	// ListenerWatch is whether this host can see what an app is listening
	// on. It can only be trusted where the app has its own network
	// namespace; on a host without that the whole machine's sockets show
	// through and the watchdog stays quiet rather than crying wolf.
	ListenerWatch bool
}

func CurrentInboundPolicy() InboundPolicy {
	sandboxed, _ := SandboxStatus()
	return InboundPolicy{
		OpenAllowed:   NetworkOpenAllowed(),
		MaxAccess:     MaxAppAccess(),
		ListenerWatch: sandboxed && listenerWatchSupported(),
	}
}

// checkListeners is the watchdog: an app that opened a port of its own is
// stopped and told why.
//
// Only where the answer can be trusted. Under a network namespace the app's
// socket table holds nothing but what the app opened; without one it is the
// host's whole table, and stopping every app because the host runs sshd would
// be wrong in a way that discredits the control. So the page says whether
// this is watching, and it stays silent where it cannot be sure.
func checkListeners(owner, repo string, pid int) {
	if !CurrentInboundPolicy().ListenerWatch {
		return
	}
	ports := listeningPorts(pid)
	if len(ports) == 0 {
		return
	}
	log.Warn("company: %s/%s is listening on TCP %v inside its sandbox; stopping it", owner, repo, ports)
	if err := supervisorFor(owner, repo).Stop("platform", AppStateFailed, ReasonRogueListener); err != nil {
		log.Error("company: stopping %s/%s after it opened a port: %v", owner, repo, err)
	}
	_ = MutateAppState(owner, repo, func(st *AppState) bool {
		st.Reason = ReasonRogueListener
		st.Message = fmt.Sprintf("the app was stopped: it opened a listening port (%v). Apps are reached only through the platform proxy", ports)
		st.UserMessage = st.Message
		return true
	})
}
