package main

// Offline round-trip checks for both wire dialects: a local stub fakes one
// tool-call turn then a final turn, and the tests assert the exact request
// shapes the loop sends. No provider is contacted. These are the same
// assertions that proved the probe implementation on 14 Aug 2026.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func weatherTool(t *testing.T) llmTool {
	t.Helper()
	return llmTool{
		Name:   "get_weather",
		Schema: json.RawMessage(`{"type":"object"}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct{ City string }
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			return "Weather in " + in.City + ": 22C, clear", nil
		},
	}
}

func TestAnthropicDialectRoundTrip(t *testing.T) {
	turns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		turns++
		if turns == 1 {
			ts := body["tools"].([]any)[0].(map[string]any)
			if ts["name"] != "get_weather" || ts["input_schema"] == nil {
				t.Fatalf("bad tools: %v", ts)
			}
			w.Write([]byte(`{"content":[{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Tokyo"}}],"stop_reason":"tool_use"}`))
			return
		}
		// Second turn must carry ONE user message whose content is
		// tool_result blocks.
		msgs := body["messages"].([]any)
		last := msgs[len(msgs)-1].(map[string]any)
		blk := last["content"].([]any)[0].(map[string]any)
		if last["role"] != "user" || blk["type"] != "tool_result" || blk["tool_use_id"] != "tu_1" {
			t.Fatalf("bad tool_result message: %v", last)
		}
		if !strings.Contains(blk["content"].(string), "Tokyo") {
			t.Fatalf("result content: %v", blk["content"])
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"22C and clear."}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	c := &llmClient{HTTP: &http.Client{Timeout: 5 * time.Second}, BaseURL: srv.URL, Model: "m", MaxTokens: 256}
	out, err := runLLMLoop(context.Background(), c.anthropicTurn, "sys", "weather in Tokyo?", []llmTool{weatherTool(t)})
	if err != nil || out != "22C and clear." || turns != 2 {
		t.Fatalf("out=%q err=%v turns=%d", out, err, turns)
	}
}

func TestOpenAIDialectRoundTrip(t *testing.T) {
	turns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		turns++
		if turns == 1 {
			ts := body["tools"].([]any)[0].(map[string]any)
			if ts["type"] != "function" || ts["function"].(map[string]any)["name"] != "get_weather" {
				t.Fatalf("bad tools: %v", ts)
			}
			w.Write([]byte(`{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		// Second turn must carry a role:"tool" message per result, and the
		// echoed assistant message must carry arguments as a JSON string.
		msgs := body["messages"].([]any)
		last := msgs[len(msgs)-1].(map[string]any)
		if last["role"] != "tool" || last["tool_call_id"] != "call_1" {
			t.Fatalf("bad tool message: %v", last)
		}
		prev := msgs[len(msgs)-2].(map[string]any)
		args := prev["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]
		if _, ok := args.(string); !ok {
			t.Fatalf("arguments not a string: %T", args)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"22C and clear."},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := &llmClient{HTTP: &http.Client{Timeout: 5 * time.Second}, BaseURL: srv.URL, Model: "m", MaxTokens: 256}
	out, err := runLLMLoop(context.Background(), c.openaiTurn, "sys", "weather in Tokyo?", []llmTool{weatherTool(t)})
	if err != nil || out != "22C and clear." || turns != 2 {
		t.Fatalf("out=%q err=%v turns=%d", out, err, turns)
	}
}

// TestLoopSurfacesToolErrors: a failing tool becomes an is_error result the
// model can react to, not a dead loop.
func TestLoopSurfacesToolErrors(t *testing.T) {
	turns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		turns++
		if turns == 1 {
			w.Write([]byte(`{"content":[{"type":"tool_use","id":"tu_1","name":"nope","input":{}}],"stop_reason":"tool_use"}`))
			return
		}
		msgs := body["messages"].([]any)
		blk := msgs[len(msgs)-1].(map[string]any)["content"].([]any)[0].(map[string]any)
		if blk["is_error"] != true || !strings.Contains(blk["content"].(string), "unknown tool") {
			t.Fatalf("error not surfaced: %v", blk)
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"understood."}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	c := &llmClient{HTTP: &http.Client{Timeout: 5 * time.Second}, BaseURL: srv.URL, Model: "m", MaxTokens: 256}
	out, err := runLLMLoop(context.Background(), c.anthropicTurn, "", "hi", nil)
	if err != nil || out != "understood." {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
