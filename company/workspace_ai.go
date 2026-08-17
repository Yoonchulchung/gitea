// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"encoding/json"
	"fmt"
	"net/http"

	"gitea.dev/modules/git"
	"gitea.dev/services/context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// workspaceAIMaxTurns caps how many read_file/list_files/write_file round
// trips the agent loop below will run before giving up and returning
// whatever it has — a runaway "keep calling tools forever" loop would
// otherwise burn API cost with nothing to show for it. Generous enough for
// "read a few files, then write one or two" but not an open-ended budget.
const workspaceAIMaxTurns = 12

const tplWorkspaceAISystemPrompt = `You are a coding assistant helping a non-technical employee edit files in their department's internal Gitea repository, on branch %q. They are not fluent in git/programming jargon.

Use list_files and read_file to look around before making changes — don't guess at a file's content.

write_file replaces a file's entire content with whatever you pass — there is no append/patch operation, so if the file already has content the employee wants to keep, you must read it first (read_file, or their own open tab shown below) and pass the FULL result — old content plus the new part — not just the new part by itself. Only skip reading first and write the literal new content alone when the employee clearly means to replace/overwrite/clear the file's CONTENT while keeping the file itself (e.g. "내용 지워줘", "내용을 비워줘", "덮어써줘", "새로 써줘") or the file doesn't exist yet. A plain "X를 써줘"/"write X to the file" on a file that already has something in it almost always means add X to what's already there, not erase it — when in doubt, keep the existing content and add to it rather than guess the other way.

delete_file removes a file entirely — use this, not write_file with empty content, whenever the employee means the file itself should go away (e.g. "이 파일 지워줘"/"삭제해줘" naming a file, "필요없는 파일이니 없애줘"). This is the one place "지워줘" is genuinely ambiguous in Korean: "내용을 지워줘" (clear its content) means write_file with an empty/new body and keep the file; "파일을 지워줘"/"이 파일 삭제해줘" (delete the file) means delete_file. When it's unclear which one they mean, ask rather than guessing — deleting the wrong thing is harder to notice than an unwanted edit.

rename_file changes a file's path without touching its content — use this instead of delete_file followed by write_file whenever the content itself isn't changing, so the change shows up as one rename rather than an unrelated deletion plus an unrelated new file. If the employee wants both a new name AND different content, rename_file first, then write_file the file at its new path.

write_file/delete_file/rename_file all only stage a proposal — none of them save, commit, or otherwise touch anything by themselves, so don't warn the user about that.

When you're done, reply with a short, plain-language summary of what you changed and why — no jargon, as if explaining to a colleague who has never used git. Reply in whatever language the employee's own message was written in (Korean, English, German, whatever) — match them, don't default to Korean.`

// activeFileContextTemplate is appended to the system prompt whenever the
// request names an ActivePath — the tab open in the editor at the moment
// the person sent their message (company-workspace.ts). Its content is
// included directly rather than making the model call read_file for it
// itself: whatever they're looking at is almost always what "here"/"this"/
// an unqualified instruction refers to, same as Claude Code treating the
// open file as ambient context. Every OTHER open tab is still reachable
// via read_file (runWorkspaceAITool checks OpenFiles before falling back
// to git), it just isn't spelled out up front the same way.
const activeFileContextTemplate = `

The employee currently has %q open in their editor. Its current content (which may include their own unsaved edits, not just what's on the branch):

%s

Treat this as the file "here"/"this" most likely refers to if their message doesn't name one explicitly — but still use read_file for any other file you need (including other tabs they have open elsewhere — read_file always returns their latest unsaved content, not just what's on the branch).`

// workspaceAIHistoryTurn is one prior user/assistant text turn, as the
// frontend remembers it (web_src/js/features/company-ai-chat.ts) and
// resends on every request — there's no server-side conversation state,
// see docs/company/ai-agent.md on why. Tool calls from earlier turns are
// deliberately not replayed: they're an implementation detail of how that
// earlier answer was produced, not something the model needs to see again
// to continue the conversation coherently.
type workspaceAIHistoryTurn struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// workspaceAIOpenFile is one open editor tab's live content at send time —
// which may include unsaved edits, so it's not necessarily what's on the
// branch (what read_file would return on its own). The frontend
// (company-workspace.ts) sends every open tab, not just the active one:
// read_file has to be able to see an unsaved edit in ANY open tab, not
// only whichever one happened to be on screen when the message was sent —
// see runWorkspaceAITool's read_file case below, which checks these before
// falling back to git.
type workspaceAIOpenFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type workspaceAIRequest struct {
	Instruction string                   `json:"instruction"`
	History     []workspaceAIHistoryTurn `json:"history"`
	ActivePath  string                   `json:"activePath"`
	OpenFiles   []workspaceAIOpenFile    `json:"openFiles"`
}

type workspaceAIEdit struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// streamEvent is one line of the ndjson protocol documented in
// docs/company/ai-agent.md — {"type":"text"|"tool"|"edit"|"done"|"error", ...}.
// context.ResponseWriter embeds http.Flusher directly (services/context/response.go),
// so w doubles as its own flusher.
func writeStreamEvent(w context.ResponseWriter, event map[string]any) {
	enc, err := json.Marshal(event)
	if err != nil {
		return
	}
	w.Write(enc) //nolint:errcheck // best-effort — a write failing means the client's gone, nothing to do about it
	w.Write([]byte("\n"))
	w.Flush()
}

// WorkspaceAI runs one agentic request against the internal AI tool,
// scoped to this repo's branch: it can read_file/list_files freely, and
// write_file to propose edits — streamed back to the frontend
// (web_src/js/features/company-ai-chat.ts) as newline-delimited JSON
// events (docs/company/ai-agent.md) rather than one final response, so the
// chat sidebar can show text as it's generated, Claude-Code-style. Nothing
// here writes to git — edits are proposals the frontend drops into open
// editor tabs, still requiring the page's own Save button.
// Mounted at /{owner}/{repo}/_edits_ai/{branch}, alongside Workspace/
// WorkspaceSave (routers/web/web.go), so ctx.Repo.Repository/.BranchName
// are already resolved.
func WorkspaceAI(ctx *context.Context) {
	if !requireWorkspaceWrite(ctx) {
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

	repo := ctx.Repo.Repository
	branch := ctx.Repo.BranchName

	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, repo)
	if err != nil {
		ctx.ServerError("RepositoryFromRequestContextOrOpen", err)
		return
	}

	ctx.Resp.Header().Set("Content-Type", "application/x-ndjson")
	ctx.Resp.Header().Set("Cache-Control", "no-cache")
	ctx.Resp.WriteHeader(http.StatusOK)

	openFiles := make(map[string]string, len(req.OpenFiles))
	for _, f := range req.OpenFiles {
		openFiles[f.Path] = f.Content
	}

	systemPrompt := fmt.Sprintf(tplWorkspaceAISystemPrompt, branch)
	if activeContent, ok := openFiles[req.ActivePath]; req.ActivePath != "" && ok {
		systemPrompt += fmt.Sprintf(activeFileContextTemplate, req.ActivePath, activeContent)
	}

	messages := make([]aiChatMessage, 0, len(req.History)+2)
	messages = append(messages, aiChatMessage{Role: "system", Content: systemPrompt})
	for _, h := range req.History {
		if h.Role != "user" && h.Role != "assistant" {
			continue // ignore anything malformed rather than send a broken conversation to the model
		}
		messages = append(messages, aiChatMessage{Role: h.Role, Content: h.Content})
	}
	messages = append(messages, aiChatMessage{Role: "user", Content: req.Instruction})

	edits := map[string]*workspaceAIEdit{} // keyed by path, last write_file wins
	deletes := map[string]bool{}           // keyed by path
	renames := map[string]string{}         // from path -> to path

	mcpServer := newWorkspaceMCPServer(gitRepo, branch, edits, deletes, renames, openFiles)
	mcpSession, err := connectMCPSession(ctx, mcpServer)
	if err != nil {
		writeStreamEvent(ctx.Resp, map[string]any{"type": "error", "message": err.Error()})
		return
	}
	defer mcpSession.Close() //nolint:errcheck // best-effort cleanup at request end, nothing left to report it to

	toolsResult, err := mcpSession.ListTools(ctx, nil)
	if err != nil {
		writeStreamEvent(ctx.Resp, map[string]any{"type": "error", "message": err.Error()})
		return
	}
	tools := mcpToolsToAI(toolsResult.Tools)

	for turn := 0; turn < workspaceAIMaxTurns; turn++ {
		resp, err := aiChatTurnStream(ctx, ctx.Doer.ID, messages, tools, func(delta string) {
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
			_ = json.Unmarshal([]byte(call.Function.Arguments), &args) // best-effort — only used for the status line below
			writeStreamEvent(ctx.Resp, map[string]any{"type": "tool", "name": call.Function.Name, "args": args})

			result, err := mcpSession.CallTool(ctx, &mcp.CallToolParams{
				Name:      call.Function.Name,
				Arguments: json.RawMessage(call.Function.Arguments),
			})
			var resultText string
			if err != nil {
				resultText = "error: " + err.Error() // protocol-level failure (bad tool name, transport) — fed back to the model, not a request-ending failure
			} else {
				resultText = mcpResultToText(result)
				if !result.IsError {
					switch call.Function.Name {
					case "write_file":
						if e, ok := edits[fmt.Sprint(args["path"])]; ok {
							writeStreamEvent(ctx.Resp, map[string]any{"type": "edit", "path": e.Path, "content": e.Content})
						}
					case "delete_file":
						if path := fmt.Sprint(args["path"]); deletes[path] {
							writeStreamEvent(ctx.Resp, map[string]any{"type": "delete", "path": path})
						}
					case "rename_file":
						from := fmt.Sprint(args["from_path"])
						if to, ok := renames[from]; ok {
							writeStreamEvent(ctx.Resp, map[string]any{"type": "rename", "from": from, "to": to})
						}
					}
				}
			}
			messages = append(messages, aiChatMessage{Role: "tool", ToolCallID: call.ID, Content: resultText})
		}
	}

	writeStreamEvent(ctx.Resp, map[string]any{"type": "done"})
}
