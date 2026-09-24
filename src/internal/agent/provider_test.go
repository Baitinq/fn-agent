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

func TestOpenAIInputDropsForeignOpaqueReasoning(t *testing.T) {
	input := openAIInput([]historyItem{
		{Type: "reasoning", Provider: "openai", Model: "old", ProviderData: json.RawMessage(`{"id":"foreign-reasoning"}`)},
		{Type: "reasoning", Provider: "anthropic", Model: "claude", Text: "readable reasoning"},
		{Type: "message", Role: "assistant", Provider: "openai", Model: "old", Text: "answer", ProviderData: json.RawMessage(`{"id":"foreign-message"}`)},
	}, "new")
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "foreign-reasoning") || strings.Contains(got, "foreign-message") || !strings.Contains(got, "readable reasoning") || !strings.Contains(got, "answer") {
		t.Fatalf("input = %s", got)
	}
}

func TestGeminiRespondExecutesToolAndPreservesThoughtSignature(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "test-key" {
			t.Errorf("API key = %q", r.URL.Query().Get("key"))
		}
		if r.URL.Path != "/models/gemini-3.7-flash:streamGenerateContent" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			if len(request.Tools) != 1 || request.SystemInstruction == nil {
				t.Errorf("first request = %#v", request)
			}
			fmt.Fprintln(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"repl","args":{"code":"print(6 * 7)"},"id":"call_1"},"thoughtSignature":"signature-1"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"thoughtsTokenCount":3,"totalTokenCount":15}}`)
		case 2:
			encoded, _ := json.Marshal(request)
			body := string(encoded)
			if !strings.Contains(body, `"thoughtSignature":"signature-1"`) {
				t.Errorf("second request omitted thought signature: %s", body)
			}
			if !strings.Contains(body, `"functionResponse":{"name":"repl","response":{"output":"42\n"},"id":"call_1"}`) {
				t.Errorf("second request omitted tool result: %s", body)
			}
			fmt.Fprintln(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"The answer is 42."}]}}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":5,"thoughtsTokenCount":1,"totalTokenCount":26}}`)
		default:
			t.Errorf("unexpected request %d", calls.Load())
		}
	}))
	defer server.Close()

	a := &Agent{provider: "gemini", baseURL: server.URL, httpClient: server.Client(), modelName: "gemini-3.7-flash", reasoningEffort: "high", instructions: "Use the tool.", maxRetries: 0, cwd: t.TempDir()}
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
	if len(a.usage) != 2 || a.usage[0].ReasoningOutputTokens != 3 || a.usage[1].TotalTokens != 26 {
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

func TestGeminiPreservesSignedEmptyParts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"thought":true,"thoughtSignature":"first"},{"thought":true,"thoughtSignature":"second"}]}}]}`)
	}))
	defer server.Close()

	a := &Agent{provider: "gemini", baseURL: server.URL, httpClient: server.Client(), modelName: "gemini-3.7-flash"}
	response, err := a.streamGemini(t.Context(), modelRequest{}, func(ToolEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 2 || response.Items[0].ThoughtSignature != "first" || response.Items[1].ThoughtSignature != "second" {
		t.Fatalf("items = %#v", response.Items)
	}
}

func TestGeminiMarksToolErrors(t *testing.T) {
	contents := geminiContents([]historyItem{{Type: "tool_result", CallID: "call_1", Name: "repl", Text: "failed", ToolError: true}}, "gemini", "model", true)
	response := contents[0].Parts[0].FunctionResponse.Response
	if response["error"] != "failed" || response["output"] != nil {
		t.Fatalf("response = %#v", response)
	}
}

func TestGeminiCancellationDoesNotLeaveModelTurn(t *testing.T) {
	var requests []geminiRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
	}))
	defer server.Close()

	a := &Agent{provider: "gemini", baseURL: server.URL, httpClient: server.Client(), modelName: "gemini-3.7-flash", maxRetries: 0}
	startTestSession(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	a.Respond("cancelled question", nil, func(event ToolEvent) {
		if event.Kind == ToolEventContextTokens {
			cancel()
		}
	}, ctx)

	response := a.Respond("next question", nil, func(ToolEvent) {}, t.Context())
	if response.Err != nil || response.Text != "answer" {
		t.Fatalf("response = %#v", response)
	}
	contents := requests[1].Contents
	if contents[len(contents)-1].Role != "user" {
		t.Fatalf("request ended with %q turn: %#v", contents[len(contents)-1].Role, contents)
	}
}

func TestNewSelectsGeminiProvider(t *testing.T) {
	t.Setenv("FN_PROVIDER", "gemini")
	t.Setenv("FN_MODEL", "gemini-3.7-flash")
	t.Setenv("FN_BASE_URL", "")
	a := mustNewAgent(t)
	if a.provider != "gemini" || a.baseURL != defaultGeminiBaseURL {
		t.Fatalf("provider/base URL = %q, %q", a.provider, a.baseURL)
	}
}

func TestGeminiGatewayUsesBearerHeadersAndOmitsToolCallIDs(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "direct-key")
	t.Setenv("FN_API_KEY", "gateway-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gateway-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("provider") != "google" {
			t.Errorf("provider = %q", r.Header.Get("provider"))
		}
		if r.URL.Query().Has("key") {
			t.Errorf("API key leaked into query: %s", r.URL.RawQuery)
		}
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(request.Contents)
		body := string(encoded)
		if strings.Contains(body, `"id":"call_1"`) {
			t.Errorf("gateway request contains tool call ID: %s", body)
		}
		fmt.Fprintln(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
	}))
	defer server.Close()

	a := &Agent{provider: "gemini", baseURL: server.URL + "/ai-gateway", httpClient: server.Client(), modelName: "gemini-3-flash-preview", reasoningEffort: "high", authHeader: true, headers: map[string]string{"provider": "google"}}
	response, err := a.streamGemini(context.Background(), modelRequest{History: []historyItem{
		{Type: "message", Role: "user", Text: "calculate"},
		{Type: "tool_call", CallID: "call_1", Name: "repl", Arguments: json.RawMessage(`{"code":"print(42)"}`), Provider: "gemini", Model: a.modelName, ThoughtSignature: "signature"},
		{Type: "tool_result", CallID: "call_1", Name: "repl", Text: "42\n"},
	}}, func(ToolEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if response.Text != "done" {
		t.Fatalf("text = %q", response.Text)
	}
}

func TestGeminiCancellationPreservesCompletedToolTurn(t *testing.T) {
	var requests []geminiRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			fmt.Fprintln(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"repl","args":{"code":"import time\ntime.sleep(1)"}}}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
		} else {
			fmt.Fprintln(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
		}
	}))
	defer server.Close()

	a := &Agent{provider: "gemini", baseURL: server.URL, httpClient: server.Client(), modelName: "gemini-3.7-flash", maxRetries: 0}
	startTestSession(t, a)
	a.repl = newPythonREPL(a.callLLM)
	if err := a.repl.start(); err != nil {
		t.Fatal(err)
	}
	defer a.repl.close()

	ctx, cancel := context.WithCancel(t.Context())
	a.Respond("first question", nil, func(event ToolEvent) {
		if event.Kind == ToolEventCall {
			cancel()
		}
	}, ctx)

	response := a.Respond("second question", nil, func(ToolEvent) {}, t.Context())
	if response.Err != nil || response.Text != "answer" {
		t.Fatalf("response = %#v", response)
	}
	if len(requests) != 2 {
		t.Fatalf("requests count = %d, want 2", len(requests))
	}
	contents := requests[1].Contents
	if len(contents) != 3 || contents[1].Role != "model" || contents[1].Parts[0].FunctionCall == nil ||
		contents[2].Role != "user" || contents[2].Parts[0].FunctionResponse == nil || contents[2].Parts[1].Text == "" {
		t.Fatalf("completed tool turn was not preserved after cancellation: %#v", contents)
	}
}

func TestRespondStreamsExactlyTheLimitedToolOutput(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprintln(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"repl","args":{"code":"for _ in range(2000): print('x' * 99)"},"id":"call_1"}}]}}]}`)
			return
		}
		fmt.Fprintln(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]}}]}`)
	}))
	defer server.Close()

	a := &Agent{provider: "gemini", baseURL: server.URL, httpClient: server.Client(), modelName: "gemini-3.7-flash", maxRetries: 0, cwd: t.TempDir()}
	startTestSession(t, a)
	var streamed strings.Builder
	var result string
	response := a.Respond("print", nil, func(event ToolEvent) {
		switch event.Kind {
		case ToolEventUpdate:
			streamed.WriteString(event.Detail)
		case ToolEventResult:
			result = event.Detail
		}
	}, context.Background())
	if response.Err != nil {
		t.Fatal(response.Err)
	}
	want := limitToolOutput(strings.Repeat(strings.Repeat("x", 99)+"\n", 2000))
	if result != want || !strings.HasPrefix(result, streamed.String()+"\n[output truncated") {
		t.Fatalf("streamed %d bytes, result %d bytes", streamed.Len(), len(result))
	}
}
