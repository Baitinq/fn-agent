package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheWarmingReplaysRequestWhileToolRuns(t *testing.T) {
	cacheWarmInterval = 50 * time.Millisecond
	t.Cleanup(func() { cacheWarmInterval = 270 * time.Second })

	var calls, warms atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request anthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if !request.Stream {
			if request.MaxTokens != 1 {
				t.Errorf("warm request max_tokens = %d, want 1", request.MaxTokens)
			}
			warms.Add(1)
			fmt.Fprint(w, `{"usage":{"cache_read_input_tokens":100,"output_tokens":1}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"repl\",\"input\":{}}}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"code\\\":\\\"import time; time.sleep(0.3)\\\"}\"}}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	a := &Agent{provider: "anthropic", baseURL: server.URL, httpClient: server.Client(), modelName: "claude-opus-5-5", reasoningEffort: "high", instructions: "Use the tool.", cwd: t.TempDir()}
	startTestSession(t, a)
	a.Respond("wait", nil, func(ToolEvent) {}, context.Background())

	if calls.Load() != 2 {
		t.Fatalf("model calls = %d, want 2", calls.Load())
	}
	if warms.Load() < 2 {
		t.Fatalf("warm requests = %d, want at least 2", warms.Load())
	}
	after := warms.Load()
	time.Sleep(150 * time.Millisecond)
	if warms.Load() != after {
		t.Fatal("cache warming continued after the turn ended")
	}
}
