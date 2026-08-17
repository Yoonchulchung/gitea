// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"encoding/json"
	"fmt"

	"gitea.dev/modules/git"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This package's file tools (list_files/read_file/write_file) are defined
// as a real MCP (Model Context Protocol) server rather than an ad hoc
// tool-calling struct — see docs/company/ai-agent.md on why. The server
// and its one client are both in-process, connected over
// mcp.NewInMemoryTransports (no subprocess, no real network socket): the
// point isn't distributed communication, it's speaking the same protocol
// real external MCP servers do, so this tool trio stays reusable (by an
// external MCP-aware client later, or alongside other MCP servers this
// package might add) instead of being locked to our own bespoke schema.
//
// One server+session is built fresh per WorkspaceAI request, closing over
// that request's own gitRepo/branch/edits/openFiles — there's no shared or
// global tool state.

// newWorkspaceMCPServer builds the MCP server for one WorkspaceAI request.
// edits and openFiles are the same maps WorkspaceAI already threads through
// its turn loop: edits accumulates write_file's proposals (never touches
// git — see docs/company/ai-agent.md's safety boundaries), openFiles is
// every tab currently open in the person's browser, live content included,
// which read_file checks before falling back to git.
func newWorkspaceMCPServer(gitRepo *git.Repository, branch string, edits map[string]*workspaceAIEdit, openFiles map[string]string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "company-workspace", Version: "1.0.0"}, nil)

	server.AddTool(&mcp.Tool{
		Name:        "list_files",
		Description: "List every file path in the repository (current branch).",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		commit, err := gitRepo.GetBranchCommit(ctx, branch)
		if err != nil {
			return errorResult(err), nil
		}
		tree, err := commit.SubTree(ctx, gitRepo, "/")
		if err != nil {
			return errorResult(err), nil
		}
		entries, err := tree.ListEntriesRecursiveFast(ctx, gitRepo)
		if err != nil {
			return errorResult(err), nil
		}
		paths := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && !e.IsSubModule() {
				paths = append(paths, e.Name())
			}
		}
		out, err := json.Marshal(paths)
		if err != nil {
			return errorResult(err), nil
		}
		return textResult(string(out)), nil
	})

	server.AddTool(&mcp.Tool{
		Name:        "read_file",
		Description: "Read one file's current content by its repo-relative path.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []string{"path"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult(err), nil
		}
		if e, ok := edits[args.Path]; ok {
			return textResult(e.Content), nil // read back what the agent itself already staged this turn
		}
		if content, ok := openFiles[args.Path]; ok {
			return textResult(content), nil // an open tab's live (possibly unsaved) content beats whatever's actually committed
		}
		commit, err := gitRepo.GetBranchCommit(ctx, branch)
		if err != nil {
			return errorResult(err), nil
		}
		entry, err := commit.GetTreeEntryByPath(ctx, gitRepo, args.Path)
		if err != nil {
			return errorResult(fmt.Errorf("no such file: %s", args.Path)), nil
		}
		blob := entry.Blob(gitRepo)
		// GetBlobBytes treats a non-positive limit as "read nothing", not
		// "unlimited" — pass the blob's real size instead of -1. Left as
		// -1 here, read_file always returned "" for any committed file not
		// already open in a tab, which could make the model think a real
		// file was empty and confidently "rewrite" it from scratch.
		content, err := blob.GetBlobBytes(ctx, blob.Size(ctx))
		if err != nil {
			return errorResult(err), nil
		}
		return textResult(string(content)), nil
	})

	server.AddTool(&mcp.Tool{
		Name:        "write_file",
		Description: "Propose a file's new full content (creating it if it doesn't exist yet). This does not save anything by itself — it only stages a proposed edit for the human to review.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string", "description": "The file's complete new content, not a diff."},
			},
			"required": []string{"path", "content"},
		},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args workspaceAIEdit
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errorResult(err), nil
		}
		if args.Path == "" {
			return errorResult(fmt.Errorf("path required")), nil
		}
		edits[args.Path] = &args
		return textResult("ok, staged as a proposed edit"), nil
	})

	return server
}

// connectMCPSession connects server and a fresh in-process client over an
// in-memory transport pair, per the SDK's own documented ordering (server
// side first, via the non-blocking Server.Connect — not Run, which blocks
// until the session ends). Callers get back a session ready for
// ListTools/CallTool, and are responsible for cs.Close() once the
// request's turn loop ends.
func connectMCPSession(ctx context.Context, server *mcp.Server) (*mcp.ClientSession, error) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "company-workspace-client", Version: "1.0.0"}, nil)
	return client.Connect(ctx, clientTransport, nil)
}

// mcpToolsToAI converts an MCP tool list (session.ListTools) into this
// package's provider-agnostic aiTool shape (company/ai.go), the same one
// OpenAI- and Anthropic-shaped requests are both built from.
func mcpToolsToAI(tools []*mcp.Tool) []aiTool {
	out := make([]aiTool, 0, len(tools))
	for _, t := range tools {
		var tool aiTool
		tool.Type = "function"
		tool.Function.Name = t.Name
		tool.Function.Description = t.Description
		if schema, ok := t.InputSchema.(map[string]any); ok {
			tool.Function.Parameters = schema
		}
		out = append(out, tool)
	}
	return out
}

// mcpResultToText flattens a CallToolResult's content blocks into the
// plain string this package's agent loop feeds back to the model as a
// tool result — every one of our own tools only ever returns TextContent,
// so this doesn't need to handle images/resources/etc.
func mcpResultToText(result *mcp.CallToolResult) string {
	var out string
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out += tc.Text
		}
	}
	if result.IsError && out == "" {
		out = "error: tool call failed"
	}
	return out
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "error: " + err.Error()}}}
}
