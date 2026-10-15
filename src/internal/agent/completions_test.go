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

func TestCompletionsRespondExecutesToolAndPreservesReasoning(t *testing.T) {
	t.Setenv("FN_API_KEY", "gateway-token")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer gateway-token" || r.Header.Get("provider") != "baseten" {
			t.Errorf("headers = %#v", r.Header)
		}
		var request completionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			if request.Model != "zai-org/GLM-5.3" || len(request.Tools) != 1 || request.ReasoningEffort != "high" || request.Messages[0].Role != "system" {
				t.Errorf("first request = %#v", request)
			}
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"reasoning_content":"I should calculate."}}]}`)
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"repl","arguments":"{\"code\":"}}]}}]}`)
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"print(6 * 7)\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18,"completion_tokens_details":{"reasoning_tokens":3}}}`)
			fmt.Fprintln(w, "data: [DONE]")
		case 2:
			encoded, _ := json.Marshal(request.Messages)
			body := string(encoded)
			for _, want := range []string{`"reasoning_content":"I should calculate."`, `"id":"call_1"`, `"tool_call_id":"call_1"`, `"content":"42\n"`} {
				if !strings.Contains(body, want) {
					t.Errorf("second request omitted %s: %s", want, body)
				}
			}
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"The answer is "}}]}`)
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"42."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`)
			fmt.Fprintln(w, "data: [DONE]")
		default:
			t.Errorf("unexpected request %d", calls.Load())
		}
	}))
	defer server.Close()

	a := &Agent{provider: "openai-completions", baseURL: server.URL + "/v1", httpClient: server.Client(), headers: map[string]string{"provider": "baseten"}, modelName: "zai-org/GLM-5.3", reasoningEffort: "high", instructions: "Use the tool.", maxRetries: 0, cwd: t.TempDir()}
	startTestSession(t, a)
	var events []ToolEvent
	response := a.Respond("calculate", nil, func(event ToolEvent) { events = append(events, event) }, context.Background())
	if response.Err != nil {
		t.Fatal(response.Err)
	}
	if response.Text != "The answer is 42." || calls.Load() != 2 {
		t.Fatalf("response = %#v, requests = %d", response, calls.Load())
	}
	if len(a.usage) != 2 || a.usage[0].ReasoningOutputTokens != 3 || a.usage[1].TotalTokens != 25 {
		t.Fatalf("usage = %#v", a.usage)
	}
}

func TestCompletionMessagesDoNotReuseOtherProviderReasoning(t *testing.T) {
	messages := completionMessages("", []historyItem{
		{Type: "reasoning", Text: "private", Provider: "openai", Model: "other"},
		{Type: "message", Role: "assistant", Text: "answer"},
		{Type: "tool_call", CallID: "call_1", Name: "repl", Arguments: json.RawMessage(`{"code":"print(1)"}`)},
		{Type: "tool_result", CallID: "call_1", Name: "repl", Text: "1\n"},
	}, "model")
	encoded, _ := json.Marshal(messages)
	body := string(encoded)
	if strings.Contains(body, "private") || !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"role":"tool"`) {
		t.Fatalf("messages = %s", body)
	}
}

func TestCompletionsRejectsIncompleteStreams(t *testing.T) {
	toolCall := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"repl","arguments":"{\"code\":\"print("}}]}}]}`
	for name, frames := range map[string][]string{
		"eof mid tool call":    {toolCall},
		"length mid tool call": {toolCall, `data: {"choices":[{"delta":{},"finish_reason":"length"}]}`, `data: [DONE]`},
	} {
		t.Run(name, func(t *testing.T) {
			server := sseServer(t, frames...)
			a := &Agent{provider: "openai-completions", baseURL: server.URL, httpClient: server.Client(), modelName: "local-model"}
			response, err := a.streamCompletions(context.Background(), modelRequest{}, func(ToolEvent) {})
			if err == nil || !isRetryableLLMError(err) {
				t.Fatalf("response = %+v, err = %v", response, err)
			}
		})
	}
}
