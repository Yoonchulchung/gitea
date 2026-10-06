// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	system_model "gitea.dev/models/system"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	gitea_context "gitea.dev/services/context"
)

const tplAdminAI templates.TplName = "company/admin_ai"

func adminAIPage() string { return setting.AppSubURL + "/-/admin/company-ai" }

// AdminAI renders the platform's own AI and what it is allowed to decide.
func AdminAI(ctx *gitea_context.Context) {
	st, err := loadPlatformAI(ctx)
	if err != nil {
		ctx.ServerError("loadPlatformAI", err)
		return
	}
	ctx.Data["Title"] = ctx.Locale.TrString("company.platform_ai.title")
	ctx.Data["PlatformAI"] = st.PlatformAI
	ctx.Data["PlatformAIKeySet"] = st.keySet
	if headers, err := parseAIHeaders(st.headers); err == nil {
		ctx.Data["HeaderNames"] = aiHeaderNames(headers)
	}
	ctx.Data["HeadersHelp"] = "company.platform_ai.headers_help"
	ctx.Data["GatewayURL"] = aiGatewayURL()
	ctx.Data["PlatformAIUnavailable"] = st.unavailableReason()
	ctx.Data["AutoApproveDelegate"] = AutoApproveDelegate(ctx)
	ctx.Data["AutoApproveDelay"] = int(AutoApproveDelay(ctx).Seconds())
	month := ctx.FormString("month")
	if !aiUsageMonthR.MatchString(month) {
		month = time.Now().Format("2006-01")
	}
	prices := modelPrices(ctx)
	usage := summarizeAIUsage(month, loadAIUsage(month), prices)
	for _, model := range usage.Unpriced {
		ensureModelPrice(ctx, model) // reached the page unpriced: ask now, shown on the next visit
	}
	ctx.Data["Usage"] = usage
	if st.Model != "" {
		if p, ok := prices(st.Model); ok {
			ctx.Data["ModelPrice"] = p
		} else {
			ensureModelPrice(ctx, st.Model)
			ctx.Data["ModelPriceEstimating"] = true
		}
	}
	ctx.Data["KnownPricesJSON"] = knownModelPricesJSON()
	ctx.Data["KnownPricesAsOf"] = knownModelPricesAsOf
	ctx.Data["UsageMonths"] = aiUsageMonths()
	ctx.HTML(http.StatusOK, tplAdminAI)
}

// AdminAIPost saves both: the auto-approval switch is checked against the
// configuration saved in the same press, so turning both on at once works.
func AdminAIPost(ctx *gitea_context.Context) {
	if !savePlatformAIForm(ctx) {
		ctx.Redirect(adminAIPage())
		return
	}
	autoApprove := ""
	if ctx.FormBool("ai_auto_approve") {
		if reason := PlatformAIUnavailableReason(ctx); reason != "" {
			ctx.Flash.Error(ctx.Locale.TrString("company.adminsettings.auto_approve_refused", ctx.Locale.TrString(reason)))
			ctx.Redirect(adminAIPage())
			return
		}
		// Whoever turns it on is who the merge is recorded as; re-saving keeps them.
		autoApprove = strconv.FormatInt(ctx.Doer.ID, 10)
		if delegate := AutoApproveDelegate(ctx); delegate != nil {
			autoApprove = strconv.FormatInt(delegate.ID, 10)
		}
	}
	delay := strings.TrimSpace(ctx.FormString("auto_approve_delay"))
	if delay != "" {
		if v, err := strconv.Atoi(delay); err != nil || v < 0 || v > 3600 {
			ctx.Flash.Error(ctx.Locale.TrString("company.platform_ai.delay_invalid", delay))
			ctx.Redirect(adminAIPage())
			return
		}
	}
	if err := system_model.SetSettings(ctx, map[string]string{settingKeyAutoApproveBy: autoApprove, settingKeyAutoApproveDelay: delay}); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
	} else {
		invalidatePlatformSettings()
		ctx.Flash.Success(ctx.Locale.TrString("company.adminsettings.saved"))
	}
	ctx.Redirect(adminAIPage())
}

// AdminAITest saves the form, then asks the platform AI one question, so the
// answer is about what is now stored rather than what was typed.
func AdminAITest(ctx *gitea_context.Context) {
	if !savePlatformAIForm(ctx) {
		ctx.Redirect(adminAIPage())
		return
	}
	if reason := PlatformAIUnavailableReason(ctx); reason != "" {
		ctx.Flash.Error(ctx.Locale.TrString(reason))
		ctx.Redirect(adminAIPage())
		return
	}
	reply, err := platformAIChat(ctx, aiPurposeTest, ctx.Doer.Name, "", "Reply with the single word OK.", "Connection test.")
	if err != nil {
		ctx.Flash.Error(ctx.Locale.TrString("company.platform_ai.test_failed", err.Error()))
	} else {
		ctx.Flash.Success(ctx.Locale.TrString("company.platform_ai.test_ok", truncate(strings.TrimSpace(reply), 80)))
	}
	ctx.Redirect(adminAIPage())
}

// platformAIForm reads the form over what is saved: a blank key or header
// value means the saved one. The error is already a message for the page.
func platformAIForm(ctx *gitea_context.Context, st platformAIState) (cfg PlatformAI, key, headers string, err error) {
	cfg = PlatformAI{
		Provider: aiProviderOpenAI,
		BaseURL:  strings.TrimSpace(ctx.FormString("base_url")),
		Model:    strings.TrimSpace(ctx.FormString("model")),
	}
	if ctx.FormString("provider") == aiProviderAnthropic {
		cfg.Provider = aiProviderAnthropic
	}
	if cfg.BaseURL != "" {
		if u, perr := url.Parse(cfg.BaseURL); perr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return cfg, "", "", errors.New(ctx.Locale.TrString("company.platform_ai.url_invalid", cfg.BaseURL))
		}
	}
	saved, _ := parseAIHeaders(st.headers) // unreadable saved headers are replaced by what the form says
	if headers, err = formAIHeaders(ctx.Req.Form["header_name"], ctx.Req.Form["header_value"], saved); err != nil {
		return cfg, "", "", errors.New(ctx.Locale.TrString("company.settings_ai.headers_invalid", err.Error()))
	}
	if cfg.Provider == aiProviderAnthropic {
		// Its endpoint is fixed and the key is all it needs; a gateway's URL and headers must not follow it there.
		cfg.BaseURL, headers = "", ""
	}
	key = strings.TrimSpace(ctx.FormString("api_key"))
	return cfg, key, headers, nil
}

// formConfig is what a request with this form would be sent as.
func formConfig(st platformAIState, cfg PlatformAI, key, headers string) (aiUserConfig, error) {
	st.PlatformAI = cfg
	if key != "" {
		st.key = key
	}
	st.headers = headers
	return st.config()
}

// AdminAIModels lists the models the form's provider and key can use, before
// anything is saved, so the model is picked from the provider rather than typed.
func AdminAIModels(ctx *gitea_context.Context) {
	st, err := loadPlatformAI(ctx)
	if err != nil {
		ctx.ServerError("loadPlatformAI", err)
		return
	}
	cfg, key, headers, err := platformAIForm(ctx, st)
	if err != nil {
		ctx.HTTPError(http.StatusBadRequest, err.Error())
		return
	}
	uc, err := formConfig(st, cfg, key, headers)
	if err != nil {
		ctx.HTTPError(http.StatusBadRequest, err.Error())
		return
	}
	if uc.apiKey == "" {
		ctx.HTTPError(http.StatusBadRequest, ctx.Locale.TrString("company.platform_ai.unavailable.no_key"))
		return
	}
	models, err := listAIModels(ctx, uc)
	if err != nil {
		ctx.HTTPError(http.StatusBadGateway, ctx.Locale.TrString("company.platform_ai.models_failed", err.Error()))
		return
	}
	ctx.JSON(http.StatusOK, map[string]any{"models": models})
}

func savePlatformAIForm(ctx *gitea_context.Context) bool {
	st, err := loadPlatformAI(ctx)
	if err != nil {
		ctx.ServerError("loadPlatformAI", err)
		return false
	}
	cfg, key, headers, err := platformAIForm(ctx, st)
	if err != nil {
		ctx.Flash.Error(err.Error())
		return false
	}
	clearKey := ctx.FormBool("clear_key")
	// Saving never waits on the provider; the page offers only listed models, and the format is checked here.
	if cfg.Model != "" && !validModelID(cfg.Model) {
		ctx.Flash.Error(ctx.Locale.TrString("company.platform_ai.model_invalid", cfg.Model))
		return false
	}
	if err := savePlatformAI(ctx, cfg, key, clearKey, headers); err != nil {
		ctx.Flash.Error(AdminErrorL(ctx.Locale, err))
		return false
	}
	ensureModelPrice(ctx, cfg.Model) // a model the list does not know is priced before its first call is counted
	return true
}
