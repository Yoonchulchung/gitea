// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSystemCPUAndDownsample(t *testing.T) {
	a, ok := parseCPUTimes("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3 4\n")
	assert.True(t, ok)
	b, _ := parseCPUTimes("cpu  150 0 150 750 150 0 0 0 0 0\n")
	assert.InDelta(t, 50.0, cpuPercentBetween(a, b), 0.01, "idle and iowait are idle")
	_, ok = parseCPUTimes("intr 1 2 3")
	assert.False(t, ok)

	in := []SystemSample{{T: 1, CPU: 10, DiskUsedMB: 5}, {T: 2, CPU: 30, DiskUsedMB: 7}, {T: 3, CPU: 50, MemTotalMB: 9}}
	out := downsampleSystem(in, 2)
	assert.Len(t, out, 2)
	assert.Equal(t, SystemSample{T: 1, CPU: 20, DiskUsedMB: 7}, out[0])
	assert.Equal(t, int64(9), out[1].MemTotalMB)
}
