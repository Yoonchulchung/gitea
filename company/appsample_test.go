// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A burst is not a breach: only a minute of samples over the limit acts,
// and one sample back inside it starts the count again.
func TestCPUWatchdogActsOnlyOnASustainedOverrun(t *testing.T) {
	key := "test-cpu"
	for i := 1; i < cpuBreachesBeforeStop; i++ {
		assert.False(t, cpuOverLimit(key, 250, 100), "sample %d", i)
	}
	assert.False(t, cpuOverLimit(key, 40, 100), "a sample within the limit resets")
	for i := 1; i < cpuBreachesBeforeStop; i++ {
		assert.False(t, cpuOverLimit(key, 250, 100))
	}
	assert.True(t, cpuOverLimit(key, 250, 100), "the twelfth consecutive sample stops it")
	assert.False(t, cpuOverLimit(key, 250, 100), "and the count starts over")
	assert.False(t, cpuOverLimit(key, 900, 0), "no limit, no watchdog")
}
