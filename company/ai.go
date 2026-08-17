// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
)

// Per-user setting keys (user_model.GetUserSetting/SetUserSetting — the
// same generic key-value store native settings like theme preference use,
// so this needs no schema change). Each person picks their own provider/
// model/key/reasoning effort at /user/settings/ai (company/settings_ai.go)
// — see docs/company/ai-agent.md on why this is per-user, not instance-wide.
const (
	userSettingAIProvider        = "company.ai.provider" // "openai" (default) or "anthropic"
	userSettingAIModelID         = "company.ai.model_id"
	userSettingAIAPIKey          = "company.ai.api_key"
	userSettingAIReasoningEffort = "company.ai.reasoning_effort"
)

const (
	aiProviderOpenAI    = "openai"
	aiProviderAnthropic = "anthropic"
)

// anthropicAPIBase is fixed — unlike the internal OpenAI-compatible tool
// (one shared gateway URL, app.ini's AI_API_URL), anyone picking "Claude"
// is talking to Anthropic's own real API with their own personal key, so
// there's no separate URL setting to add.
const anthropicAPIBase = "https://api.anthropic.com"

const anthropicVersion = "2023-06-01"

// aiUserConfig is doer's own AI settings, loaded fresh on every call — see
// userSettingAI* above.
type aiUserConfig struct {
	provider        string
	modelID         string
	apiKey          string
	reasoningEffort string
}

func loadAIUserConfig(ctx context.Context, userID int64) (aiUserConfig, error) {
	provider, err := user_model.GetUserSetting(ctx, userID, userSettingAIProvider, aiProviderOpenAI)
	if err != nil {
		return aiUserConfig{}, err
	}
	modelID, err := user_model.GetUserSetting(ctx, userID, userSettingAIModelID)
	if err != nil {
		return aiUserConfig{}, err
	}
	apiKey, err := user_model.GetUserSetting(ctx, userID, userSettingAIAPIKey)
	if err != nil {
		return aiUserConfig{}, err
	}
	reasoningEffort, err := user_model.GetUserSetting(ctx, userID, userSettingAIReasoningEffort)
	if err != nil {
		return aiUserConfig{}, err
	}
	return aiUserConfig{provider: provider, modelID: modelID, apiKey: apiKey, reasoningEffort: reasoningEffort}, nil
}

// aiGatewayURL is the one instance-wide internal-AI endpoint (app.ini's
// [company] AI_API_URL) — only relevant for the OpenAI-compatible
// provider; empty until an admin sets it.
func aiGatewayURL() string {
	return strings.TrimSuffix(setting.CfgProvider.Section("company").Key("AI_API_URL").String(), "/")
}

// AIConfiguredFor reports whether doer can actually use the AI features
// yet. Everyone needs their own API key (/user/settings/ai) regardless of
// provider; the OpenAI-compatible provider additionally needs an admin to
// have set the instance-wide gateway URL (app.ini) — Anthropic doesn't,
// since it talks to Anthropic's own fixed API directly. Every AI-touching
// feature (workspace editor's "Ask AI" button, Deploy Request's review
// comment) checks this and no-ops/hides itself while it's false.
func AIConfiguredFor(ctx context.Context, userID int64) bool {
	c, err := loadAIUserConfig(ctx, userID)
	if err != nil || c.apiKey == "" {
		return false
	}
	if c.provider == aiProviderAnthropic {
		return true
	}
	return aiGatewayURL() != ""
}

type aiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // always "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-encoded, per the tool's own schema
	} `json:"function"`
}

// aiChatMessage is this package's provider-agnostic message shape — every
// caller (company/workspace_ai.go's agent loop, company/deploy.go's
// review) builds/reads only this; aiChatTurnStream below translates to and
// from whichever provider's own wire format underneath. Covers every role
// needed: "system"/"user" (plain Content), "assistant" (Content and/or
// ToolCalls it wants executed), and "tool" (a ToolCallID's result fed back
// in — OpenAI's own role name, reused here even for the Anthropic path,
// which maps it to a tool_result content block instead).
type aiChatMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content,omitempty"`
	ToolCalls  []aiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

// aiTool is this package's provider-agnostic function-calling schema — see
// workspaceAITools (company/workspace_ai.go) for the ones this package
// offers. Parameters is standard JSON Schema either way; only the field
// name differs on the wire (OpenAI's "parameters" vs Anthropic's
// "input_schema"), handled inside aiChatTurnStream.
type aiTool struct {
	Type     string `json:"type"` // always "function"
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// aiChat sends one system+user exchange, authenticated as userID's own
// saved provider/key/model/reasoning effort, and returns the assistant's
// reply text. No conversation history, no tool use — for Deploy Request's
// review comment (company/deploy.go), a single, self-contained "review
// this diff" request. The workspace editor's "Ask AI" needs tool use and
// live output, so it calls aiChatTurnStream directly instead.
func aiChat(ctx context.Context, userID int64, systemPrompt, userPrompt string) (string, error) {
	resp, err := aiChatTurnStream(ctx, userID, []aiChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, nil, nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// aiChatTurn is aiChatTurnStream without live output — every chunk still
// streams in under the hood (both providers are streaming-only here), just
// with onDelta left nil, so the caller only sees the final assembled
// message. Kept as a thin, clearly-named wrapper since most callers (the
// non-streaming ones) don't want to think about streaming at all.
func aiChatTurn(ctx context.Context, userID int64, messages []aiChatMessage, tools []aiTool) (*aiChatMessage, error) {
	return aiChatTurnStream(ctx, userID, messages, tools, nil)
}

// aiChatTurnStream is one request/response round-trip against userID's own
// configured provider, with the model's text hant to onDelta piece by
// piece as it arrives (for live display in the chat sidebar) if onDelta is
// non-nil. messages/tools/the returned message are all this package's own
// provider-agnostic shapes (aiChatMessage/aiTool) — translation to/from
// OpenAI's or Anthropic's actual wire format happens entirely inside this
// function and its two provider-specific helpers below, so callers (the
// agent loop in company/workspace_ai.go) never need to know which provider
// is in use. Callers that pass tools need to check the returned message's
// ToolCalls themselves, since a tool-capable model may return either a
// final text answer or a list of calls to execute and feed back.
func aiChatTurnStream(ctx context.Context, userID int64, messages []aiChatMessage, tools []aiTool, onDelta func(string)) (*aiChatMessage, error) {
	uc, err := loadAIUserConfig(ctx, userID)
	if err != nil {
		return nil, err
	}
	if uc.apiKey == "" {
		return nil, fmt.Errorf("AI not set up yet — add your API key at /user/settings/ai")
	}
	if uc.provider == aiProviderAnthropic {
		return aiChatTurnStreamAnthropic(ctx, uc, messages, tools, onDelta)
	}
	if aiGatewayURL() == "" {
		return nil, fmt.Errorf("AI not configured: an admin needs to set [company] AI_API_URL in app.ini")
	}
	return aiChatTurnStreamOpenAI(ctx, uc, messages, tools, onDelta)
}

// ---- OpenAI-compatible provider ----

type openAIChatRequest struct {
	Model           string          `json:"model"`
	Messages        []aiChatMessage `json:"messages"`
	Tools           []aiTool        `json:"tools,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Stream          bool            `json:"stream"`
}

// openAIStreamDelta/openAIStreamChunk mirror the OpenAI-compatible
// streaming shape (Server-Sent-Events framed, "data: {...}\n\n" lines,
// terminated by a literal "data: [DONE]"). Tool-call arguments arrive
// fragmented across many chunks, keyed by Index — reassembled below.
type openAIStreamDelta struct {
	Content   string `json:"content,omitempty"`
	ToolCalls []struct {
		Index    int    `json:"index"`
		ID       string `json:"id,omitempty"`
		Function struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
}

type openAIStreamChunk struct {
	Choices []struct {
		Delta openAIStreamDelta `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func aiChatTurnStreamOpenAI(ctx context.Context, uc aiUserConfig, messages []aiChatMessage, tools []aiTool, onDelta func(string)) (*aiChatMessage, error) {
	body, err := json.Marshal(openAIChatRequest{
		Model:           uc.modelID,
		Messages:        messages,
		Tools:           tools,
		ReasoningEffort: uc.reasoningEffort,
		Stream:          true,
	})
	if err != nil {
		return nil, err
	}

	resp, err := doAIRequest(ctx, aiGatewayURL()+"/chat/completions", body, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+uc.apiKey)
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var contentBuilder strings.Builder
	type toolCallBuilder struct {
		id, name string
		args     strings.Builder
	}
	toolCalls := map[int]*toolCallBuilder{}
	var toolCallOrder []int

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // a single chunk line can be large (a long tool-call arg fragment)
	for scanner.Scan() {
		payload, ok := sseDataPayload(scanner.Text())
		if !ok || payload == "[DONE]" {
			if payload == "[DONE]" {
				break
			}
			continue
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // a malformed/keep-alive line shouldn't kill an otherwise-good stream
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("AI error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			contentBuilder.WriteString(delta.Content)
			if onDelta != nil {
				onDelta(delta.Content)
			}
		}
		for _, tc := range delta.ToolCalls {
			b, ok := toolCalls[tc.Index]
			if !ok {
				b = &toolCallBuilder{}
				toolCalls[tc.Index] = b
				toolCallOrder = append(toolCallOrder, tc.Index)
			}
			if tc.ID != "" {
				b.id = tc.ID
			}
			if tc.Function.Name != "" {
				b.name = tc.Function.Name
			}
			b.args.WriteString(tc.Function.Arguments)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading AI stream: %w", err)
	}

	result := &aiChatMessage{Role: "assistant", Content: contentBuilder.String()}
	for _, idx := range toolCallOrder {
		b := toolCalls[idx]
		tc := aiToolCall{ID: b.id, Type: "function"}
		tc.Function.Name = b.name
		tc.Function.Arguments = orEmptyObject(b.args.String())
		result.ToolCalls = append(result.ToolCalls, tc)
	}
	return result, nil
}

// orEmptyObject defaults a tool call's Arguments to a valid, empty JSON
// object rather than leaving it "" — a no-argument tool (list_files has no
// parameters at all) can legitimately stream zero input_json_delta/
// arguments fragments, and both providers reject an empty/absent value
// where a JSON object is expected (Anthropic: "tool_use.input: Field
// required"; downstream json.Unmarshal into a tool's own args struct would
// also fail on "" rather than "{}").
func orEmptyObject(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}

// ---- Anthropic provider ----

type anthropicContentBlock struct {
	Type      string          `json:"type"` // "text" | "tool_use" | "tool_result"
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use
	Name      string          `json:"name,omitempty"`        // tool_use
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   string          `json:"content,omitempty"`     // tool_result
}

type anthropicMessage struct {
	Role    string                  `json:"role"` // "user" | "assistant" only
	Content []anthropicContentBlock `json:"content"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicThinking struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Thinking  *anthropicThinking `json:"thinking,omitempty"`
	Stream    bool               `json:"stream"`
}

// toAnthropicMessages translates this package's provider-agnostic
// conversation into Anthropic's shape: the (only ever one, in practice)
// "system" message becomes the top-level System field rather than a
// message; an assistant turn's Content and ToolCalls become sibling
// content blocks in one assistant message; and — since our agent loop
// appends one "tool" role aiChatMessage per tool call, but Anthropic
// expects all of a turn's tool_results together in a single user message —
// consecutive tool messages are merged into one user message with
// multiple tool_result blocks.
func toAnthropicMessages(messages []aiChatMessage) (system string, out []anthropicMessage) {
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		switch m.Role {
		case "system":
			if system != "" {
				system += "\n\n"
			}
			system += m.Content

		case "user":
			out = append(out, anthropicMessage{Role: "user", Content: []anthropicContentBlock{{Type: "text", Text: m.Content}}})

		case "assistant":
			var blocks []anthropicContentBlock
			if m.Content != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: json.RawMessage(orEmptyObject(tc.Function.Arguments)),
				})
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})

		case "tool":
			var blocks []anthropicContentBlock
			for i < len(messages) && messages[i].Role == "tool" {
				blocks = append(blocks, anthropicContentBlock{Type: "tool_result", ToolUseID: messages[i].ToolCallID, Content: messages[i].Content})
				i++
			}
			i-- // the outer for loop's own i++ accounts for the last one consumed
			out = append(out, anthropicMessage{Role: "user", Content: blocks})
		}
	}
	return system, out
}

// reasoningEffortToThinkingBudget maps this package's provider-agnostic
// "low"/"medium"/"high" reasoning effort (the same setting the OpenAI path
// sends as-is) onto Anthropic's extended-thinking token budget — the two
// providers don't share a parameter for this, so this is a judgment-call
// mapping, not a spec'd equivalence. Empty/unrecognized values disable
// thinking entirely (Anthropic's default, standard-mode behavior).
func reasoningEffortToThinkingBudget(effort string) int {
	switch effort {
	case "low":
		return 2048
	case "medium":
		return 8192
	case "high":
		return 24576
	default:
		return 0
	}
}

const anthropicMaxTokensBase = 8192

func aiChatTurnStreamAnthropic(ctx context.Context, uc aiUserConfig, messages []aiChatMessage, tools []aiTool, onDelta func(string)) (*aiChatMessage, error) {
	system, anthMessages := toAnthropicMessages(messages)

	var anthTools []anthropicTool
	for _, t := range tools {
		anthTools = append(anthTools, anthropicTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: t.Function.Parameters})
	}

	maxTokens := anthropicMaxTokensBase
	var thinking *anthropicThinking
	if budget := reasoningEffortToThinkingBudget(uc.reasoningEffort); budget > 0 {
		thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
		maxTokens = budget + anthropicMaxTokensBase
	}

	body, err := json.Marshal(anthropicRequest{
		Model:     uc.modelID,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  anthMessages,
		Tools:     anthTools,
		Thinking:  thinking,
		Stream:    true,
	})
	if err != nil {
		return nil, err
	}

	resp, err := doAIRequest(ctx, anthropicAPIBase+"/v1/messages", body, func(req *http.Request) {
		req.Header.Set("x-api-key", uc.apiKey)
		req.Header.Set("anthropic-version", anthropicVersion)
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var contentBuilder strings.Builder
	type blockState struct {
		toolUse  bool
		id, name string
		args     strings.Builder
	}
	blocks := map[int]*blockState{}
	var toolOrder []int

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		payload, ok := sseDataPayload(scanner.Text())
		if !ok {
			continue
		}

		var event struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}

		switch event.Type {
		case "error":
			if event.Error != nil {
				return nil, fmt.Errorf("AI error: %s", event.Error.Message)
			}
		case "content_block_start":
			if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				blocks[event.Index] = &blockState{toolUse: true, id: event.ContentBlock.ID, name: event.ContentBlock.Name}
				toolOrder = append(toolOrder, event.Index)
			}
		case "content_block_delta":
			if event.Delta == nil {
				continue
			}
			switch event.Delta.Type {
			case "text_delta":
				contentBuilder.WriteString(event.Delta.Text)
				if onDelta != nil {
					onDelta(event.Delta.Text)
				}
			case "input_json_delta":
				if b, ok := blocks[event.Index]; ok {
					b.args.WriteString(event.Delta.PartialJSON)
				}
			}
		case "message_stop":
			scanner.Text() // no-op, just documents intent to stop after this event's already-processed content
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading AI stream: %w", err)
	}

	result := &aiChatMessage{Role: "assistant", Content: contentBuilder.String()}
	for _, idx := range toolOrder {
		b := blocks[idx]
		tc := aiToolCall{ID: b.id, Type: "function"}
		tc.Function.Name = b.name
		tc.Function.Arguments = orEmptyObject(b.args.String())
		result.ToolCalls = append(result.ToolCalls, tc)
	}
	return result, nil
}

// ---- shared HTTP/SSE plumbing ----

// cancelOnClose wraps a response body so the timeout context set up in
// doAIRequest gets cancelled exactly when the caller's own
// defer resp.Body.Close() runs, instead of leaking until the parent ctx
// itself ends.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	c.cancel()
	return c.ReadCloser.Close()
}

// doAIRequest issues the POST common to both providers, differing only in
// which auth header(s) setAuth adds, and returns the still-open response
// body for the caller's own SSE loop (closed by the caller via
// defer resp.Body.Close()).
func doAIRequest(ctx context.Context, url string, body []byte, setAuth func(*http.Request)) (*http.Response, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	setAuth(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("AI request failed: status %d, body: %s", resp.StatusCode, truncate(string(respBody), 500))
	}
	resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// sseDataPayload extracts the payload of one Server-Sent-Events "data:"
// line, shared by both providers' streaming parsers (they differ in what
// the JSON inside means, not in the SSE framing itself). ok is false for
// blank lines, "event:"/"id:" framing lines, and anything else that isn't
// a data line — callers should just skip those.
func sseDataPayload(line string) (payload string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
