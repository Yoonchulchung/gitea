// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	gitea_context "gitea.dev/services/context"
)

const tplSettingsAI templates.TplName = "company/settings_ai"

// AISettings renders /user/settings/ai — each person's own model/API
// key/reasoning effort for the internal AI tool (company/ai.go). The
// gateway URL itself is instance-wide (app.ini, admin-only); this page is
// only ever the per-user half. Mounted inside Gitea's native
// /user/settings route group (routers/web/web.go) so it inherits
// reqSignIn + user_setting.SettingsCtxData (the PageIsSettingsXXX/
// EnableXXX ctx.Data every settings template, including our own navbar
// override, already depends on) for free.
func AISettings(ctx *gitea_context.Context) {
	provider, err := user_model.GetUserSetting(ctx, ctx.Doer.ID, userSettingAIProvider, aiProviderOpenAI)
	if err != nil {
		ctx.ServerError("GetUserSetting", err)
		return
	}
	modelID, err := user_model.GetUserSetting(ctx, ctx.Doer.ID, userSettingAIModelID)
	if err != nil {
		ctx.ServerError("GetUserSetting", err)
		return
	}
	reasoningEffort, err := user_model.GetUserSetting(ctx, ctx.Doer.ID, userSettingAIReasoningEffort)
	if err != nil {
		ctx.ServerError("GetUserSetting", err)
		return
	}
	// Read raw, not via getUserSecret: this page only needs "is a key
	// stored?", and the stored value being non-empty answers that whether
	// it is encrypted or legacy plaintext. Decrypting here would be work
	// for a value we deliberately never show, and would drag the plaintext
	// migration into every settings page load.
	apiKeyStored, err := user_model.GetUserSetting(ctx, ctx.Doer.ID, userSettingAIAPIKey)
	if err != nil {
		ctx.ServerError("GetUserSetting", err)
		return
	}

	ctx.Data["Title"] = string(ctx.Locale.Tr("company.settings_ai.nav_title"))
	ctx.Data["PageIsSettingsAI"] = true
	ctx.Data["Provider"] = provider
	ctx.Data["ModelID"] = modelID
	ctx.Data["ReasoningEffort"] = reasoningEffort
	ctx.Data["APIKeySet"] = apiKeyStored != "" // never echo the key itself back into the form
	ctx.Data["GatewayConfigured"] = aiGatewayURL() != ""
	ctx.HTML(http.StatusOK, tplSettingsAI)
}

// AISettingsPost saves the form above. The API key field is optional on
// submit — left blank, the previously saved key (if any) is kept, so
// re-saving the model/reasoning effort doesn't force re-entering it every
// time. A literal blank-out needs its own explicit "지우기" action instead
// of overloading an empty submit for that, to avoid silently locking
// someone out of AI features from a copy-paste mistake.
func AISettingsPost(ctx *gitea_context.Context) {
	provider := strings.TrimSpace(ctx.Req.FormValue("provider"))
	if provider != aiProviderAnthropic {
		provider = aiProviderOpenAI
	}
	modelID := strings.TrimSpace(ctx.Req.FormValue("model_id"))
	reasoningEffort := strings.TrimSpace(ctx.Req.FormValue("reasoning_effort"))
	apiKey := strings.TrimSpace(ctx.Req.FormValue("api_key"))

	if err := user_model.SetUserSetting(ctx, ctx.Doer.ID, userSettingAIProvider, provider); err != nil {
		ctx.ServerError("SetUserSetting", err)
		return
	}
	if err := user_model.SetUserSetting(ctx, ctx.Doer.ID, userSettingAIModelID, modelID); err != nil {
		ctx.ServerError("SetUserSetting", err)
		return
	}
	if err := user_model.SetUserSetting(ctx, ctx.Doer.ID, userSettingAIReasoningEffort, reasoningEffort); err != nil {
		ctx.ServerError("SetUserSetting", err)
		return
	}
	if apiKey != "" {
		if err := setUserSecret(ctx, ctx.Doer.ID, userSettingAIAPIKey, apiKey); err != nil {
			ctx.ServerError("SetUserSetting", err)
			return
		}
	}

	ctx.Flash.Success(ctx.Tr("settings.update_setting_success"))
	ctx.Redirect(setting.AppSubURL + "/user/settings/ai")
}

type aiListModelsResponse struct {
	Models []string `json:"models"`
}

// AIListModels backs the "모델 조회" button on the settings form above —
// OpenAI-compatible only. Unlike Anthropic (a small, well-known, mostly
// static family of model names the frontend just lists directly, no
// request needed), an internal OpenAI-compatible gateway's available
// models are deployment-specific, so there's nothing to suggest without
// actually asking it. Accepts an api_key in the POST body so someone can
// look up models with a key they've typed but not saved yet; falls back
// to their already-saved key if the field was left blank (e.g. re-checking
// after the gateway added a model, without retyping the key).
func AIListModels(ctx *gitea_context.Context) {
	url := aiGatewayURL()
	if url == "" {
		ctx.HTTPError(http.StatusServiceUnavailable, "AI gateway not configured")
		return
	}

	apiKey := strings.TrimSpace(ctx.Req.FormValue("api_key"))
	if apiKey == "" {
		saved, err := getUserSecret(ctx, ctx.Doer.ID, userSettingAIAPIKey)
		if err != nil {
			ctx.ServerError("GetUserSetting", err)
			return
		}
		apiKey = saved
	}
	if apiKey == "" {
		ctx.HTTPError(http.StatusBadRequest, "enter an API key first")
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url+"/models", nil)
	if err != nil {
		ctx.ServerError("NewRequestWithContext", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ctx.HTTPError(http.StatusBadGateway, fmt.Sprintf("could not reach the AI gateway: %v", err))
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		ctx.ServerError("ReadAll", err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		ctx.HTTPError(http.StatusBadGateway, fmt.Sprintf("gateway returned status %d", resp.StatusCode))
		return
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		ctx.HTTPError(http.StatusBadGateway, "gateway's /models response wasn't the expected shape")
		return
	}

	models := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	sort.Strings(models)
	ctx.JSON(http.StatusOK, &aiListModelsResponse{Models: models})
}
