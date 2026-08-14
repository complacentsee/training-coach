package main

// A hand-rolled, stdlib-only agentic tool loop speaking both wire dialects:
// Anthropic Messages (/v1/messages) and OpenAI Chat Completions
// (/v1/chat/completions). The latter covers Ollama's OpenAI-compatible
// endpoint; Ollama ≥ 0.14.0 also exposes an Anthropic-compatible
// /v1/messages, so either adapter can front a local model. Two dialects
// cover the three provider classes — Anthropic's own OpenAI-compat layer is
// documented test-grade and is deliberately not built on.
//
// Design: a neutral transcript (llmMsg/llmToolCall/llmToolResult) that each
// dialect serialises on the way out and parses on the way in. The loop is
// dialect-blind: it calls a turn function, executes tool calls, appends
// results. Wire shapes are pinned by the offline httptest round-trips in
// llm_test.go — the same tests that proved the probe implementation.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type llmTool struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema for the input object
	Run         func(ctx context.Context, args json.RawMessage) (string, error)
}

type llmToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

type llmToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// llmMsg is one neutral transcript entry.
// Role "user": Text. Role "assistant": Text and/or Calls.
// Role "tool": Results (rendered per-dialect: one user message of
// tool_result blocks for Anthropic; one role:"tool" message per result for
// OpenAI).
type llmMsg struct {
	Role    string
	Text    string
	Calls   []llmToolCall
	Results []llmToolResult
}

// llmTurn sends the transcript and returns the assistant's reply plus the
// provider's stop reason ("tool_use"/"tool_calls" means keep going).
type llmTurn func(ctx context.Context, system string, msgs []llmMsg, tools []llmTool) (llmMsg, string, error)

type llmClient struct {
	HTTP      *http.Client
	BaseURL   string // host only, e.g. https://api.anthropic.com or http://localhost:11434
	Key       string
	Model     string
	MaxTokens int
}

func (c *llmClient) post(ctx context.Context, path string, hdr map[string]string, body any) ([]byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s: %s", path, resp.Status, bytes.TrimSpace(out))
	}
	return out, nil
}

/* ── Anthropic Messages dialect ────────────────────────────────────────── */

func (c *llmClient) anthropicTurn(ctx context.Context, system string, msgs []llmMsg, tools []llmTool) (llmMsg, string, error) {
	type tool struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	type block map[string]any
	type wireMsg struct {
		Role    string  `json:"role"`
		Content []block `json:"content"`
	}

	req := struct {
		Model     string    `json:"model"`
		MaxTokens int       `json:"max_tokens"`
		System    string    `json:"system,omitempty"`
		Messages  []wireMsg `json:"messages"`
		Tools     []tool    `json:"tools,omitempty"`
	}{Model: c.Model, MaxTokens: c.MaxTokens, System: system}

	for _, t := range tools {
		req.Tools = append(req.Tools, tool{t.Name, t.Description, t.Schema})
	}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			req.Messages = append(req.Messages, wireMsg{"user", []block{{"type": "text", "text": m.Text}}})
		case "assistant":
			var bs []block
			if m.Text != "" {
				bs = append(bs, block{"type": "text", "text": m.Text})
			}
			for _, tc := range m.Calls {
				bs = append(bs, block{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": tc.Args})
			}
			req.Messages = append(req.Messages, wireMsg{"assistant", bs})
		case "tool": // ALL results go in ONE user message of tool_result blocks
			var bs []block
			for _, r := range m.Results {
				bs = append(bs, block{"type": "tool_result", "tool_use_id": r.CallID,
					"content": r.Content, "is_error": r.IsError})
			}
			req.Messages = append(req.Messages, wireMsg{"user", bs})
		}
	}

	raw, err := c.post(ctx, "/v1/messages", map[string]string{
		"x-api-key": c.Key, "anthropic-version": "2023-06-01",
	}, req)
	if err != nil {
		return llmMsg{}, "", err
	}

	var resp struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return llmMsg{}, "", fmt.Errorf("decode /v1/messages: %w", err)
	}
	out := llmMsg{Role: "assistant"}
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			out.Text += b.Text
		case "tool_use":
			out.Calls = append(out.Calls, llmToolCall{b.ID, b.Name, b.Input})
		}
	}
	return out, resp.StopReason, nil
}

/* ── OpenAI Chat Completions dialect ───────────────────────────────────── */

func (c *llmClient) openaiTurn(ctx context.Context, system string, msgs []llmMsg, tools []llmTool) (llmMsg, string, error) {
	type fn struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	type tool struct {
		Type     string `json:"type"` // "function"
		Function fn     `json:"function"`
	}
	type call struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"` // JSON *string*, not object
		} `json:"function"`
	}
	type wireMsg struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCalls  []call `json:"tool_calls,omitempty"`
		ToolCallID string `json:"tool_call_id,omitempty"`
	}

	req := struct {
		Model    string    `json:"model"`
		Messages []wireMsg `json:"messages"`
		Tools    []tool    `json:"tools,omitempty"`
	}{Model: c.Model}

	if system != "" {
		req.Messages = append(req.Messages, wireMsg{Role: "system", Content: system})
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, tool{"function", fn{t.Name, t.Description, t.Schema}})
	}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			req.Messages = append(req.Messages, wireMsg{Role: "user", Content: m.Text})
		case "assistant":
			w := wireMsg{Role: "assistant", Content: m.Text}
			for _, tc := range m.Calls {
				var cl call
				cl.ID, cl.Type = tc.ID, "function"
				cl.Function.Name, cl.Function.Arguments = tc.Name, string(tc.Args)
				w.ToolCalls = append(w.ToolCalls, cl)
			}
			req.Messages = append(req.Messages, w)
		case "tool": // one role:"tool" message PER result
			for _, r := range m.Results {
				content := r.Content
				if r.IsError { // no is_error flag in this dialect
					content = "ERROR: " + content
				}
				req.Messages = append(req.Messages, wireMsg{Role: "tool", Content: content, ToolCallID: r.CallID})
			}
		}
	}

	raw, err := c.post(ctx, "/v1/chat/completions", map[string]string{
		"Authorization": "Bearer " + c.Key,
	}, req)
	if err != nil {
		return llmMsg{}, "", err
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Refusal   string `json:"refusal"`
				ToolCalls []call `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return llmMsg{}, "", fmt.Errorf("decode /chat/completions: %w", err)
	}
	if len(resp.Choices) == 0 {
		return llmMsg{}, "", fmt.Errorf("no choices in response")
	}
	ch := resp.Choices[0]
	if ch.Message.Refusal != "" {
		return llmMsg{Role: "assistant", Text: ch.Message.Refusal}, "refusal", nil
	}
	out := llmMsg{Role: "assistant", Text: ch.Message.Content}
	for _, tc := range ch.Message.ToolCalls {
		out.Calls = append(out.Calls, llmToolCall{tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments)})
	}
	return out, ch.FinishReason, nil
}

/* ── the loop ──────────────────────────────────────────────────────────── */

// llmLoopTurns caps the tool loop. A grade needs prescription + metrics +
// context + post — four calls on the happy path; ten is headroom, not a
// target.
const llmLoopTurns = 10

func runLLMLoop(ctx context.Context, turn llmTurn, system, prompt string, tools []llmTool) (string, error) {
	byName := make(map[string]*llmTool, len(tools))
	for i := range tools {
		byName[tools[i].Name] = &tools[i]
	}
	msgs := []llmMsg{{Role: "user", Text: prompt}}
	for i := 0; i < llmLoopTurns; i++ {
		reply, reason, err := turn(ctx, system, msgs, tools)
		if err != nil {
			return "", err
		}
		msgs = append(msgs, reply)
		if len(reply.Calls) == 0 || (reason != "tool_use" && reason != "tool_calls") {
			return reply.Text, nil // end_turn/stop/refusal/max_tokens all land here
		}
		res := llmMsg{Role: "tool"}
		for _, tc := range reply.Calls {
			var out string
			var err error
			if tl := byName[tc.Name]; tl != nil {
				out, err = tl.Run(ctx, tc.Args)
			} else {
				err = fmt.Errorf("unknown tool %q", tc.Name)
			}
			if err != nil {
				res.Results = append(res.Results, llmToolResult{tc.ID, err.Error(), true})
			} else {
				res.Results = append(res.Results, llmToolResult{tc.ID, out, false})
			}
		}
		msgs = append(msgs, res)
	}
	return "", fmt.Errorf("tool loop did not converge in %d turns", llmLoopTurns)
}
