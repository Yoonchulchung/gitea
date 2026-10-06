// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An administrator's access choice must apply even after the department narrowed it.
func TestAdminAccessOverridesDepartmentNarrowing(t *testing.T) {
	withTempAppData(t)
	t.Cleanup(func() { departmentAccess.Delete(appKey("PO", "da")) })

	require.NoError(t, SetDepartmentAccess("PO", "da", "dept-user", AccessOrg))
	assert.Equal(t, AccessOrg, effectiveAccess("PO", "da", AccessPublic))

	require.NoError(t, clearDepartmentAccess("PO", "da", "admin", AccessPublic))
	assert.Equal(t, AccessPublic, effectiveAccess("PO", "da", AccessPublic))
	st, _ := readAppStateFile(appStateFile("PO", "da"))
	require.NotNil(t, st)
	assert.Empty(t, st.AccessChoice)

	require.NoError(t, clearDepartmentAccess("PO", "never-narrowed", "admin", AccessPublic))
}
