// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import "gitea.dev/modules/log"

// Whether any AI request may leave this instance at all.
//
// The provider toggle in admin settings decides what departments are
// *offered*; this decides whether the feature exists. They are different
// questions with different owners. A company that forbids AI tools does not
// want a screen where an administrator could turn one on — it wants the
// request path to not be there, for everyone, including administrators
// testing what it does. Every AI call funnels through aiChatTurnStream and
// every AI screen through AIConfiguredFor, so a refusal here is complete
// rather than cosmetic.
//
// Off unless an operator switches it on. That default follows the stated
// policy rather than convenience: a stray request to api.anthropic.com from
// a network where that is forbidden is an incident, and "it was on by
// default" is not an answer anyone wants to give.
func AIEnabled() bool { return companySetting("AI_ENABLED") == "true" }

// logAIPolicyAtBoot says once, at startup, which way the switch is set. An
// operator who set it should see it took; one who did not should learn the
// feature is dark before a department asks why the button is gone.
func logAIPolicyAtBoot() {
	if AIEnabled() {
		log.Info("company: AI requests are ENABLED ([company] AI_ENABLED) — outbound calls to the configured provider are permitted")
		return
	}
	log.Info("company: AI requests are disabled ([company] AI_ENABLED is not true); no request will leave this instance for any provider")
}

// errAIDisabled is what every AI entry point returns while the switch is
// off. One sentence, one place, so a department and an administrator read
// the same reason and neither is told to "check your API key" for a feature
// that policy turned off.
var errAIDisabled = userKeyError("company.err.ai_disabled")
