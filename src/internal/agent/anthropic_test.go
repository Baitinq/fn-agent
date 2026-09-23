package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAnthropicRespondExecutesToolAndPreservesThinkingSignature(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("API key = %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("version = %q", r.Header.Get("anthropic-version"))
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var request anthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			if len(request.Tools) != 1 || request.System == "" || request.Thinking["type"] != "enabled" {
				t.Errorf("first request = %#v", request)
			}
			fmt.Fprintln(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":2}}}`)
			fmt.Fprintln(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
			fmt.Fprintln(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"I should calculate."}}`)
			fmt.Fprintln(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signature-1"}}`)
			fmt.Fprintln(w, `data: {"type":"content_block_stop","index":0}`)
			fmt.Fprintln(w, `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool_1","name":"repl","input":{}}}`)
			fmt.Fprintln(w, `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"code\":\"print(6 * 7)\"}"}}`)
			fmt.Fprintln(w, `data: {"type":"content_block_stop","index":1}`)
			fmt.Fprintln(w, `data: {"type":"message_delta","usage":{"output_tokens":8}}`)
		case 2:
			encoded, _ := json.Marshal(request.Messages)
			body := string(encoded)
			if !strings.Contains(body, `"thinking":"I should calculate.","signature":"signature-1"`) {
				t.Errorf("second request omitted thinking signature: %s", body)
			}
			if !strings.Contains(body, `"type":"tool_result"`) || !strings.Contains(body, `"tool_use_id":"tool_1"`) || !strings.Contains(body, `"content":"42\n"`) {
				t.Errorf("second request omitted tool result: %s", body)
			}
			fmt.Fprintln(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":20}}}`)
			fmt.Fprintln(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			fmt.Fprintln(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The answer is 42."}}`)
			fmt.Fprintln(w, `data: {"type":"content_block_stop","index":0}`)
			fmt.Fprintln(w, `data: {"type":"message_delta","usage":{"output_tokens":5}}`)
		default:
			t.Errorf("unexpected request %d", calls.Load())
		}
	}))
	defer server.Close()

	a := &Agent{provider: "anthropic", baseURL: server.URL, httpClient: server.Client(), modelName: "claude-test", reasoningEffort: "high", instructions: "Use the tool.", maxRetries: 0, cwd: t.TempDir()}
	startTestSession(t, a)
	var events []ToolEvent
	response := a.Respond("calculate", nil, func(event ToolEvent) { events = append(events, event) }, context.Background())
	if response.Err != nil {
		t.Fatal(response.Err)
	}
	if response.Text != "The answer is 42." {
		t.Fatalf("text = %q", response.Text)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests = %d", calls.Load())
	}
	if len(a.usage) != 2 || a.usage[0].InputTokens != 12 || a.usage[1].TotalTokens != 25 {
		t.Fatalf("usage = %#v", a.usage)
	}
	foundCall, foundResult := false, false
	for _, event := range events {
		foundCall = foundCall || event.Kind == ToolEventCall
		foundResult = foundResult || event.Kind == ToolEventResult && event.Detail == "42\n"
	}
	if !foundCall || !foundResult {
		t.Fatalf("events = %#v", events)
	}
}

func TestAnthropicHistoryCompatibility(t *testing.T) {
	messages := anthropicMessages([]historyItem{
		{Type: "reasoning", Text: "unsigned"},
		{Type: "reasoning", Provider: "anthropic", Model: "model", ThoughtSignature: "empty-signature"},
		{Type: "reasoning", Provider: "anthropic", Model: "model", RedactedThinking: "opaque"},
		{Type: "tool_call", CallID: "call.bad/id", Name: "repl", Arguments: json.RawMessage(`{}`)},
		{Type: "tool_result", CallID: "call.bad/id", Text: "failed", ToolError: true},
	}, "model")
	encoded, _ := json.Marshal(messages)
	body := string(encoded)
	for _, want := range []string{`"type":"text","text":"unsigned"`, `"type":"thinking","thinking":"","signature":"empty-signature"`, `"type":"redacted_thinking","data":"opaque"`, `"id":"callbadid"`, `"tool_use_id":"callbadid"`, `"is_error":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("messages omitted %s: %s", want, body)
		}
	}
}

func TestNewSelectsAnthropicProvider(t *testing.T) {
	t.Setenv("FN_PROVIDER", "anthropic")
	t.Setenv("FN_MODEL", "claude-sonnet-4-20250514")
	t.Setenv("FN_BASE_URL", "")
	a := mustNewAgent(t)
	if a.provider != "anthropic" || a.baseURL != defaultAnthropicBaseURL {
		t.Fatalf("provider/base URL = %q, %q", a.provider, a.baseURL)
	}
}

func TestAnthropicAdaptiveThinking(t *testing.T) {
	if !anthropicAdaptiveThinking("claude-sonnet-5") || !anthropicAdaptiveThinking("claude-opus-4-8-20260801") {
		t.Fatal("current models should use adaptive thinking")
	}
	if anthropicAdaptiveThinking("claude-sonnet-4-5-20250929") {
		t.Fatal("older models should use budget thinking")
	}
}
