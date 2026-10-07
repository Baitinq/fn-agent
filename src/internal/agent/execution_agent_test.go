package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAgentYieldsOrdinaryCommandsAndCanChooseToWait(t *testing.T) {
	commands := []struct {
		code string
	}{
		{code: `await shell("echo ready; exec sleep 60")`},
		{code: `job = execution(1); print(6 * 7); print(job.status)`},
		{code: `await job.cancel(); print(job.status)`},
		{code: `import asyncio; await asyncio.sleep(0.3); print("waited")`},
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var output []map[string]any
		if calls < len(commands) {
			args, _ := json.Marshal(map[string]any{"code": commands[calls].code})
			output = []map[string]any{{
				"id": fmt.Sprintf("fc_%d", calls), "type": "function_call",
				"call_id": fmt.Sprintf("call_%d", calls), "name": "repl", "arguments": string(args), "status": "completed",
			}}
		} else {
			output = []map[string]any{{
				"id": "msg_done", "type": "message", "role": "assistant", "status": "completed",
				"content": []map[string]string{{"type": "output_text", "text": "done"}},
			}}
		}
		calls++
		response, _ := json.Marshal(map[string]any{
			"type": "response.completed", "sequence_number": 1, "response": map[string]any{"id": "resp_test", "object": "response", "model": "test-model", "status": "completed", "output": output},
		})
		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", response)
	}))
	defer server.Close()
	t.Setenv("FN_BASE_URL", server.URL)
	t.Setenv("FN_PROVIDER", "openai")
	t.Setenv("FN_MODEL", "test-model")
	t.Setenv("FN_API_KEY", "test-key")
	t.Setenv("FN_HEADERS", "")
	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	startTestSession(t, a)
	var results []string
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := a.Respond("start work, do something else, inspect it, then stop it", nil, func(event ToolEvent) {
		if event.Kind == ToolEventResult {
			results = append(results, event.Detail)
		}
	}, ctx)
	if result.Err != nil || result.Text != "done" {
		t.Fatalf("agent response = %+v", result)
	}
	if len(results) != 4 || !strings.HasPrefix(results[0], "Execution ") || !strings.Contains(results[0], "ready\n") {
		t.Fatalf("command did not yield with partial output: %q", results)
	}
	want := []string{"42\nrunning\n", "cancelled\n", "waited\n"}
	if fmt.Sprint(results[1:]) != fmt.Sprint(want) {
		t.Fatalf("tool results = %q, want %q", results[1:], want)
	}
}
