package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Baitinq/fn-agent/src/internal/agent"
	"time"
)

// TestTmuxHarness is a deterministic real-TTY target for tmux end-to-end tests.
// It is skipped during the normal suite and intentionally exercises the same
// Run entry point as production.
func TestTmuxHarness(t *testing.T) {
	if os.Getenv("FN_TMUX_HARNESS") != "1" {
		t.Skip("tmux harness")
	}
	respond := func(input string, steer <-chan string, emit func(agent.ToolEvent), ctx context.Context) agent.Response {
		switch input {
		case "stream":
			for i := 1; i <= 32; i++ {
				select {
				case <-ctx.Done():
					return agent.Response{}
				case <-time.After(18 * time.Millisecond):
				}
				emit(toolEvent{Kind: toolEventTextDelta, Detail: fmt.Sprintf("STREAM-LINE-%02d abcdefghijklmnopqrstuvwxyz\n", i)})
			}
			return agent.Response{Text: strings.TrimSuffix(buildNumberedLines("STREAM-LINE", 32), "\n"), ContextTokens: 321}
		case "steertest":
			emit(toolEvent{Kind: toolEventTextDelta, Detail: "STEER-WAIT"})
			select {
			case correction := <-steer:
				emit(toolEvent{Kind: toolEventSteerConsumed, Detail: correction})
				emit(toolEvent{Kind: toolEventTextReset})
				emit(toolEvent{Kind: toolEventTextDelta, Detail: "STEERED<" + correction + ">"})
				return agent.Response{Text: "STEERED<" + correction + ">", ContextTokens: 901}
			case <-ctx.Done():
				return agent.Response{}
			}
		case "toolburst":
			emit(toolEvent{Kind: toolEventCall, Name: "shell", ID: "burst-call", Detail: "produce 60 lines"})
			var output strings.Builder
			for i := 1; i <= 60; i++ {
				line := fmt.Sprintf("BURST-%02d\n", i)
				output.WriteString(line)
				emit(toolEvent{Kind: toolEventUpdate, Name: "shell", ID: "burst-call", Detail: line})
				time.Sleep(10 * time.Millisecond)
			}
			// Leave enough time for the last partial update to be rendered before
			// completion, so the tmux test proves this was genuinely live.
			time.Sleep(250 * time.Millisecond)
			emit(toolEvent{Kind: toolEventResult, Name: "shell", ID: "burst-call", Detail: output.String()})
			emit(toolEvent{Kind: toolEventTextDelta, Detail: "burst complete"})
			return agent.Response{Text: "burst complete", ContextTokens: 876}
		case "tools":
			emit(toolEvent{Kind: toolEventCall, Name: "shell", ID: "tool-call", Detail: "printf tool-output"})
			time.Sleep(40 * time.Millisecond)
			emit(toolEvent{Kind: toolEventUpdate, Name: "shell", ID: "tool-call", Detail: "LIVE-"})
			time.Sleep(40 * time.Millisecond)
			emit(toolEvent{Kind: toolEventUpdate, Name: "shell", ID: "tool-call", Detail: "PARTIAL"})
			time.Sleep(200 * time.Millisecond)
			emit(toolEvent{Kind: toolEventResult, Name: "shell", ID: "tool-call", Detail: "tool-output"})
			for _, chunk := range []string{"tool ", "turn ", "complete"} {
				emit(toolEvent{Kind: toolEventTextDelta, Detail: chunk})
				time.Sleep(20 * time.Millisecond)
			}
			return agent.Response{Text: "tool turn complete", ContextTokens: 654}
		case "cancel":
			emit(toolEvent{Kind: toolEventTextDelta, Detail: "WAITING-FOR-CANCEL"})
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			emit(toolEvent{Kind: toolEventREPLRecovery, Detail: "REPL unresponsive; restarted from last checkpoint. Changes from the interrupted call may be lost."})
			return agent.Response{}
		default:
			for _, chunk := range []string{"ECHO<", input, ">"} {
				emit(toolEvent{Kind: toolEventTextDelta, Detail: chunk})
				time.Sleep(20 * time.Millisecond)
			}
			return agent.Response{Text: "ECHO<" + input + ">", ContextTokens: 777}
		}
	}
	if err := Run("tmux-e2e", "medium", "test-session", "/tmp/project", nil, Commands{}, respond); err != nil {
		t.Fatal(err)
	}
}

func buildNumberedLines(prefix string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s-%02d abcdefghijklmnopqrstuvwxyz\n", prefix, i)
	}
	return b.String()
}
