// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	stdjson "encoding/json" //nolint:depguard // RawMessage is what the MCP client takes; Gitea's wrapper has none
	"errors"
	"fmt"
	"net/http"
	"strings"

	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	gitea_context "gitea.dev/services/context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The person approving a deploy request is not a developer, and the native
// pull request page is built for one: reviewers, milestones, assignees,
// dependencies, a due date. None of those decide whether an app may go
// live. For a Deploy Request PR the sidebar is replaced with what does —
// who asked, what the app wants to be allowed, which packages it pulls in
// — and an assistant that can read the change and answer questions about
// it in plain language. See docs/company/ai-agent.md.

// reviewPackage is one requirements.txt line as the sidebar shows it.
type reviewPackage struct {
	Name    string
	Version string
	Allowed bool // already on this app's allow list or the base set
}

// reviewPackages classifies what the snapshot's requirements.txt asks for
// against what the app may already install. The denied ones are what
// merging this request would approve.
func reviewPackages(requirements string, settings AppSettings) []reviewPackage {
	reqs, _ := ParseRequirements(requirements)
	denied := map[string]bool{}
	for _, d := range DeniedPackages(reqs, settings.AllowedPackages()) {
		denied[d.Name] = true
	}
	out := make([]reviewPackage, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, reviewPackage{Name: r.Name, Version: r.Version, Allowed: !denied[r.Name]})
	}
	return out
}

// deployRequestFile reads one file of the department's snapshot from the
// request's branch; "" when the branch is gone (the request closed) or the
// file is not in it.
func deployRequestFile(ctx context.Context, gitRepo *git.Repository, branch, prefix, path string) string {
	commit, err := gitRepo.GetBranchCommit(ctx, branch)
	if err != nil {
		return ""
	}
	entry, err := commit.GetTreeEntryByPath(ctx, gitRepo, prefix+"/"+path)
	if err != nil {
		return ""
	}
	blob := entry.Blob(gitRepo)
	content, err := blob.GetBlobBytes(ctx, blob.Size(ctx))
	if err != nil {
		return ""
	}
	return string(content)
}

// setDeployReviewData fills in the sidebar for a verified Deploy Request PR.
func setDeployReviewData(ctx *gitea_context.Context, pr *issues_model.PullRequest, deptOwner, deptName string) {
	ctx.Data["IsDeployRequest"] = true
	ctx.Data["DeployDeptOwner"] = deptOwner
	ctx.Data["DeployDeptName"] = deptName
	ctx.Data["DeployDeptLink"] = fmt.Sprintf("%s/%s/%s", setting.AppSubURL, deptOwner, deptName)
	ctx.Data["DeployFilesURL"] = fmt.Sprintf("%s/org/%s/dashboard/deploy-requests/%d", setting.AppSubURL, deptOwner, pr.ID)
	ctx.Data["DeployChatURL"] = fmt.Sprintf("%s/company/deploy-request/%d/chat", setting.AppSubURL, pr.ID)
	if _, _, requesterID, ok := parseDeployBranchName(pr.HeadBranch); ok {
		if requester, err := user_model.GetUserByID(ctx, requesterID); err == nil {
			ctx.Data["DeployRequester"] = requester
		}
	}
	if status, err := deployStatusFor(ctx, pr); err == nil {
		ctx.Data["DeployRequestStatus"] = status
	}
	// Always a slice, even an empty one: the sidebar counts these, and `len`
	// of an unset value stops the template half-way down the page.
	ctx.Data["PermissionRequests"] = LoadPermissionRequests(deptOwner, deptName, pr.ID)
	ctx.Data["DeployPackages"] = []reviewPackage{}
	if gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, ctx.Repo.Repository); err == nil {
		requirements := deployRequestFile(ctx, gitRepo, pr.HeadBranch, deployPathPrefix(deptOwner, deptName), "requirements.txt")
		ctx.Data["DeployPackages"] = reviewPackages(requirements, SettingsFor(deptOwner, deptName))
	}
	ctx.Data["CompanyAIOffered"] = AIOfferedTo(ctx)
	ctx.Data["AIEnabled"] = AIConfiguredFor(ctx, ctx.Doer.ID)
	if cfg, err := loadAIUserConfig(ctx, ctx.Doer.ID); err == nil {
		ctx.Data["AIModel"] = cfg.modelID
		ctx.Data["AIProvider"] = cfg.provider
	}
}

const tplDeployReviewSystemPrompt = `You are helping an administrator who is not a developer decide whether to approve a deploy request: an internal department wants a new version of their small web app to go live on the company platform. Explain in plain language, without jargon, and be concrete: name the file and what the code does in everyday terms.

What matters for the decision: does the change do what the request says; does it read, send or store anything it should not (files outside its own data, other hosts, credentials); is a requested package well known and needed for what the code does; is anything left in that looks like a test or a mistake. Say clearly when something is fine — an administrator needs a "nothing to worry about" as much as a warning.

You can read the department's files with list_files and read_file, and the full change with get_diff. Read before answering rather than guessing. You cannot change anything; approving or rejecting is the administrator's decision, made on the page. Reply in the language the administrator writes in.`

// deployReviewDiffMaxChars is how much of the diff goes into the system
// prompt up front. The rest is a get_diff call away; the model should not
// have to ask for the change it was opened to discuss.
const deployReviewDiffMaxChars = 30000

// deployReviewContext is the request as the assistant sees it: what was
// asked, by whom, what the app wants to be allowed, and the change itself.
func deployReviewContext(pr *issues_model.PullRequest, deptOwner, deptName, diff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nDepartment app: %s/%s\nRequest title: %s\n", deptOwner, deptName, pr.Issue.Title)
	if body := strings.TrimSpace(pr.Issue.Content); body != "" {
		fmt.Fprintf(&b, "Request message:\n%s\n", body)
	}
	if requests := LoadPermissionRequests(deptOwner, deptName, pr.ID); len(requests) > 0 {
		b.WriteString("\nPermissions this request asks for (approved together with the request):\n")
		for _, r := range requests {
			fmt.Fprintf(&b, "- %s: %s", r.Kind, r.Value)
			if r.Detail != "" {
				fmt.Fprintf(&b, " (%s)", r.Detail)
			}
			if r.Reason != "" {
				fmt.Fprintf(&b, " — reason given: %s", r.Reason)
			}
			b.WriteString("\n")
		}
	} else {
		b.WriteString("\nNo extra permissions are requested.\n")
	}
	if diff == "" {
		b.WriteString("\nThe change itself is no longer available (the request is closed and its branch was cleaned up).\n")
	} else if len(diff) > deployReviewDiffMaxChars {
		fmt.Fprintf(&b, "\nThe first part of the change (%d of %d characters; get_diff returns all of it):\n%s\n", deployReviewDiffMaxChars, len(diff), diff[:deployReviewDiffMaxChars])
	} else {
		fmt.Fprintf(&b, "\nThe change:\n%s\n", diff)
	}
	return b.String()
}

// newDeployReviewMCPServer offers the reviewer's tools: the department's
// files as they are in this request, and the change. Paths are the
// department's own — the central repository's prefix is added and removed
// here, so the model never sees or escapes it. Nothing writes.
func newDeployReviewMCPServer(gitRepo *git.Repository, branch, prefix, diff string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "company-deploy-review", Version: "1.0.0"}, nil)

	server.AddTool(&mcp.Tool{
		Name:        "list_files",
		Description: "List every file in the department's app as it would be deployed.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		commit, err := gitRepo.GetBranchCommit(ctx, branch)
		if err != nil {
			return errorResult(err), nil
		}
		tree, err := commit.SubTree(ctx, gitRepo, prefix)
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
		Description: "Read one of the app's files, by the path list_files shows.",
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
		if hasParentSegment(args.Path) {
			return errorResult(fmt.Errorf("no such file: %s", args.Path)), nil
		}
		content := deployRequestFile(ctx, gitRepo, branch, prefix, strings.TrimPrefix(args.Path, "/"))
		if content == "" {
			return errorResult(fmt.Errorf("no such file: %s", args.Path)), nil
		}
		return textResult(content), nil
	})

	server.AddTool(&mcp.Tool{
		Name:        "get_diff",
		Description: "The whole change this request would deploy, as a unified diff.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if diff == "" {
			return errorResult(errors.New("the change is no longer available")), nil
		}
		return textResult(diff), nil
	})

	return server
}

// DeployRequestChat answers an administrator's questions about one Deploy
// Request, streamed as the same ndjson the workspace assistant uses
// (web_src/js/features/company-ai-chat.ts). Read-only: the tools can look
// at the request, never touch it. Mounted at
// /company/deploy-request/{id}/chat (company/routes.go), admin only, on the
// administrator's own AI settings like TriggerDeployRequestAIReview.
func DeployRequestChat(ctx *gitea_context.Context) {
	if !ctx.Doer.IsAdmin {
		ctx.NotFound(nil)
		return
	}
	pr, err := issues_model.GetPullRequestByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		ctx.NotFound(err)
		return
	}
	deptOwner, deptName, ok := verifyDeployRequestPR(ctx, pr)
	if !ok {
		ctx.NotFound(nil)
		return
	}
	if !AIEnabled() {
		ctx.HTTPError(http.StatusServiceUnavailable, errAIDisabled.Error())
		return
	}
	if !AIConfiguredFor(ctx, ctx.Doer.ID) {
		ctx.HTTPError(http.StatusServiceUnavailable, "AI not set up yet — add your API key at /user/settings/ai")
		return
	}
	var req workspaceAIRequest
	if err := json.NewDecoder(ctx.Req.Body).Decode(&req); err != nil || req.Instruction == "" {
		ctx.HTTPError(http.StatusBadRequest, "instruction required")
		return
	}

	central := pr.BaseRepo
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, central)
	if err != nil {
		ctx.ServerError("RepositoryFromRequestContextOrOpen", err)
		return
	}
	var diffBuf strings.Builder
	// A closed request's branch is gone; the assistant then says so rather
	// than the page failing.
	_ = gitRepo.GetDiff(ctx, central.DefaultBranch+"..."+pr.HeadBranch, &diffBuf)
	diff := diffBuf.String()
	prefix := deployPathPrefix(deptOwner, deptName)
	// The snapshot lives under the department's prefix in the central
	// repository; the paths in the diff carry it, and the reviewer should
	// read the same names it lists.
	diff = strings.ReplaceAll(diff, " a/"+prefix+"/", " a/")
	diff = strings.ReplaceAll(diff, " b/"+prefix+"/", " b/")

	ctx.Resp.Header().Set("Content-Type", "application/x-ndjson")
	ctx.Resp.Header().Set("Cache-Control", "no-cache")
	ctx.Resp.WriteHeader(http.StatusOK)

	messages := make([]aiChatMessage, 0, len(req.History)+2)
	messages = append(messages, aiChatMessage{Role: "system", Content: tplDeployReviewSystemPrompt + deployReviewContext(pr, deptOwner, deptName, diff)})
	for _, h := range req.History {
		if h.Role == "user" || h.Role == "assistant" {
			messages = append(messages, aiChatMessage{Role: h.Role, Content: h.Content})
		}
	}
	messages = append(messages, aiChatMessage{Role: "user", Content: req.Instruction})

	mcpSession, err := connectMCPSession(ctx, newDeployReviewMCPServer(gitRepo, pr.HeadBranch, prefix, diff))
	if err != nil {
		writeStreamEvent(ctx.Resp, map[string]any{"type": "error", "message": err.Error()})
		return
	}
	defer mcpSession.Close() // best-effort cleanup at request end, nothing left to report it to
	toolsResult, err := mcpSession.ListTools(ctx, nil)
	if err != nil {
		writeStreamEvent(ctx.Resp, map[string]any{"type": "error", "message": err.Error()})
		return
	}
	tools := mcpToolsToAI(toolsResult.Tools)

	for range workspaceAIMaxTurns {
		resp, err := aiChatTurnStream(ctx, ctx.Doer.ID, req.Model, messages, tools, func(delta string) {
			writeStreamEvent(ctx.Resp, map[string]any{"type": "text", "delta": delta})
		})
		if err != nil {
			writeStreamEvent(ctx.Resp, map[string]any{"type": "error", "message": err.Error()})
			return
		}
		messages = append(messages, *resp)
		if len(resp.ToolCalls) == 0 {
			break
		}
		for _, call := range resp.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(call.Function.Arguments), &args) // only for the status line
			writeStreamEvent(ctx.Resp, map[string]any{"type": "tool", "name": call.Function.Name, "args": args})
			result, err := mcpSession.CallTool(ctx, &mcp.CallToolParams{Name: call.Function.Name, Arguments: stdjson.RawMessage(call.Function.Arguments)})
			resultText := "error: tool call failed"
			if err != nil {
				resultText = "error: " + err.Error()
			} else if result != nil {
				resultText = mcpResultToText(result)
			}
			messages = append(messages, aiChatMessage{Role: "tool", ToolCallID: call.ID, Content: resultText})
		}
	}
	writeStreamEvent(ctx.Resp, map[string]any{"type": "done"})
}
