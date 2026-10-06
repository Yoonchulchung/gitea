// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Switching from Fable to Haiku must not reprice what Fable already spent:
// each call costs its own model's price, and the report says which AI it was.
func TestSummarizeAIUsageByModel(t *testing.T) {
	day1 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.Local).Unix()
	day2 := time.Date(2026, 10, 6, 10, 0, 0, 0, time.Local).Unix()
	stored := 0.5
	entries := []aiUsageEntry{
		{At: day1, Purpose: aiPurposeDeployReview, Model: "claude-fable-5", Input: 1_000_000, Output: 100_000},   // $10 + $5
		{At: day2, Purpose: aiPurposeDeployReview, Model: "claude-haiku-4-5", Input: 1_000_000, Output: 100_000}, // $1 + $0.5
		{At: day2, Purpose: aiPurposeTest, Model: "claude-fable-5", Input: 10, Output: 1, Cost: &stored},         // as recorded
		{At: day2, Purpose: aiPurposeTest, Model: "gateway-llm", Input: 1_000_000, Output: 0},                    // priced by the AI's estimate
		{At: day2, Purpose: aiPurposeTest, Model: "not-yet-priced", Input: 5, Output: 5},
	}
	estimated := map[string]ModelPrice{"gateway-llm": {In: 2, Out: 6, Source: "ai"}}
	price := func(model string) (ModelPrice, bool) {
		if in, out, ok := knownModelPrice(model); ok {
			return ModelPrice{In: in, Out: out, Source: "known"}, true
		}
		p, ok := estimated[model]
		return p, ok
	}
	r := summarizeAIUsage("2026-10", entries, price)
	assert.True(t, r.Priced)
	assert.InDelta(t, 15+1.5+0.5+2, r.Total.Cost, 1e-9)
	assert.Equal(t, []string{"not-yet-priced"}, r.Unpriced)

	byModel := map[string]AIUsageRow{}
	for _, m := range r.ByModel {
		byModel[m.Label] = m
	}
	assert.InDelta(t, 15.5, byModel["claude-fable-5"].Cost, 1e-9)
	assert.InDelta(t, 1.5, byModel["claude-haiku-4-5"].Cost, 1e-9)
	assert.InDelta(t, 2, byModel["gateway-llm"].Cost, 1e-9)
	assert.Equal(t, 2, byModel["claude-fable-5"].Calls)

	assert.Equal(t, aiPurposeDeployReview, r.ByPurpose[0].Label)
	assert.Equal(t, "2026-10-06", r.ByDay[0].Label) // newest first
	assert.Equal(t, "999", groupDigits(999))
	assert.Equal(t, "1,000", groupDigits(1000))
}

func TestAIUsageIsRecordedAndReadBack(t *testing.T) {
	withTempAppData(t)
	recordAIUsage(context.Background(), aiPurposeCodeReview, "m", "", "PO/da", aiUsage{Input: 12, Output: 3})
	recordAIUsage(context.Background(), aiPurposeCodeReview, "m", "", "PO/da", aiUsage{}) // nothing reported, nothing written
	month := time.Now().Format("2006-01")
	got := loadAIUsage(month)
	require.Len(t, got, 1)
	assert.Equal(t, 12, got[0].Input)
	assert.Equal(t, []string{month}, aiUsageMonths())
	assert.Empty(t, loadAIUsage("../../etc/passwd")) // only a month names a file
}

// A streamed OpenAI-compatible reply carries its token counts on a last
// chunk with no choices, and only when asked for them.
func TestOpenAIStreamReportsUsage(t *testing.T) {
	withCompanyINI(t, "AI_ENABLED = true")
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		asked = string(buf)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":42,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	resp, err := aiChatTurnStreamWith(context.Background(), aiUserConfig{provider: aiProviderOpenAI, modelID: "m", apiKey: "k", baseURL: server.URL},
		[]aiChatMessage{{Role: "user", Content: "hi"}}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "OK", resp.Content)
	assert.Equal(t, aiUsage{Input: 42, Output: 7}, resp.Usage)
	assert.Contains(t, asked, `"include_usage":true`)
	assert.NotContains(t, asked, "Usage") // the counts never travel back to the provider
}

func TestUsageDaysCoverTheCalendar(t *testing.T) {
	byDay := map[string]*AIUsageRow{"2026-09-03": {Input: 5, Output: 1, Calls: 1}}
	byModel := map[string]map[string]int{"2026-09-03": {"claude-fable-5": 6}}
	days := usageDays("2026-09", byDay, byModel, time.Date(2026, 10, 6, 0, 0, 0, 0, time.Local))
	assert.Len(t, days, 30) // a past month: all of it
	assert.Equal(t, AIUsageDay{Day: "09-03", Input: 5, Output: 1, Calls: 1, Models: map[string]int{"claude-fable-5": 6}}, days[2])
	assert.Zero(t, days[0].Input) // quiet days are on the axis too

	assert.Len(t, usageDays("2026-10", nil, nil, time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)), 6) // this month: through today
	assert.Nil(t, usageDays("bad", nil, nil, time.Now()))
}
