// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"testing"

	repo_model "gitea.dev/models/repo"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppInsightToolsAreOffered(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	addAppInsightTools(server, nil, &repo_model.Repository{OwnerName: "PO", Name: "app"}) // registration reads only the name; the handlers need the request
	session, err := connectMCPSession(context.Background(), server)
	require.NoError(t, err)
	defer session.Close()
	listed, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"app_status", "app_logs", "deploy_checks", "run_startup_check"} {
		assert.True(t, names[want], want)
	}
	assert.Equal(t, aiLogLinesDefault, clampLogLines(0))
	assert.Equal(t, aiLogLinesMax, clampLogLines(10000))
}
