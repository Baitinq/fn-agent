package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func mustNewAgent(t *testing.T) *Agent {
	t.Helper()
	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestNewUsesEnvironmentOverrides(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/v1/responses" {
			t.Errorf("request path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":\"override-model\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()

	t.Setenv("FN_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("FN_BASE_URL", server.URL+"/custom/v1/")
	t.Setenv("FN_MODEL", "override-model")
	t.Setenv("FN_REASONING_EFFORT", "high")

	a := mustNewAgent(t)
	startTestSession(t, a)
	if a.ModelName() != "override-model" || a.ReasoningEffort() != "high" {
		t.Fatalf("configuration = model %q, reasoning %q", a.ModelName(), a.ReasoningEffort())
	}
	if resp := a.Respond("hello", nil, func(ToolEvent) {}, t.Context()); resp.Err != nil {
		t.Fatal(resp.Err)
	}
	if request["model"] != "override-model" {
		t.Fatalf("request model = %v", request["model"])
	}
	tools, ok := request["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("request tools = %#v", request["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["name"] != "repl" {
		t.Fatalf("request tool = %#v", tools[0])
	}
	reasoning, ok := request["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" {
		t.Fatalf("request reasoning = %#v", request["reasoning"])
	}
}

func TestCallLLMUsesFreshToolFreeRequest(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_llm\",\"object\":\"response\",\"model\":\"test\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_llm\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"classified\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":40,\"input_tokens_details\":{\"cached_tokens\":10},\"output_tokens\":2,\"output_tokens_details\":{\"reasoning_tokens\":1},\"total_tokens\":42}}}\n\n")
	}))
	defer server.Close()

	a := &Agent{
		client:          openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0)),
		modelName:       "test-model",
		reasoningEffort: "low",
	}
	text, err := a.callLLM(t.Context(), "classify this")
	if err != nil || text != "classified" {
		t.Fatalf("callLLM() = %q, %v", text, err)
	}
	if a.TokensUsed() != 42 {
		t.Fatalf("TokensUsed() = %d, want 42", a.TokensUsed())
	}
	usage := a.Usage()
	if len(usage) != 1 || usage[0].InputTokens != 40 || usage[0].CachedInputTokens != 10 || usage[0].OutputTokens != 2 || usage[0].ReasoningOutputTokens != 1 || usage[0].TotalTokens != 42 {
		t.Fatalf("Usage() = %#v", usage)
	}
	if _, ok := request["tools"]; ok {
		t.Fatalf("llm request included tools: %#v", request["tools"])
	}
	input := request["input"].([]any)
	message := input[0].(map[string]any)
	if message["content"] != "classify this" {
		t.Fatalf("llm input = %#v", request["input"])
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	prompt := buildSystemPrompt("/work/project")
	for _, want := range []string{
		"expert general-purpose assistant operating inside fn agent",
		"Available tool:",
		"repl: Execute Python in a persistent REPL",
		"shell(command, timeout=None) -> ShellResult",
		"web_search(query, max_results=8) -> list[SearchResult]",
		"llm(prompt) -> str",
		"Only printed output and the final expression enter model context",
		"Old REPL outputs are replaced with [tool output omitted after use] after each turn; Python state persists",
		"Host functions are async and must be awaited",
		"Use asyncio.gather() for independent calls",
		"Do not create detached background tasks",
		"mcporter@latest list",
		"mcporter@latest call <server>.<tool>",
		"https://github.com/Baitinq/fn-agent",
		"~/.fn/sessions/<UUID>/session.jsonl",
		"fn --session <UUID>",
		"Prioritize fast, verifiable iteration",
		"exercise changes end to end in the local environment",
		"Use the fastest relevant feedback loop while developing",
		"Do not run destructive or difficult-to-reverse commands",
		"Current working directory: /work/project",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("system prompt does not contain %q", want)
		}
	}
}

func TestPrefixUserMessageIncludesCurrentDateAndTime(t *testing.T) {
	now := time.Date(2026, time.August, 23, 19, 42, 17, 0, time.FixedZone("PDT", -7*60*60))
	got := prefixUserMessage("hello", now)
	want := "[2026-08-23T19:42:17-07:00]\n\nhello"
	if got != want {
		t.Fatalf("prefixed message = %q, want %q", got, want)
	}
}

func TestConsumeSteeringDeliversOneMessageAtATime(t *testing.T) {
	a := &Agent{}
	startTestSession(t, a)
	steer := make(chan string, 2)
	steer <- "first"
	steer <- "second"
	var events []ToolEvent
	emit := func(event ToolEvent) { events = append(events, event) }

	if consumed, err := a.consumeSteering(steer, emit); err != nil || !consumed {
		t.Fatal("first steering message was not consumed")
	}
	if len(a.history) != 1 || len(events) != 1 || events[0].Detail != "first" {
		t.Fatalf("first delivery: history=%d events=%#v", len(a.history), events)
	}
	if consumed, err := a.consumeSteering(steer, emit); err != nil || !consumed {
		t.Fatal("second steering message was not consumed")
	}
	if len(a.history) != 2 || len(events) != 2 || events[1].Detail != "second" {
		t.Fatalf("second delivery: history=%d events=%#v", len(a.history), events)
	}
	if consumed, err := a.consumeSteering(steer, emit); err != nil || consumed {
		t.Fatal("empty steering queue reported a message")
	}
}

func TestPruneTransientHistoryKeepsConversationAndREPLCalls(t *testing.T) {
	a := &Agent{history: []historyItem{
		{Type: "message", Role: "user", Text: "first"},
		{Type: "message", Role: "assistant", Text: "prior"},
		{Type: "message", Role: "user", Text: "second"},
		{Type: "reasoning", Text: "thinking"},
		{Type: "message", Role: "assistant", Text: "intermediate", transient: true},
		{Type: "tool_call", CallID: "call_1", Name: "repl"},
		{Type: "tool_result", CallID: "call_1", Text: "result"},
		{Type: "message", Role: "assistant", Text: "final"},
	}}
	a.pruneTransientHistory()
	if len(a.history) != 6 {
		t.Fatalf("retained history = %#v", a.history)
	}
	if a.history[0].Text != "first" || a.history[1].Text != "prior" || a.history[3].Type != "tool_call" || a.history[4].Text != OmittedToolResult || a.history[5].Text != "final" {
		t.Fatalf("retained history = %#v", a.history)
	}
}

func TestLimitToolOutputKeepsTail(t *testing.T) {
	var full strings.Builder
	for i := 1; i <= maxToolOutputLines+10; i++ {
		fmt.Fprintf(&full, "line-%04d\n", i)
	}
	limited := limitToolOutput(full.String())
	if strings.Contains(limited, "line-0001") || !strings.Contains(limited, "line-2010") {
		t.Fatalf("limited output did not retain the tail: prefix=%q suffix=%q", limited[:min(100, len(limited))], limited[max(0, len(limited)-100):])
	}
	if !strings.Contains(limited, "Tool output truncated:") || !strings.Contains(limited, "Assign large results to a Python variable") {
		t.Fatalf("limited output lacks truncation guidance: %q", limited[max(0, len(limited)-300):])
	}
}

func TestLimitToolOutputRespectsByteLimitAndUTF8(t *testing.T) {
	full := strings.Repeat("é", maxToolOutputBytes)
	limited := limitToolOutput(full)
	if !utf8.ValidString(limited) {
		t.Fatal("byte-limited output is not valid UTF-8")
	}
	if !strings.Contains(limited, "50KB limit") {
		t.Fatalf("byte limit was not reported: %q", limited[max(0, len(limited)-200):])
	}
}

func TestDefaultLLMRetryBudget(t *testing.T) {
	if got := mustNewAgent(t).maxRetries; got != 10 {
		t.Fatalf("max retries = %d, want 10", got)
	}
}

func TestIsRetryableLLMError(t *testing.T) {
	for _, status := range []int{408, 409, 429, 500, 503} {
		if !isRetryableLLMError(&openai.Error{StatusCode: status}) {
			t.Errorf("status %d was not retryable", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404} {
		if isRetryableLLMError(&openai.Error{StatusCode: status}) {
			t.Errorf("status %d was retryable", status)
		}
	}
	missingToolOutput := &openai.Error{
		StatusCode: http.StatusBadRequest,
		Message:    "No tool output found for function call call_test.",
	}
	if isRetryableLLMError(missingToolOutput) {
		t.Fatal("missing tool output error was retryable")
	}
	if isRetryableLLMError(&openai.Error{StatusCode: 429, Code: "insufficient_quota"}) {
		t.Fatal("quota exhaustion was retryable")
	}
	if !isRetryableLLMError(fmt.Errorf("dial tcp: network is unreachable")) {
		t.Fatal("transport error was not retryable")
	}
	if isRetryableLLMError(context.Canceled) {
		t.Fatal("cancellation was retryable")
	}
}

func TestIsContextOverflowError(t *testing.T) {
	for _, err := range []error{
		&openai.Error{StatusCode: http.StatusBadRequest, Code: "context_length_exceeded"},
		&responseFailure{message: "Maximum context length exceeded"},
		fmt.Errorf("input too long for model"),
	} {
		if !isContextOverflowError(err) {
			t.Errorf("did not recognize context overflow: %v", err)
		}
	}
	if isContextOverflowError(&openai.Error{StatusCode: http.StatusBadRequest, Message: "invalid argument"}) {
		t.Fatal("ordinary bad request was recognized as context overflow")
	}
}

func TestRespondRetriesTransientFailures(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests <= 2 {
			http.Error(w, `{"error":{"message":"temporarily unavailable","type":"server_error","code":"server_error"}}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\",\"item_id\":\"msg_test\",\"output_index\":0,\"content_index\":0,\"sequence_number\":1}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":\"test\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\",\"annotations\":[]}]}]}}\n\n")
	}))
	defer server.Close()

	a := &Agent{
		client:         openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0)),
		modelName:      "test",
		instructions:   "test",
		maxRetries:     3,
		retryBaseDelay: time.Millisecond,
		retryJitter:    func() float64 { return 1 },
	}
	startTestSession(t, a)
	var events []ToolEvent
	resp := a.Respond("hello", make(chan string), func(ev ToolEvent) { events = append(events, ev) }, t.Context())
	if resp.Err != nil || resp.Text != "hello" {
		t.Fatalf("response = %#v", resp)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
	var retries []ToolEvent
	for _, ev := range events {
		if ev.Kind == ToolEventRetry {
			retries = append(retries, ev)
		}
	}
	if len(retries) != 2 || retries[0].Attempt != 1 || retries[0].Delay != time.Millisecond || retries[1].Attempt != 2 || retries[1].Delay != 2*time.Millisecond {
		t.Fatalf("retry events = %#v", retries)
	}
}

func TestRespondCancellationDoesNotLeaveFunctionCallWithoutOutput(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)

		var output []any
		if len(requests) == 1 {
			output = []any{map[string]any{
				"id": "fc_test", "type": "function_call", "call_id": "call_test",
				"name": "repl", "arguments": `{"code":"print('test')"}`, "status": "completed",
			}}
		} else {
			output = []any{map[string]any{
				"id": "msg_test", "type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "recovered", "annotations": []any{}}},
			}}
		}
		payload, _ := json.Marshal(map[string]any{
			"type": "response.completed", "sequence_number": 1,
			"response": map[string]any{
				"id": "resp_test", "object": "response", "model": "test", "status": "completed", "output": output,
			},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", payload)
	}))
	defer server.Close()

	a := &Agent{
		client:       openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0)),
		modelName:    "test",
		instructions: "test",
		maxRetries:   0,
	}
	startTestSession(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	a.Respond("cancelled question", nil, func(event ToolEvent) {
		if event.Kind == ToolEventContextTokens {
			cancel()
		}
	}, ctx)

	response := a.Respond("next question", nil, func(ToolEvent) {}, t.Context())
	if response.Err != nil || response.Text != "recovered" {
		t.Fatalf("response = %#v", response)
	}
	input, _ := json.Marshal(requests[1]["input"])
	if strings.Contains(string(input), "function_call") {
		t.Fatalf("cancelled function call remained in history: %s", input)
	}
}

func TestRespondPrunesToolResultsAfterEachModelTurn(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, request)

		var output []any
		switch len(requests) {
		case 1:
			output = []any{map[string]any{
				"id": "fc_test", "type": "function_call", "call_id": "call_test",
				"name": "repl", "arguments": `{"code":"saved = ''.join(map(chr, [83, 69, 67, 82, 69, 84])); print(saved)"}`, "status": "completed",
			}}
		case 2:
			output = []any{map[string]any{
				"id": "fc_second", "type": "function_call", "call_id": "call_second",
				"name": "repl", "arguments": `{"code":"print(''.join(map(chr, [83, 69, 67, 79, 78, 68])))"}`, "status": "completed",
			}}
		case 3:
			output = []any{map[string]any{
				"id": "msg_first", "type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "first answer", "annotations": []any{}}},
			}}
		case 4:
			output = []any{map[string]any{
				"id": "msg_second", "type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "second answer", "annotations": []any{}}},
			}}
		}
		payload, _ := json.Marshal(map[string]any{
			"type": "response.completed", "sequence_number": 1,
			"response": map[string]any{
				"id": "resp_test", "object": "response", "model": "test", "status": "completed", "output": output,
			},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", payload)
	}))
	defer server.Close()

	a := &Agent{
		client:       openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0)),
		modelName:    "test",
		instructions: "test",
		maxRetries:   0,
	}
	startTestSession(t, a)
	defer a.Close()
	if response := a.Respond("first question", nil, func(ToolEvent) {}, t.Context()); response.Err != nil {
		t.Fatal(response.Err)
	}
	if response := a.Respond("second question", nil, func(ToolEvent) {}, t.Context()); response.Err != nil {
		t.Fatal(response.Err)
	}

	if len(requests) != 4 {
		t.Fatalf("requests = %d, want 4", len(requests))
	}
	firstToolTurn, _ := json.Marshal(requests[1]["input"])
	if !strings.Contains(string(firstToolTurn), "function_call") || !strings.Contains(string(firstToolTurn), "SECRET") {
		t.Fatalf("first tool turn did not include its complete result: %s", firstToolTurn)
	}
	secondToolTurn, _ := json.Marshal(requests[2]["input"])
	if strings.Contains(string(secondToolTurn), "SECRET") || !strings.Contains(string(secondToolTurn), OmittedToolResult) || !strings.Contains(string(secondToolTurn), "SECOND") {
		t.Fatalf("second tool turn did not prune only the consumed result: %s", secondToolTurn)
	}
	laterTurn, _ := json.Marshal(requests[3]["input"])
	if !strings.Contains(string(laterTurn), "function_call") || !strings.Contains(string(laterTurn), "saved =") {
		t.Fatalf("later turn omitted the REPL code cell: %s", laterTurn)
	}
	if strings.Contains(string(laterTurn), "SECRET") || strings.Contains(string(laterTurn), "SECOND") || !strings.Contains(string(laterTurn), OmittedToolResult) {
		t.Fatalf("later turn did not replace the REPL results: %s", laterTurn)
	}
	for _, text := range []string{"first question", "first answer", "second question"} {
		if !strings.Contains(string(laterTurn), text) {
			t.Fatalf("later turn omitted %q: %s", text, laterTurn)
		}
	}
}

func TestREPLCheckpointUndoResumeEndToEnd(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, request)

		var output []any
		switch len(requests) {
		case 1:
			output = []any{map[string]any{
				"id": "fc_first", "type": "function_call", "call_id": "call_first",
				"name": "repl", "arguments": `{"code":"value = 1"}`, "status": "completed",
			}}
		case 2:
			output = testAssistantOutput("msg_first", "set to one")
		case 3:
			output = []any{map[string]any{
				"id": "fc_second", "type": "function_call", "call_id": "call_second",
				"name": "repl", "arguments": `{"code":"value = 2; added = True"}`, "status": "completed",
			}}
		case 4:
			output = testAssistantOutput("msg_second", "set to two")
		case 5:
			output = []any{map[string]any{
				"id": "fc_inspect", "type": "function_call", "call_id": "call_inspect",
				"name": "repl", "arguments": `{"code":"value, 'added' in globals()"}`, "status": "completed",
			}}
		case 6:
			input, _ := json.Marshal(request["input"])
			if !strings.Contains(string(input), "(1, False)") {
				t.Errorf("model did not observe restored REPL state: %s", input)
			}
			output = testAssistantOutput("msg_inspect", "state is restored")
		default:
			t.Errorf("unexpected request %d", len(requests))
		}
		payload, _ := json.Marshal(map[string]any{
			"type": "response.completed", "sequence_number": 1,
			"response": map[string]any{
				"id": "resp_test", "object": "response", "model": "test", "status": "completed", "output": output,
			},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", payload)
	}))
	defer server.Close()

	root, cwd := t.TempDir(), t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	a := &Agent{
		client:       openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0)),
		modelName:    "test",
		provider:     "openai",
		instructions: "test",
		cwd:          cwd,
		maxRetries:   0,
	}
	if err := a.StartSession(id, root); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"set one", "change it"} {
		if response := a.Respond(prompt, nil, func(ToolEvent) {}, t.Context()); response.Err != nil {
			t.Fatal(response.Err)
		}
		if err := a.SaveSession(); err != nil {
			t.Fatal(err)
		}
	}
	if text, err := a.Undo(0); err != nil || text != "change it" {
		t.Fatalf("Undo() = %q, %v", text, err)
	}
	if response := a.Respond("inspect it", nil, func(ToolEvent) {}, t.Context()); response.Err != nil {
		t.Fatal(response.Err)
	}
	if err := a.SaveSession(); err != nil {
		t.Fatal(err)
	}
	a.Close()

	loaded := &Agent{cwd: cwd, provider: "openai", modelName: "test"}
	defer loaded.Close()
	if err := loaded.ResumeSession(id, root); err != nil {
		t.Fatal(err)
	}
	output, failed, err := loaded.pythonREPL().execute(t.Context(), "value, 'added' in globals()")
	if err != nil || failed || output != "(1, False)" {
		t.Fatalf("resumed state = %q, failed %v, err %v", output, failed, err)
	}
	if len(requests) != 6 {
		t.Fatalf("requests = %d, want 6", len(requests))
	}
}

func testAssistantOutput(id, text string) []any {
	return []any{map[string]any{
		"id": id, "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}}
}

func TestRetryDelayUsesEqualJitterAndCapsBackoff(t *testing.T) {
	a := &Agent{
		retryBaseDelay: 2 * time.Second,
		retryJitter:    func() float64 { return 0 },
	}
	if got := a.retryDelay(0); got != time.Second {
		t.Fatalf("first retry delay = %s, want 1s", got)
	}
	if got := a.retryDelay(10); got != 15*time.Second {
		t.Fatalf("capped retry delay = %s, want 15s", got)
	}

	a.retryJitter = func() float64 { return 1 }
	if got := a.retryDelay(0); got != 2*time.Second {
		t.Fatalf("maximum first retry delay = %s, want 2s", got)
	}
	if got := a.retryDelay(10); got != maxRetryDelay {
		t.Fatalf("maximum capped retry delay = %s, want %s", got, maxRetryDelay)
	}
}

func TestWaitForRetryCanBeCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started := time.Now()
	if waitForRetry(ctx, time.Hour) {
		t.Fatal("cancelled wait completed")
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("cancelled retry wait did not return promptly")
	}
}

func TestLoadContextFilesWalksAncestorsInOrder(t *testing.T) {
	root := t.TempDir()
	project := root + "/project"
	cwd := project + "/service"
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/AGENTS.md", []byte("root guidance"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project+"/AGENTS.md", []byte("project guidance"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwd+"/CLAUDE.md", []byte("service guidance"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := loadContextFiles(cwd)
	if len(files) != 3 {
		t.Fatalf("loaded files = %#v", files)
	}
	for i, want := range []string{"root guidance", "project guidance", "service guidance"} {
		if files[i].content != want {
			t.Fatalf("file %d content = %q, want %q", i, files[i].content, want)
		}
	}

	prompt := buildSystemPrompt(cwd)
	rootIndex := strings.Index(prompt, "root guidance")
	projectIndex := strings.Index(prompt, "project guidance")
	serviceIndex := strings.Index(prompt, "service guidance")
	if rootIndex < 0 || rootIndex >= projectIndex || projectIndex >= serviceIndex {
		t.Fatalf("instructions are not ordered broadest to most specific: %q", prompt)
	}
}

func TestLoadContextFilesPrefersAgents(t *testing.T) {
	cwd := t.TempDir()
	for name, content := range map[string]string{
		"AGENTS.md": "agents guidance",
		"CLAUDE.md": "claude guidance",
	} {
		if err := os.WriteFile(cwd+"/"+name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	files := loadContextFiles(cwd)
	if len(files) == 0 || files[len(files)-1].content != "agents guidance" {
		t.Fatalf("loaded files = %#v", files)
	}
	prompt := buildSystemPrompt(cwd)
	if !strings.Contains(prompt, "agents guidance") || strings.Contains(prompt, "claude guidance") {
		t.Fatalf("AGENTS.md selection was not reflected in prompt: %q", prompt)
	}
}

func TestPythonREPLPersistsStateAndExposesShell(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)

	output, failed, err := repl.execute(t.Context(), "value = 40")
	if err != nil || failed || output != "" {
		t.Fatalf("assignment result = %q, failed=%v, error=%v", output, failed, err)
	}
	output, failed, err = repl.execute(t.Context(), "value + 2")
	if err != nil || failed || output != "42" {
		t.Fatalf("persistent result = %q, failed=%v, error=%v", output, failed, err)
	}
	output, failed, err = repl.execute(t.Context(), `result = await shell("printf hello; exit 7"); (result.stdout, result.exit_code, result.error)`)
	if err != nil || failed || !strings.Contains(output, "('hello', 7, 'exit status 7')") {
		t.Fatalf("shell result = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLExposesLLM(t *testing.T) {
	var prompts []string
	repl := newPythonREPL(func(_ context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "result for " + prompt, nil
	})
	t.Cleanup(repl.close)

	output, failed, err := repl.execute(t.Context(), `[await llm(item) for item in ["first", "second"]]`)
	if err != nil || failed || output != "['result for first', 'result for second']" {
		t.Fatalf("llm result = %q, failed=%v, error=%v", output, failed, err)
	}
	if strings.Join(prompts, ",") != "first,second" {
		t.Fatalf("llm prompts = %#v", prompts)
	}
}

func TestPythonREPLRunsLLMCallsConcurrently(t *testing.T) {
	var started atomic.Int32
	release := make(chan struct{})
	repl := newPythonREPL(func(ctx context.Context, prompt string) (string, error) {
		if started.Add(1) == 2 {
			close(release)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if prompt == "first" {
			time.Sleep(20 * time.Millisecond)
		}
		return "result for " + prompt, nil
	})
	t.Cleanup(repl.close)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	output, failed, err := repl.execute(ctx, `import asyncio; await asyncio.gather(llm("first"), llm("second"))`)
	if err != nil || failed || output != "['result for first', 'result for second']" {
		t.Fatalf("llm result = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLDrainsConcurrentLLMCallsAfterError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	repl := newPythonREPL(func(ctx context.Context, prompt string) (string, error) {
		if prompt == "first" {
			select {
			case <-secondStarted:
				return "", errors.New("first failed")
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		close(secondStarted)
		select {
		case <-releaseSecond:
			return "second result", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	t.Cleanup(repl.close)

	type executionResult struct {
		output string
		failed bool
		err    error
	}
	resultCh := make(chan executionResult, 1)
	go func() {
		output, failed, err := repl.execute(ctx, `import asyncio; await asyncio.gather(llm("first"), llm("second"))`)
		resultCh <- executionResult{output, failed, err}
	}()
	select {
	case <-secondStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case got := <-resultCh:
		t.Fatalf("execute returned before pending LLM call completed: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseSecond)
	var got executionResult
	select {
	case got = <-resultCh:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got.err != nil || !got.failed || !strings.Contains(got.output, "RuntimeError: first failed") {
		t.Fatalf("llm result = %q, failed=%v, error=%v", got.output, got.failed, got.err)
	}

	output, failed, err := repl.execute(t.Context(), "42")
	if err != nil || failed || output != "42" {
		t.Fatalf("result after failed concurrent call = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLDrainsCanceledLLMCall(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	repl := newPythonREPL(func(ctx context.Context, prompt string) (string, error) {
		if prompt != "stale" {
			return "result for " + prompt, nil
		}
		close(started)
		<-ctx.Done()
		close(finished)
		return "", ctx.Err()
	})
	t.Cleanup(repl.close)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	output, failed, err := repl.execute(ctx, `preserved = 42; await llm("stale")`)
	if !errors.Is(err, context.Canceled) || !failed || output != "" {
		t.Fatalf("canceled result = %q, failed=%v, error=%v", output, failed, err)
	}

	<-finished
	output, failed, err = repl.execute(t.Context(), `(preserved, await llm("fresh"))`)
	if err != nil || failed || output != "(42, 'result for fresh')" {
		t.Fatalf("fresh result = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLPreservesStateWhenCancellationOvertakesLLMDispatch(t *testing.T) {
	marker := t.TempDir() + "/host-call"
	release := t.TempDir() + "/release"
	repl := newPythonREPL(func(_ context.Context, prompt string) (string, error) {
		return "result for " + prompt, nil
	})
	t.Cleanup(repl.close)

	code := fmt.Sprintf(`
import os, signal, time
protocol_out = llm.__globals__["_protocol_out"]
class DelayedHostCall:
    def write(self, data):
        if '"host_call"' in data:
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            open(%q, "w").close()
            while not os.path.exists(%q):
                time.sleep(0.001)
        return protocol_out.write(data)
    def flush(self):
        protocol_out.flush()
llm.__globals__["_protocol_out"] = DelayedHostCall()
preserved = 42
await llm("stale")`, marker, release)

	type executionResult struct {
		output string
		failed bool
		err    error
	}
	ctx, cancel := context.WithCancel(t.Context())
	resultCh := make(chan executionResult, 1)
	go func() {
		output, failed, err := repl.execute(ctx, code)
		resultCh <- executionResult{output, failed, err}
	}()

	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Python did not emit host call")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := <-resultCh
	if !errors.Is(got.err, context.Canceled) || !got.failed || got.output != "" {
		t.Fatalf("canceled result = %q, failed=%v, error=%v", got.output, got.failed, got.err)
	}

	nextCtx, nextCancel := context.WithTimeout(t.Context(), time.Second)
	defer nextCancel()
	output, failed, err := repl.execute(nextCtx, `(preserved, await llm("fresh"))`)
	if err != nil || failed || output != "(42, 'result for fresh')" {
		t.Fatalf("fresh result = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLRunsAsyncShellCallsConcurrently(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)

	dir := t.TempDir()
	first := fmt.Sprintf(`touch %q; while [ ! -e %q ]; do sleep 0.01; done; printf first`, dir+"/first", dir+"/second")
	second := fmt.Sprintf(`touch %q; while [ ! -e %q ]; do sleep 0.01; done; printf second`, dir+"/second", dir+"/first")
	code := fmt.Sprintf(`import asyncio; results = await asyncio.gather(shell(%q, 1), shell(%q, 1)); [(result.stdout, result.exit_code, result.error) for result in results]`, first, second)
	output, failed, err := repl.execute(t.Context(), code)
	if err != nil || failed || output != "[('first', 0, None), ('second', 0, None)]" {
		t.Fatalf("result = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLRequiresStringLLMPrompt(t *testing.T) {
	repl := newPythonREPL(func(_ context.Context, prompt string) (string, error) {
		t.Fatalf("llm callback called with %q", prompt)
		return "", nil
	})
	t.Cleanup(repl.close)

	output, failed, err := repl.execute(t.Context(), `await llm(42)`)
	if err != nil || !failed || !strings.Contains(output, "TypeError: llm() prompt must be a string") {
		t.Fatalf("llm result = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLIsolatesRuntimeAndRestoresHostFunctions(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)

	_, failed, err := repl.execute(t.Context(), `json = "user value"; _protocol_out = None; shell = "shadowed"`)
	if err != nil || failed {
		t.Fatalf("shadowing result: failed=%v, error=%v", failed, err)
	}
	output, failed, err := repl.execute(t.Context(), `(json, _protocol_out, callable(shell))`)
	if err != nil || failed || output != "('user value', None, True)" {
		t.Fatalf("isolated result = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLWebSearchParsesResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<div data-kind='web' class='results_links result web-result'>
<h2><a href='//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fdocs' class='result__a'>Example <strong>&amp; docs</strong></a></h2>
<a class='result__snippet'>A <b>nested</b> result.</a>
</div>
<div class="result"><h2><a class="result__a" href="https://example.org">Second</a></h2></div>`)
	}))
	defer server.Close()

	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	code := fmt.Sprintf(`web_search.__globals__["_web_search_url"] = %q; hits = await web_search("test", 2); [(hit.title, hit.url, hit.snippet) for hit in hits]`, server.URL)
	output, failed, err := repl.execute(t.Context(), code)
	want := "[('Example & docs', 'https://example.com/docs', 'A nested result.'), ('Second', 'https://example.org', '')]"
	if err != nil || failed || output != want {
		t.Fatalf("search result = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLReportsPythonErrorsWithoutLosingState(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	_, _, _ = repl.execute(t.Context(), "saved = 'still here'")
	output, failed, err := repl.execute(t.Context(), "1 / 0")
	if err != nil || !failed || !strings.Contains(output, "ZeroDivisionError") {
		t.Fatalf("error result = %q, failed=%v, error=%v", output, failed, err)
	}
	output, failed, err = repl.execute(t.Context(), "saved")
	if err != nil || failed || output != "'still here'" {
		t.Fatalf("state after error = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLCancellationPreservesState(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	_, _, _ = repl.execute(t.Context(), "saved = 42")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, _, err := repl.execute(ctx, `await shell("sleep 10 | cat")`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error = %v", err)
	}
	output, failed, err := repl.execute(t.Context(), "saved")
	if err != nil || failed || output != "42" {
		t.Fatalf("preserved result = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLCancellationStopsConcurrentShellCalls(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)

	if _, _, err := repl.execute(t.Context(), "1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, _, err := repl.execute(ctx, `import asyncio; await asyncio.gather(shell("sleep 10"), shell("sleep 10"))`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error = %v", err)
	}
	output, failed, err := repl.execute(t.Context(), "42")
	if err != nil || failed || output != "42" {
		t.Fatalf("result after cancellation = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLRestartsWhenInterruptIsIgnored(t *testing.T) {
	for name, code := range map[string]string{
		"blocking code":  "import time; ready(); time.sleep(30)",
		"ignored signal": "import signal, asyncio; signal.signal(signal.SIGINT, signal.SIG_IGN); ready(); await asyncio.sleep(30)",
		"ignored cancellation": `
import asyncio
try:
    ready()
    await asyncio.sleep(30)
except asyncio.CancelledError:
    await asyncio.sleep(30)
`,
	} {
		t.Run(name, func(t *testing.T) {
			repl := newPythonREPL(nil)
			t.Cleanup(repl.close)
			if _, _, err := repl.execute(t.Context(), "saved = 42"); err != nil {
				t.Fatal(err)
			}
			checkpointDir := t.TempDir()
			if err := repl.snapshot(checkpointDir+"/state.json", checkpointDir+"/objects"); err != nil {
				t.Fatal(err)
			}
			process := repl.cmd.Process
			marker := t.TempDir() + "/started"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, _, err := repl.execute(ctx, fmt.Sprintf("saved = 99\nunsaved = True\ndef ready():\n    open(%q, 'w').close()\n%s", marker, code))
				result <- err
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					_ = process.Kill()
					<-result
					t.Fatal("Python did not start executing")
				}
				time.Sleep(time.Millisecond)
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "restarted from last checkpoint") {
					t.Fatalf("cancellation error = %v", err)
				}
			case <-time.After(10 * time.Second):
				_ = process.Kill()
				<-result
				t.Fatal("interrupt did not return")
			}
			output, failed, err := repl.execute(t.Context(), "(saved, 'unsaved' in globals())")
			if err != nil || failed || output != "(42, False)" {
				t.Fatalf("result after restart = %q, failed=%v, error=%v", output, failed, err)
			}
			if repl.cmd.Process.Pid == process.Pid {
				t.Fatal("unresponsive REPL was not restarted")
			}
		})
	}
}

func TestPythonREPLRestartUsesRestoredCheckpoint(t *testing.T) {
	checkpointDir := t.TempDir()
	checkpoint := checkpointDir + "/state.json"
	objects := checkpointDir + "/objects"
	original := newPythonREPL(nil)
	t.Cleanup(original.close)
	if _, _, err := original.execute(t.Context(), "saved = (40, 2)"); err != nil {
		t.Fatal(err)
	}
	if err := original.snapshot(checkpoint, objects); err != nil {
		t.Fatal(err)
	}
	original.close()

	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	if err := repl.restore(checkpoint, objects); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, _, err := repl.execute(ctx, "saved = 99; import time; time.sleep(30)")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "restarted from last checkpoint") {
		t.Fatalf("cancellation error = %v", err)
	}
	output, failed, err := repl.execute(t.Context(), "saved")
	if err != nil || failed || output != "(40, 2)" {
		t.Fatalf("restored state = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLCancelWithInheritedStderr(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	output, failed, err := repl.execute(t.Context(), `
import subprocess
child = subprocess.Popen(["sleep", "30"], stdout=subprocess.DEVNULL)
child.pid
`)
	if err != nil || failed {
		t.Fatalf("start child: output=%q, failed=%v, error=%v", output, failed, err)
	}
	var pid int
	if _, err := fmt.Sscan(output, &pid); err != nil {
		t.Fatal(err)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Kill() })
	process := repl.cmd.Process
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := repl.execute(ctx, "import time; time.sleep(30)")
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "restarted") {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = process.Kill()
		_ = child.Kill()
		<-result
		t.Fatal("interrupt blocked on inherited stderr")
	}
	output, failed, err = repl.execute(t.Context(), "40 + 2")
	if err != nil || failed || output != "42" {
		t.Fatalf("result after restart = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLIgnoresInterruptWhileIdle(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)

	if _, _, err := repl.execute(t.Context(), "1"); err != nil {
		t.Fatal(err)
	}
	if err := repl.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	output, failed, err := repl.execute(t.Context(), "42")
	if err != nil || failed || output != "42" {
		t.Fatalf("result = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLShellDoesNotInheritProtocolInput(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	output, failed, err := repl.execute(t.Context(), `result = await shell("if read line; then printf data; else printf eof; fi", 0.1); (result.stdout, result.exit_code, result.error)`)
	if err != nil || failed || output != "('eof', 0, None)" {
		t.Fatalf("shell result = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLShellTimeout(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	started := time.Now()
	output, failed, err := repl.execute(t.Context(), `result = await shell("sleep 10 | cat", 0.05); (result.exit_code, result.error)`)
	if err != nil || failed || output != "(-1, 'timeout:0.05')" {
		t.Fatalf("timeout result = %q, failed=%v, error=%v", output, failed, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timed command took %s", elapsed)
	}
}
