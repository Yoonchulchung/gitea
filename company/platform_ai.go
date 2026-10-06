// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	system_model "gitea.dev/models/system"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	secret_module "gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
)

// The platform's own AI: what the platform asks when it decides something
// itself (company/deploy_auto_approve.go). Never a person's key — that is
// theirs to revoke, and a decision the platform makes should not stop when
// somebody rotates their own credentials or leaves.
const (
	settingKeyPlatformAI        = "company.platform_ai"
	settingKeyPlatformAIKey     = "company.platform_ai.key"     // encrypted
	settingKeyPlatformAIHeaders = "company.platform_ai.headers" // encrypted, "Name: value" per line
)

// PlatformAI is the part of the configuration a page may show.
type PlatformAI struct {
	Provider string `json:"provider"`
	// BaseURL is the OpenAI-compatible endpoint; blank uses [company] AI_API_URL.
	BaseURL string `json:"baseURL"`
	Model   string `json:"model"`
}

type platformAIState struct {
	PlatformAI
	keySet  bool
	key     string
	headers string
}

// loadPlatformAI reads it fresh: it is used when a request is decided and on
// its own settings page, never on a hot path.
func loadPlatformAI(ctx context.Context) (platformAIState, error) {
	_, all, err := system_model.GetAllSettings(ctx)
	if err != nil {
		return platformAIState{}, err
	}
	st := platformAIState{PlatformAI: PlatformAI{Provider: aiProviderOpenAI}}
	if raw := all[settingKeyPlatformAI]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &st.PlatformAI); err != nil {
			return platformAIState{}, err
		}
	}
	decrypt := func(key string) string {
		stored := all[key]
		if stored == "" {
			return ""
		}
		plain, err := secret_module.DecryptSecret(setting.SecretKey, stored)
		if err != nil {
			log.Error("company: decrypting %s: %v", key, err) // SECRET_KEY changed; treated as unset
			return ""
		}
		return plain
	}
	st.key = decrypt(settingKeyPlatformAIKey)
	st.keySet = st.key != ""
	st.headers = decrypt(settingKeyPlatformAIHeaders)
	return st, nil
}

// savePlatformAI stores it; a blank key keeps the saved one unless clearKey.
func savePlatformAI(ctx context.Context, cfg PlatformAI, key string, clearKey bool, headers string) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	values := map[string]string{settingKeyPlatformAI: string(raw)}
	encrypt := func(plain string) (string, error) {
		if plain == "" {
			return "", nil
		}
		return secret_module.EncryptSecret(setting.SecretKey, plain)
	}
	if key != "" || clearKey {
		if values[settingKeyPlatformAIKey], err = encrypt(key); err != nil {
			return err
		}
	}
	if values[settingKeyPlatformAIHeaders], err = encrypt(headers); err != nil {
		return err
	}
	return system_model.SetSettings(ctx, values)
}

func (st platformAIState) config() (aiUserConfig, error) {
	headers, err := parseAIHeaders(st.headers)
	if err != nil {
		return aiUserConfig{}, err
	}
	return aiUserConfig{provider: st.Provider, modelID: st.Model, apiKey: st.key, headers: headers, baseURL: st.BaseURL}, nil
}

// unavailableReason is the locale key of what is missing, or "" when it works.
func (st platformAIState) unavailableReason() string {
	switch {
	case !AIEnabled():
		return "company.platform_ai.unavailable.disabled"
	case !st.keySet:
		return "company.platform_ai.unavailable.no_key"
	case strings.TrimSpace(st.Model) == "":
		return "company.platform_ai.unavailable.no_model"
	case st.Provider != aiProviderAnthropic && st.BaseURL == "" && aiGatewayURL() == "":
		return "company.platform_ai.unavailable.no_url"
	}
	return ""
}

// PlatformAIUnavailableReason is unavailableReason for whoever only needs the answer.
func PlatformAIUnavailableReason(ctx context.Context) string {
	st, err := loadPlatformAI(ctx)
	if err != nil {
		log.Error("company: reading the platform AI settings: %v", err)
		return "company.platform_ai.unavailable.no_key"
	}
	return st.unavailableReason()
}

var errPlatformAIUnavailable = errors.New("the platform AI is not set up")

// PlatformAIModel is the model the platform AI uses, for display.
func PlatformAIModel(ctx context.Context) string {
	st, err := loadPlatformAI(ctx)
	if err != nil {
		return ""
	}
	return st.Model
}

// platformAIChat is aiChat on the platform's own configuration; the call is
// recorded under purpose (company/ai_usage.go).
func platformAIChat(ctx context.Context, purpose, actor, ref, systemPrompt, userPrompt string) (string, error) {
	st, err := loadPlatformAI(ctx)
	if err != nil {
		return "", err
	}
	if st.unavailableReason() != "" {
		return "", errPlatformAIUnavailable
	}
	uc, err := st.config()
	if err != nil {
		return "", err
	}
	resp, err := aiChatTurnStreamWith(ctx, uc, []aiChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, nil, nil)
	if err != nil {
		return "", err
	}
	recordAIUsage(ctx, purpose, uc.modelID, actor, ref, resp.Usage)
	return resp.Content, nil
}

// listAIModels asks the provider which models this configuration may use.
func listAIModels(ctx context.Context, uc aiUserConfig) ([]string, error) {
	if !AIEnabled() {
		return nil, errAIDisabled
	}
	endpoint := anthropicAPIBase + "/v1/models?limit=1000"
	if uc.provider != aiProviderAnthropic {
		if uc.gatewayURL() == "" {
			return nil, errors.New("no API URL")
		}
		endpoint = uc.gatewayURL() + "/models"
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if uc.provider == aiProviderAnthropic {
		req.Header.Set("x-api-key", uc.apiKey)
		req.Header.Set("anthropic-version", anthropicVersion)
	} else {
		req.Header.Set("Authorization", "Bearer "+uc.apiKey)
	}
	applyAIHeaders(req, uc.headers)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, errors.New("the model list was not in the expected shape")
	}
	models := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if validModelID(m.ID) {
			models = append(models, m.ID)
		}
	}
	slices.Sort(models)
	return slices.Compact(models), nil
}
