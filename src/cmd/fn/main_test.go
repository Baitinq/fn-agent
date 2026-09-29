package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Baitinq/fn-agent/src/internal/agent"
)

func TestRequireAPIKey(t *testing.T) {
	t.Setenv("FN_API_KEY", "")
	t.Setenv("FN_PROVIDER", "")
	t.Setenv("FN_MODEL", "")
	t.Setenv("OPENAI_API_KEY", "")
	if err := requireAPIKey(); err == nil {
		t.Fatal("expected an error when OPENAI_API_KEY is empty")
	}
	t.Setenv("OPENAI_API_KEY", "test-key")
	if err := requireAPIKey(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Setenv("FN_PROVIDER", "openai-completions")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("FN_API_KEY", "gateway-token")
	if err := requireAPIKey(); err != nil {
		t.Fatalf("unexpected completions error: %v", err)
	}
	t.Setenv("FN_PROVIDER", "gemini")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("FN_API_KEY", "")
	if err := requireAPIKey(); err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("Gemini error = %v", err)
	}
	t.Setenv("GEMINI_API_KEY", "test-gemini-key")
	if err := requireAPIKey(); err != nil {
		t.Fatalf("unexpected Gemini error: %v", err)
	}
	t.Setenv("FN_PROVIDER", "anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if err := requireAPIKey(); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("Anthropic error = %v", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "test-anthropic-key")
	if err := requireAPIKey(); err != nil {
		t.Fatalf("unexpected Anthropic error: %v", err)
	}
}

func TestParseArgs(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		prompt    string
		printMode bool
		wantErr   string
	}{
		{name: "interactive"},
		{name: "short print", args: []string{"-p", "inspect", "this"}, prompt: "inspect this", printMode: true},
		{name: "long print", args: []string{"--print", "inspect this"}, prompt: "inspect this", printMode: true},
		{name: "stdin print", args: []string{"-p"}, printMode: true},
		{name: "json print", args: []string{"-p", "--json"}, printMode: true},
		{name: "json requires print", args: []string{"--json"}, wantErr: "requires -p"},
		{name: "unexpected argument", args: []string{"hello"}, wantErr: "use -p"},
	} {
		t.Run(test.name, func(t *testing.T) {
			prompt, printMode, _, _, err := parseArgs(test.args)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil || prompt != test.prompt || printMode != test.printMode {
				t.Fatalf("parseArgs() = %q, %v, %v", prompt, printMode, err)
			}
		})
	}
}

func TestSessionIDs(t *testing.T) {
	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if !validSessionID(id) {
		t.Fatalf("generated invalid session ID %q", id)
	}
	for _, invalid := range []string{"", "not-a-uuid", "../../other", "550e8400-e29b-41d4-a716-44665544000z"} {
		if validSessionID(invalid) {
			t.Fatalf("accepted invalid session ID %q", invalid)
		}
	}
}

func TestPublishRunningSession(t *testing.T) {
	home := t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	remove, err := publishRunningSession(home, id)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".fn", "running", strconv.Itoa(os.Getpid()))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != id {
		t.Fatalf("running session = %q", data)
	}
	remove()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("running session still exists: %v", err)
	}
}

func TestBuildPrintPrompt(t *testing.T) {
	for _, test := range []struct {
		name        string
		instruction string
		stdin       string
		want        string
		wantErr     bool
	}{
		{name: "instruction", instruction: "review this", want: "review this"},
		{name: "stdin", stdin: "review this\n", want: "review this"},
		{name: "instruction and stdin", instruction: "review this", stdin: "diff content\n", want: "review this\n\ndiff content"},
		{name: "empty", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := buildPrintPrompt(test.instruction, strings.NewReader(test.stdin))
			if test.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("buildPrintPrompt() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestPrintResponseWritesOnlyFinalText(t *testing.T) {
	var out bytes.Buffer
	respond := func(prompt string, _ <-chan string, emit func(agent.ToolEvent), _ context.Context) agent.Response {
		if prompt != "delegate this" {
			t.Fatalf("prompt = %q", prompt)
		}
		emit(agent.ToolEvent{Kind: agent.ToolEventUpdate, Detail: "tool noise"})
		return agent.Response{Text: "child result"}
	}
	if err := printResponse("delegate this", respond, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "child result\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestPrintJSONResponseWritesUsageAndResult(t *testing.T) {
	var out bytes.Buffer
	a := &agent.Agent{}
	respond := func(string, <-chan string, func(agent.ToolEvent), context.Context) agent.Response {
		return agent.Response{Text: "done"}
	}
	if err := printJSONResponse("task", a, respond, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "{\"type\":\"result\",\"text\":\"done\"}\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestRunJSONWritesStructuredUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":\"test\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":40,\"input_tokens_details\":{\"cached_tokens\":10},\"output_tokens\":2,\"output_tokens_details\":{\"reasoning_tokens\":1},\"total_tokens\":42}}}\n\n")
	}))
	defer server.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("FN_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("FN_BASE_URL", server.URL+"/")
	t.Setenv("FN_MODEL", "test")
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-p", "--json", "do it"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	usage := events[0]["usage"].(map[string]any)
	if len(events) != 2 || events[0]["type"] != "response" || usage["total_tokens"] != float64(42) || events[1]["text"] != "done" || stderr.Len() != 0 {
		t.Fatalf("events = %#v, stderr = %q", events, stderr.String())
	}
}

func TestPrintResponseReturnsAgentError(t *testing.T) {
	want := errors.New("request failed")
	respond := func(string, <-chan string, func(agent.ToolEvent), context.Context) agent.Response {
		return agent.Response{Err: want}
	}
	if err := printResponse("hello", respond, &bytes.Buffer{}); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}
