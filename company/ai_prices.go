// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	system_model "gitea.dev/models/system"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
)

// What a model costs, so token usage can be shown as money without anybody
// looking prices up. Anthropic's first-party list prices for the models this
// table knows; any other model — an internal gateway's, or one released since —
// is priced by the platform AI itself the first time it is used. A model being
// called means the AI is reachable, so the price can always be asked for.

// knownModelPricesAsOf is when the table below was taken from Anthropic's list.
const knownModelPricesAsOf = "2026-09"

var knownModelPrices = map[string][2]float64{
	"claude-fable-5-1":  {10, 50},
	"claude-mythos-5-1": {10, 50},
	"claude-fable-5":    {10, 50},
	"claude-opus-5-5":   {4, 20},
	"claude-opus-5":     {5, 25},
	"claude-opus-4-8":   {5, 25},
	"claude-opus-4-7":   {5, 25},
	"claude-opus-4-6":   {5, 25},
	"claude-sonnet-5-5": {2, 10},
	"claude-sonnet-5":   {2, 10},
	"claude-sonnet-4-6": {3, 15},
	"claude-haiku-4-5":  {1, 5},
}

// knownModelPrice finds a model's list price; a dated or suffixed ID
// ("claude-haiku-4-5-20251001") matches the longest known ID it starts with.
func knownModelPrice(model string) (in, out float64, ok bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	best := ""
	for id := range knownModelPrices {
		if (model == id || strings.HasPrefix(model, id+"-")) && len(id) > len(best) {
			best = id
		}
	}
	if best == "" {
		return 0, 0, false
	}
	p := knownModelPrices[best]
	return p[0], p[1], true
}

// ModelPrice is a model's price per million tokens and where it came from.
type ModelPrice struct {
	In     float64 `json:"in"`
	Out    float64 `json:"out"`
	Source string  `json:"source"` // "known" (the list above) or "ai" (estimated)
	Note   string  `json:"note,omitempty"`
	At     int64   `json:"at,omitempty"`
}

const settingKeyAIPrices = "company.platform_ai.prices" // model → ModelPrice, the AI's estimates

func loadEstimatedPrices(ctx context.Context) map[string]ModelPrice {
	prices := map[string]ModelPrice{}
	_, all, err := system_model.GetAllSettings(ctx)
	if err != nil {
		return prices
	}
	if raw := all[settingKeyAIPrices]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &prices) // unreadable: estimated again on next use
	}
	return prices
}

// modelPrices is the lookup a report prices its calls with: the list, then the estimates.
func modelPrices(ctx context.Context) func(model string) (ModelPrice, bool) {
	estimated := loadEstimatedPrices(ctx)
	return func(model string) (ModelPrice, bool) {
		if in, out, ok := knownModelPrice(model); ok {
			return ModelPrice{In: in, Out: out, Source: "known"}, true
		}
		p, ok := estimated[strings.ToLower(model)]
		return p, ok
	}
}

var priceEstimates sync.Map // model → struct{}: an estimate in flight

// ensureModelPrice has the platform AI price a model the list does not know,
// in the background and once: the call that needed it is not held up, and the
// report picks the price up the next time it is drawn.
func ensureModelPrice(ctx context.Context, model string) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || !platformRunning.Load() {
		return
	}
	if _, _, ok := knownModelPrice(model); ok {
		return
	}
	if _, ok := loadEstimatedPrices(ctx)[model]; ok {
		return
	}
	if _, running := priceEstimates.LoadOrStore(model, struct{}{}); running {
		return
	}
	go func() {
		defer priceEstimates.Delete(model)
		defer recoverBackground("estimating the price of %s", model)
		bg := graceful.GetManager().ShutdownContext()
		e, err := estimateModelPrice(bg, model)
		if err != nil {
			log.Warn("company: the platform AI could not price %s: %v", model, err)
			return
		}
		prices := loadEstimatedPrices(bg)
		prices[model] = ModelPrice{In: e.Input, Out: e.Output, Source: "ai", Note: e.Note, At: time.Now().Unix()}
		raw, _ := json.Marshal(prices)
		if err := system_model.SetSettings(bg, map[string]string{settingKeyAIPrices: string(raw)}); err != nil {
			log.Error("company: storing the price of %s: %v", model, err)
			return
		}
		log.Info("company: the platform AI priced %s at $%v in / $%v out per 1M tokens", model, e.Input, e.Output)
	}()
}

// knownModelPricesJSON is the list for the page's script, which updates the
// price line as soon as a model is picked.
func knownModelPricesJSON() string {
	ids := make([]string, 0, len(knownModelPrices))
	for id := range knownModelPrices {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := map[string][2]float64{}
	for _, id := range ids {
		out[id] = knownModelPrices[id]
	}
	b, _ := json.Marshal(out)
	return string(b)
}

const priceEstimatePrompt = `You estimate what a language model costs to call through its provider's API, for a cost dashboard. Answer from what you know; you cannot look anything up.

Reply with only this JSON object:
{"input": USD per 1M input tokens, "output": USD per 1M output tokens, "confidence": "high" or "low", "note": "one short sentence in Korean: what the estimate is based on"}
Use "low" when you do not recognise the model or it may be newer than what you know; still give your best estimate from similar models. Use 0 for both prices only when the model is clearly self-hosted and has no per-token price.`

type priceEstimate struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	Confidence string  `json:"confidence"`
	Note       string  `json:"note"`
}

func parsePriceEstimate(reply string) (priceEstimate, bool) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return priceEstimate{}, false
	}
	var e priceEstimate
	if err := json.Unmarshal([]byte(reply[start:end+1]), &e); err != nil || e.Input < 0 || e.Output < 0 || e.Input > 10000 || e.Output > 10000 {
		return priceEstimate{}, false
	}
	e.Note = oneLine(e.Note)
	return e, true
}

func estimateModelPrice(ctx context.Context, model string) (priceEstimate, error) {
	st, err := loadPlatformAI(ctx)
	if err != nil {
		return priceEstimate{}, err
	}
	provider := "an OpenAI-compatible gateway"
	if st.Provider == aiProviderAnthropic {
		provider = "Anthropic"
	}
	reply, err := platformAIChat(ctx, aiPurposePriceEstimate, "", model, priceEstimatePrompt, fmt.Sprintf("Provider: %s\nModel ID: %s", provider, model))
	if err != nil {
		return priceEstimate{}, err
	}
	e, ok := parsePriceEstimate(reply)
	if !ok {
		return priceEstimate{}, errors.New("the estimate was not in the asked-for form")
	}
	return e, nil
}
