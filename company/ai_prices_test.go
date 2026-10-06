// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKnownModelPrice(t *testing.T) {
	in, out, ok := knownModelPrice("claude-fable-5")
	assert.True(t, ok)
	assert.Equal(t, [2]float64{10, 50}, [2]float64{in, out})

	in, _, ok = knownModelPrice("claude-haiku-4-5-20251001") // a dated ID
	assert.True(t, ok)
	assert.InDelta(t, 1, in, 0)

	_, _, ok = knownModelPrice("claude-opus-5-5") // not mistaken for claude-opus-5
	assert.True(t, ok)
	in, _, _ = knownModelPrice("claude-opus-5-5")
	assert.InDelta(t, 4, in, 0)

	_, _, ok = knownModelPrice("company-gateway-llm")
	assert.False(t, ok)
}

func TestParsePriceEstimate(t *testing.T) {
	e, ok := parsePriceEstimate("```json\n{\"input\": 3, \"output\": 15, \"confidence\": \"low\", \"note\": \"비슷한\\n모델 기준\"}\n```")
	assert.True(t, ok)
	assert.Equal(t, priceEstimate{Input: 3, Output: 15, Confidence: "low", Note: "비슷한 모델 기준"}, e)
	for _, bad := range []string{"", "모름", `{"input": -1, "output": 2}`, `{"input": 1e9, "output": 2}`} {
		_, ok := parsePriceEstimate(bad)
		assert.False(t, ok, bad)
	}
}
