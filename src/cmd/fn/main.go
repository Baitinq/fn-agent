package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Baitinq/fn-agent/src/internal/agent"
	"github.com/Baitinq/fn-agent/src/internal/assert"
	"github.com/Baitinq/fn-agent/src/internal/ui"
)

func requireAPIKey() error {
	provider := os.Getenv("FN_PROVIDER")
	if provider == "gemini" {
		if os.Getenv("GEMINI_API_KEY") == "" && os.Getenv("FN_API_KEY") == "" {
			return fmt.Errorf("GEMINI_API_KEY or FN_API_KEY must be set")
		}
		return nil
	}
	if provider == "anthropic" {
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			return fmt.Errorf("ANTHROPIC_API_KEY must be set")
		}
		return nil
	}
	if os.Getenv("OPENAI_API_KEY") == "" && os.Getenv("FN_API_KEY") == "" {
		return fmt.Errorf("OPENAI_API_KEY or FN_API_KEY must be set")
	}
	return nil
}

func parseArgs(args []string) (string, bool, bool, string, error) {
	flags := flag.NewFlagSet("fn", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	printMode := flags.Bool("p", false, "print the response without starting the UI")
	flags.BoolVar(printMode, "print", false, "print the response without starting the UI")
	jsonMode := flags.Bool("json", false, "print structured JSONL output")
	sessionID := flags.String("session", "", "restore a session by UUID")
	if err := flags.Parse(args); err != nil {
		return "", false, false, "", err
	}
	if *jsonMode && !*printMode {
		return "", false, false, "", fmt.Errorf("--json requires -p")
	}
	if !*printMode {
		if flags.NArg() > 0 {
			return "", false, false, "", fmt.Errorf("unexpected arguments; use -p to run in print mode")
		}
		return "", false, false, *sessionID, nil
	}
	return strings.Join(flags.Args(), " "), true, *jsonMode, *sessionID, nil
}

func buildPrintPrompt(instruction string, stdin io.Reader) (string, error) {
	if file, ok := stdin.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeCharDevice != 0 {
			stdin = strings.NewReader("")
		}
	}
	input, err := io.ReadAll(stdin)
	if err != nil {
		return "", err
	}
	prompt := strings.TrimSpace(strings.Join([]string{instruction, string(input)}, "\n\n"))
	if prompt == "" {
		return "", fmt.Errorf("print mode requires a prompt or stdin input")
	}
	return prompt, nil
}

func printResponse(prompt string, respond func(string, <-chan string, func(agent.ToolEvent), context.Context) agent.Response, out io.Writer) error {
	resp := respond(prompt, nil, func(agent.ToolEvent) {}, context.Background())
	if resp.Err != nil {
		return resp.Err
	}
	if _, err := io.WriteString(out, resp.Text); err != nil {
		return err
	}
	if !strings.HasSuffix(resp.Text, "\n") {
		_, err := io.WriteString(out, "\n")
		return err
	}
	return nil
}

func printJSONResponse(prompt string, a *agent.Agent, respond func(string, <-chan string, func(agent.ToolEvent), context.Context) agent.Response, out io.Writer) error {
	usageStart := len(a.Usage())
	resp := respond(prompt, nil, func(agent.ToolEvent) {}, context.Background())
	if resp.Err != nil {
		return resp.Err
	}
	encoder := json.NewEncoder(out)
	for _, usage := range a.Usage()[usageStart:] {
		if err := encoder.Encode(struct {
			Type  string      `json:"type"`
			Usage agent.Usage `json:"usage"`
		}{Type: "response", Usage: usage}); err != nil {
			return err
		}
	}
	return encoder.Encode(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "result", Text: resp.Text})
}

func newSessionID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}

func validSessionID(id string) bool {
	parts := strings.Split(id, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		return false
	}
	for _, part := range parts {
		for _, c := range part {
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

func setRunningSession(home, sessionID string) error {
	assert.That(home != "", "set running session without home directory")
	assert.That(filepath.IsAbs(home), "home directory is not absolute")
	assert.That(validSessionID(sessionID), "invalid running session ID")
	dir := filepath.Join(home, ".fn", "running")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getpid())), []byte(sessionID), 0o600)
}

func publishRunningSession(home, sessionID string) (func(), error) {
	if err := setRunningSession(home, sessionID); err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".fn", "running", strconv.Itoa(os.Getpid()))
	return func() { os.Remove(path) }, nil
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	prompt, printMode, jsonMode, sessionID, err := parseArgs(args)
	if err != nil {
		return err
	}
	if printMode {
		prompt, err = buildPrintPrompt(prompt, stdin)
		if err != nil {
			return err
		}
	}
	if err := requireAPIKey(); err != nil {
		return err
	}
	a, err := agent.New()
	if err != nil {
		return err
	}
	defer a.Close()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sessionsDir := home + "/.fn/sessions"
	if sessionID == "" {
		sessionID, err = newSessionID()
		if err != nil {
			return err
		}
		err = a.StartSession(sessionID, sessionsDir)
	} else {
		if !validSessionID(sessionID) {
			return fmt.Errorf("invalid session UUID %q", sessionID)
		}
		err = a.ResumeSession(sessionID, sessionsDir)
	}
	if err != nil {
		return err
	}
	removeRunningSession, err := publishRunningSession(home, sessionID)
	if err != nil {
		return err
	}
	defer removeRunningSession()
	respond := func(input string, steer <-chan string, emit func(agent.ToolEvent), ctx context.Context) agent.Response {
		return a.Respond(input, steer, emit, ctx)
	}
	if printMode {
		if jsonMode {
			return printJSONResponse(prompt, a, respond, stdout)
		}
		if err := printResponse(prompt, respond, stdout); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "tokens used\n%d\n", a.TokensUsed())
		return nil
	}
	commands := ui.Commands{
		Undo: a.Undo,
		Fork: func() (string, error) {
			id, err := newSessionID()
			if err != nil {
				return "", err
			}
			if err := setRunningSession(home, id); err != nil {
				return "", err
			}
			if err := a.Fork(id, sessionsDir); err != nil {
				_ = setRunningSession(home, sessionID)
				return "", err
			}
			sessionID = id
			return id, nil
		},
	}
	contextTokens, cacheHitRate := a.ContextUsage()
	return ui.Run(a.ModelName(), a.ReasoningEffort(), sessionID, cwd, a.Conversation(), contextTokens, cacheHitRate, commands, respond)
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
