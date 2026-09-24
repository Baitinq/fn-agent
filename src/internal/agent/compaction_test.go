package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func historyMessage(role, text string) historyItem {
	return historyItem{Type: "message", Role: role, Text: text}
}

func TestFindHistoryCutKeepsRecentCompleteTurns(t *testing.T) {
	history := []historyItem{
		historyMessage("user", "first request"),
		historyMessage("assistant", strings.Repeat("a", 400)),
		historyMessage("user", "second request"),
		historyMessage("assistant", strings.Repeat("b", 400)),
		historyMessage("user", "third request"),
		historyMessage("assistant", strings.Repeat("c", 400)),
	}
	latestTurnTokens := estimateHistoryItemTokens(history[4]) + estimateHistoryItemTokens(history[5])
	if cut := findHistoryCut(history, latestTurnTokens+1); cut != 2 {
		t.Fatalf("cut = %d, want second user message at 2", cut)
	}
}

func TestFindHistoryCutDoesNotStartAtToolOutput(t *testing.T) {
	history := []historyItem{
		historyMessage("user", "request"),
		historyItem{Type: "tool_call", Arguments: json.RawMessage(`{"code":"work()"}`), CallID: "call_1", Name: "repl"},
		historyItem{Type: "tool_result", CallID: "call_1", Text: strings.Repeat("result", 200)},
		historyMessage("assistant", "finished"),
	}
	cut := findHistoryCut(history, estimateHistoryItemTokens(history[3])+1)
	if cut == 2 || cut == 0 {
		t.Fatalf("unsafe split-turn cut = %d", cut)
	}
}

func TestFindHistoryCutKeepsBatchedToolCallsPaired(t *testing.T) {
	history := []historyItem{
		historyMessage("user", "request"),
		historyItem{Type: "tool_call", Arguments: json.RawMessage(`{"code":"first()"}`), CallID: "call_1", Name: "repl"},
		historyItem{Type: "tool_call", Arguments: json.RawMessage(`{"code":"second()"}`), CallID: "call_2", Name: "repl"},
		historyItem{Type: "tool_result", CallID: "call_1", Text: "first result"},
		historyItem{Type: "tool_result", CallID: "call_2", Text: "second result"},
	}
	if isSafeHistoryCut(history, 2) {
		t.Fatal("cut between batched function calls leaves an orphaned output")
	}
	if !isSafeHistoryCut(history, 1) {
		t.Fatal("cut before the complete tool-call batch was unsafe")
	}
}

func TestCompactHistoryPreservesCanonicalHistory(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_summary\",\"object\":\"response\",\"model\":\"test\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_summary\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"## Goal\\nContinue the test\",\"annotations\":[]}]}]}}\n\n")
	}))
	defer server.Close()

	a := &Agent{
		client:    openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0)),
		modelName: "test",
		history: []historyItem{
			historyMessage("user", "old request"),
			historyMessage("assistant", strings.Repeat("old result", 100)),
			historyMessage("user", "middle request"),
			historyMessage("assistant", strings.Repeat("middle result", 100)),
			historyMessage("user", "recent request"),
			historyMessage("assistant", "recent result"),
		},
	}
	latestTokens := estimateHistoryItemTokens(a.history[4]) + estimateHistoryItemTokens(a.history[5])
	if err := a.compactHistory(t.Context(), latestTokens+1); err != nil {
		t.Fatal(err)
	}
	if a.compaction == nil || a.compaction.Summary != "## Goal\nContinue the test" {
		t.Fatalf("compaction = %#v", a.compaction)
	}
	if len(a.history) != 6 || a.compaction.FirstKeptItem != 2 {
		t.Fatalf("canonical history = %#v, summarized items %d", a.history, a.compaction.FirstKeptItem)
	}
	body, _ := json.Marshal(request)
	if !strings.Contains(string(body), "old request") || strings.Contains(string(body), "recent request") {
		t.Fatalf("summarization request used the wrong history: %s", body)
	}
	if len(a.input()) != 5 {
		t.Fatalf("compacted input has %d items, want summary plus four recent items", len(a.input()))
	}
}

func TestRespondCompactsAndRetriesContextOverflow(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"Maximum context length exceeded","type":"invalid_request_error","code":"context_length_exceeded"}}`)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		text, id := "## Goal\nRecovered context", "msg_summary"
		if requests == 3 {
			text, id = "recovered", "msg_answer"
		}
		payload, _ := json.Marshal(map[string]any{
			"type": "response.completed", "sequence_number": 1,
			"response": map[string]any{
				"id": "resp_test", "object": "response", "model": "test", "status": "completed",
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12, "input_tokens_details": map[string]any{}, "output_tokens_details": map[string]any{}},
				"output": []any{map[string]any{
					"id": id, "type": "message", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
				}},
			},
		})
		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", payload)
	}))
	defer server.Close()

	a := &Agent{
		client:       openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0)),
		modelName:    "test",
		instructions: "test",
		maxRetries:   0,
		history: []historyItem{
			historyMessage("user", "old request"),
			historyMessage("assistant", "old result"),
		},
	}
	startTestSession(t, a)
	var events []ToolEvent
	response := a.Respond(strings.Repeat("new request ", 10000), nil, func(event ToolEvent) { events = append(events, event) }, t.Context())
	if response.Err != nil || response.Text != "recovered" {
		t.Fatalf("response = %#v", response)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want failed request, summary, and retry", requests)
	}
	var started, done bool
	for _, event := range events {
		started = started || event.Kind == ToolEventCompactionStart
		done = done || event.Kind == ToolEventCompactionDone
	}
	if !started || !done || a.compaction == nil {
		t.Fatalf("compaction state: started=%v done=%v compaction=%#v events=%#v", started, done, a.compaction, events)
	}
}
