package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func checkpointedREPL(t *testing.T) *pythonREPL {
	t.Helper()
	r := newPythonREPL(nil)
	t.Cleanup(r.close)
	if _, _, err := r.execute(t.Context(), "saved = 42"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := r.snapshot(filepath.Join(dir, "state.json"), filepath.Join(dir, "objects")); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPythonREPLRecoversAfterProcessLoss(t *testing.T) {
	for _, reason := range []string{"exit", "killed", "protocol error", "closed", "snapshot after kill"} {
		t.Run(reason, func(t *testing.T) {
			r := checkpointedREPL(t)
			switch reason {
			case "exit", "protocol error":
				code := "import os; os._exit(7)"
				if reason == "protocol error" {
					code = "import os; os.write(1, b'not json\\n')"
				}
				_, _, err := r.execute(t.Context(), code)
				if err == nil || !strings.Contains(err.Error(), "restarted from last checkpoint") {
					t.Fatalf("recovery error = %v", err)
				}
			case "killed", "snapshot after kill":
				if err := r.cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if reason == "snapshot after kill" {
					if err := r.snapshot(r.checkpoint, r.checkpointObjects); err != nil {
						t.Fatal(err)
					}
				} else if _, _, err := r.execute(t.Context(), "saved = 99"); err == nil {
					t.Fatal("execution on a killed process succeeded")
				}
			case "closed":
				r.close()
			}
			output, failed, err := r.execute(t.Context(), "saved")
			if err != nil || failed || output != "42" {
				t.Fatalf("restored state = %q, failed=%v, error=%v", output, failed, err)
			}
			notices := r.takeNotices()
			if len(notices) != 1 || !strings.Contains(notices[0], "restarted from last checkpoint") {
				t.Fatalf("recovery notices = %v", notices)
			}
			if notices := r.takeNotices(); len(notices) != 0 {
				t.Fatalf("duplicate recovery notices: %v", notices)
			}
		})
	}
}

func TestPythonREPLRecoveryDoesNotReplayCode(t *testing.T) {
	r := checkpointedREPL(t)
	marker := filepath.Join(t.TempDir(), "executions")
	code := fmt.Sprintf("with open(%q, 'a') as f:\n    f.write('x')\nimport os; os._exit(7)", marker)
	if _, _, err := r.execute(t.Context(), code); err == nil {
		t.Fatal("expected process exit error")
	}
	contents, err := os.ReadFile(marker)
	if err != nil || string(contents) != "x" {
		t.Fatalf("executions = %q, error=%v", contents, err)
	}
}

func TestPythonREPLFailedRecoveryPreservesCheckpoint(t *testing.T) {
	r := checkpointedREPL(t)
	checkpoint, objects := r.checkpoint, r.checkpointObjects
	original, err := os.ReadFile(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	r.close()
	if err := os.WriteFile(checkpoint, []byte("invalid json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.execute(t.Context(), "saved = 99"); err == nil {
		t.Fatal("expected restore failure")
	}
	if err := r.snapshot(checkpoint, objects); err == nil {
		t.Fatal("snapshot after failed recovery succeeded")
	}
	contents, err := os.ReadFile(checkpoint)
	if err != nil || string(contents) != "invalid json" {
		t.Fatalf("failed checkpoint was overwritten: %q, error=%v", contents, err)
	}
	if err := os.WriteFile(checkpoint, original, 0600); err != nil {
		t.Fatal(err)
	}
	output, failed, err := r.execute(t.Context(), "saved")
	if err != nil || failed || output != "42" {
		t.Fatalf("restored state = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestRespondRecoversAfterREPLInterrupt(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	code := fmt.Sprintf("open(%q, 'w').close(); import time; time.sleep(30)", marker)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var output []any
		if requests == 1 {
			args, _ := json.Marshal(map[string]string{"code": code})
			output = []any{map[string]any{"id": "fc_test", "type": "function_call", "call_id": "call_test", "name": "repl", "arguments": string(args), "status": "completed"}}
		} else {
			output = []any{map[string]any{"id": "msg_test", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "recovered", "annotations": []any{}}}}}
		}
		payload, _ := json.Marshal(map[string]any{"type": "response.completed", "sequence_number": 1, "response": map[string]any{"id": "resp_test", "object": "response", "model": "test", "status": "completed", "output": output}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", payload)
	}))
	defer server.Close()
	a := &Agent{client: openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0)), modelName: "test", instructions: "test"}
	startTestSession(t, a)
	t.Cleanup(a.Close)
	if _, _, err := a.pythonREPL().execute(t.Context(), "saved = 42"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var events []ToolEvent
	result := make(chan Response, 1)
	go func() {
		result <- a.Respond("run", nil, func(ev ToolEvent) { events = append(events, ev) }, ctx)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-result
			t.Fatal("Python did not start executing")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case response := <-result:
		if response.Err != nil {
			t.Fatalf("interrupt response = %#v", response)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not release interrupted turn")
	}
	found := false
	for _, event := range events {
		if event.Kind == ToolEventREPLRecovery && strings.Contains(event.Detail, "last checkpoint") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no recovery event: %#v", events)
	}
	response := a.Respond("continue", nil, func(ToolEvent) {}, t.Context())
	if response.Err != nil || response.Text != "recovered" {
		t.Fatalf("next response = %#v", response)
	}
	output, failed, err := a.pythonREPL().execute(t.Context(), "saved")
	if err != nil || failed || output != "42" {
		t.Fatalf("state after next prompt = %q, failed=%v, error=%v", output, failed, err)
	}
}

func TestPythonREPLRecoversFromHungSnapshot(t *testing.T) {
	r := checkpointedREPL(t)
	_, failed, err := r.execute(t.Context(), `
class Slow:
    def __reduce__(self):
        import time
        time.sleep(30)
slow = Slow()
saved = 99
`)
	if err != nil || failed {
		t.Fatalf("setup failed: failed=%v, error=%v", failed, err)
	}
	if err := r.snapshot(r.checkpoint, r.checkpointObjects); err != nil {
		t.Fatal(err)
	}
	output, failed, err := r.execute(t.Context(), "saved")
	if err != nil || failed || output != "42" {
		t.Fatalf("state after snapshot timeout = %q, failed=%v, error=%v", output, failed, err)
	}
	if notices := r.takeNotices(); len(notices) != 1 || !strings.Contains(notices[0], "last checkpoint") {
		t.Fatalf("snapshot recovery notices = %v", notices)
	}
}

func TestPythonREPLLateHostResponseAfterRecovery(t *testing.T) {
	r := checkpointedREPL(t)
	started := make(chan struct{})
	release := make(chan struct{})
	r.llm = func(ctx context.Context, prompt string) (string, error) {
		if prompt == "old" {
			close(started)
			<-release
			return "late", nil
		}
		return "new", nil
	}
	process := r.cmd.Process
	result := make(chan error, 1)
	go func() {
		_, _, err := r.execute(t.Context(), `await llm("old")`)
		result <- err
	}()
	<-started
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil {
		t.Fatal("expected process exit error")
	}
	close(release)
	output, failed, err := r.execute(t.Context(), `await llm("new")`)
	if err != nil || failed || output != "'new'" {
		t.Fatalf("new host response = %q, failed=%v, error=%v", output, failed, err)
	}
}
