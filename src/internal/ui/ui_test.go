package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tui "github.com/grindlemire/go-tui"

	"github.com/Baitinq/fn-agent/src/internal/agent"
)

const (
	testModelName       = "test-model"
	testReasoningEffort = "medium"
)

type testRuntime struct{ calls chan func() }

func newTestRuntime() *testRuntime        { return &testRuntime{calls: make(chan func(), 32)} }
func (r *testRuntime) Dispatch(fn func()) { r.calls <- fn }

func (r *testRuntime) runNext(t *testing.T) {
	t.Helper()
	select {
	case fn := <-r.calls:
		fn()
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for UI dispatch")
	}
}

func newState(respond func(string, <-chan string, func(toolEvent), context.Context) response) (*fnUI, *testRuntime) {
	runtime := newTestRuntime()
	s := newUI(testModelName, testReasoningEffort, respond)
	s.ensureTextarea()
	s.dispatch = runtime.Dispatch
	s.emit = func(message) {}
	return s, runtime
}

func TestUndoCommandSelectsTurnAndRestoresInput(t *testing.T) {
	s, _ := newState(nil)
	s.restoreConversation([]agent.ConversationMessage{
		{Role: "user", Text: "first"},
		{Role: "assistant", Text: "one"},
		{Role: "user", Text: "second"},
		{Role: "assistant", Text: "two"},
	})
	gotSteps := -1
	s.commands.Undo = func(steps int) (string, error) { gotSteps = steps; return "first", nil }
	s.contextTokens = 100
	s.submitInput("/undo", false)
	if len(s.undoOptions) != 2 || s.undoSelected != 1 {
		t.Fatalf("selector state: options=%#v selected=%d", s.undoOptions, s.undoSelected)
	}
	s.handleKey(tui.KeyEvent{Key: tui.KeyUp})
	s.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	if gotSteps != 1 || len(s.messages) != 0 || len(s.inputHistory) != 0 || s.textarea.Text() != "first" || s.contextTokens != 0 {
		t.Fatalf("undo state: steps=%d messages=%#v history=%#v input=%q tokens=%d", gotSteps, s.messages, s.inputHistory, s.textarea.Text(), s.contextTokens)
	}
}

func TestHistoryPruningMatchesModelContext(t *testing.T) {
	s, _ := newState(nil)
	s.messages = []message{
		{role: "you", text: "prior"},
		{role: "agent", text: "prior answer"},
		{role: "tool", toolID: "old-tool", toolState: "success", toolResult: "old result"},
		{role: "you", text: "current"},
	}
	s.requestMessageStart = 3
	s.responding = true
	s.messages = append(s.messages,
		message{role: "reasoning", text: "thinking"},
		message{role: "agent", text: "checking"},
		message{role: "tool", toolID: "new-tool", toolState: "success", toolResult: "new result"},
	)
	s.streamingText.WriteString("final answer")

	s.handleToolEvent(0, toolEvent{Kind: toolEventToolResultsPruned})
	if s.messages[2].toolResult != agent.OmittedToolResult || s.messages[6].toolResult != agent.OmittedToolResult {
		t.Fatalf("tool results were not pruned: %#v", s.messages)
	}
	s.handleToolEvent(0, toolEvent{Kind: toolEventTransientHistoryPruned})
	if s.streamingText.Len() != 0 || len(s.messages) != 5 || s.messages[1].text != "prior answer" || s.messages[4].toolID != "new-tool" {
		t.Fatalf("transient history was not pruned: %#v", s.messages)
	}
}

func TestRestoreConversationIncludesToolCalls(t *testing.T) {
	s, _ := newState(nil)
	s.restoreConversation([]agent.ConversationMessage{
		{Type: "message", Role: "user", Text: "inspect"},
		{Type: "reasoning", Text: "I should check."},
		{Type: "tool_call", ToolID: "call-1", ToolName: "repl", ToolInput: "print(42)"},
		{Type: "tool_result", ToolID: "call-1", ToolName: "repl", Text: "42\n"},
		{Type: "message", Role: "assistant", Text: "done"},
	})
	if len(s.messages) != 4 || s.messages[0].role != "you" || s.messages[1].role != "reasoning" || s.messages[3].text != "done" {
		t.Fatalf("restored messages = %#v", s.messages)
	}
	tool := s.messages[2]
	if tool.role != "tool" || tool.toolName != "repl" || tool.toolCommand != "print(42)" || tool.toolResult != "42\n" || tool.toolState != "success" {
		t.Fatalf("restored tool = %#v", tool)
	}
}

func TestUndoSelectorEscapeCancels(t *testing.T) {
	s, _ := newState(nil)
	s.restoreConversation([]agent.ConversationMessage{{Role: "user", Text: "first"}})
	s.commands.Undo = func(int) (string, error) { t.Fatal("undo called"); return "", nil }
	s.submitInput("/undo", false)
	s.handleKey(tui.KeyEvent{Key: tui.KeyEscape})
	if s.undoOptions != nil || len(s.messages) != 1 {
		t.Fatalf("cancel state: options=%#v messages=%#v", s.undoOptions, s.messages)
	}
}

func TestExitCommandQuits(t *testing.T) {
	s, _ := newState(nil)
	s.textarea.SetText("/exit")
	if s.handleKey(tui.KeyEvent{Key: tui.KeyEnter}) {
		t.Fatal("exit command did not quit")
	}
}

func TestForkCommandUpdatesSession(t *testing.T) {
	s, _ := newState(nil)
	s.commands.Fork = func() (string, error) { return "new-session", nil }
	s.submitInput("/fork", false)
	if s.sessionID != "new-session" || len(s.messages) != 1 || s.messages[0].role != "system" {
		t.Fatalf("fork state: session=%q messages=%#v", s.sessionID, s.messages)
	}
}

func TestSessionCommandsAreRejectedWhileResponding(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	called := false
	s.commands.Undo = func(int) (string, error) { called = true; return "", nil }
	s.submitInput("/undo", false)
	if called || len(s.messages) != 1 || s.messages[0].role != "error" {
		t.Fatalf("command state: called=%v messages=%#v", called, s.messages)
	}
}

func TestTextareaEditingAndWordAliases(t *testing.T) {
	s, _ := newState(nil)
	s.textarea.SetText("hello world")
	s.moveWord(-1)
	s.textarea.InsertText("X")
	if got := s.textarea.Text(); got != "hello Xworld" {
		t.Fatalf("word movement produced %q", got)
	}
	s.deleteWord(-1)
	if got := s.textarea.Text(); got != "hello world" {
		t.Fatalf("word deletion produced %q", got)
	}

	s.textarea.SetText("alpha beta")
	s.textarea.SetCursorPos(5)
	s.deleteToLineStart()
	if got := s.textarea.Text(); got != " beta" {
		t.Fatalf("line deletion produced %q", got)
	}
}

func TestInputHistoryNavigatesOlderAndRestoresDraft(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.submitInput("first message", false)
	s.submitInput("second message", false)
	s.textarea.SetText("unfinished draft")

	up := tui.KeyEvent{Key: tui.KeyUp}
	down := tui.KeyEvent{Key: tui.KeyDown}
	for _, step := range []struct {
		key  tui.KeyEvent
		want string
	}{
		{up, "second message"},
		{up, "first message"},
		{up, "first message"},
		{down, "second message"},
		{down, "unfinished draft"},
	} {
		s.handleKey(step.key)
		if got := s.textarea.Text(); got != step.want {
			t.Fatalf("history navigation produced %q, want %q", got, step.want)
		}
	}
}

func TestUpArrowStillMovesWithinMultilineDraft(t *testing.T) {
	s, _ := newState(nil)
	s.inputHistory = []string{"older message"}
	s.textarea.SetText("first line\nsecond line")
	s.textarea.SetCursorPos(runeToClusterIndex(s.textarea.Text(), len([]rune(s.textarea.Text()))))

	s.handleKey(tui.KeyEvent{Key: tui.KeyUp})
	if got := s.textarea.Text(); got != "first line\nsecond line" {
		t.Fatalf("multiline navigation recalled history: %q", got)
	}
	if s.historyIndex != -1 {
		t.Fatalf("multiline navigation entered history at index %d", s.historyIndex)
	}
}

func TestPendingInputsAreRecordedInOrder(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.submitInput("queued follow-up", true)
	s.submitInput("priority correction", false)

	if len(s.pendingInputs) != 2 || s.pendingInputs[0].kind != "queued" || s.pendingInputs[1].kind != "steer" {
		t.Fatalf("pending inputs = %#v", s.pendingInputs)
	}
}

func TestSubmitRunsAsynchronously(t *testing.T) {
	release := make(chan struct{})
	s, runtime := newState(func(input string, _ <-chan string, _ func(toolEvent), _ context.Context) response {
		if input != "hello" {
			t.Errorf("input = %q", input)
		}
		<-release
		return response{Text: "answer", ContextTokens: 1234}
	})
	s.textarea.SetText("hello")
	s.submitInput(s.textarea.Text(), false)

	if !s.responding || s.textarea.Text() != "" {
		t.Fatalf("request did not start: responding=%v input=%q", s.responding, s.textarea.Text())
	}
	if len(s.messages) != 1 || s.messages[0].role != "you" {
		t.Fatalf("user message missing: %#v", s.messages)
	}

	close(release)
	runtime.runNext(t)
	if s.responding || s.contextTokens != 1234 {
		t.Fatalf("response did not finish: %#v", s)
	}
	if got := s.messages[len(s.messages)-2]; got.role != "agent" || got.text != "answer" {
		t.Fatalf("agent response missing: %#v", s.messages)
	}
	if got := s.messages[len(s.messages)-1]; got.role != "status" || !strings.HasPrefix(got.text, "Done in ") {
		t.Fatalf("completion duration missing: %#v", s.messages)
	}
}

func TestToolEventsAreAddedToTranscript(t *testing.T) {
	s, runtime := newState(func(_ string, _ <-chan string, emit func(toolEvent), _ context.Context) response {
		emit(toolEvent{Kind: toolEventCall, Name: "shell", ID: "call-1", Detail: "pwd"})
		emit(toolEvent{Kind: toolEventResult, Name: "shell", ID: "call-1", Detail: "/tmp"})
		return response{Text: "done"}
	})
	s.textarea.SetText("inspect")
	s.submitInput(s.textarea.Text(), false)

	for s.responding {
		runtime.runNext(t)
	}
	if len(s.messages) != 4 {
		t.Fatalf("messages = %#v", s.messages)
	}
	tool := s.messages[1]
	if tool.role != "tool" || tool.toolCommand != "pwd" || tool.toolResult != "/tmp" || tool.toolState != "success" {
		t.Fatalf("tool card was not updated in place: %#v", tool)
	}
}

func TestCompactionEventsAreAddedToTranscript(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.nextRequestID = 1
	s.handleToolEvent(1, toolEvent{Kind: toolEventCompactionStart})
	s.handleToolEvent(1, toolEvent{Kind: toolEventCompactionDone, Detail: "Compacted context at 200000 tokens."})

	if len(s.messages) != 2 || s.messages[0].role != "system" || s.messages[1].role != "status" {
		t.Fatalf("compaction transcript = %#v", s.messages)
	}
}

func TestToolUpdatesStreamIntoPendingCard(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.nextRequestID = 1
	s.handleToolEvent(1, toolEvent{Kind: toolEventCall, Name: "shell", ID: "call-1", Detail: "work"})
	s.handleToolEvent(1, toolEvent{Kind: toolEventUpdate, Name: "shell", ID: "call-1", Detail: "first"})
	s.handleToolEvent(1, toolEvent{Kind: toolEventUpdate, Name: "shell", ID: "call-1", Detail: " second"})

	tool := s.messages[0]
	if tool.toolState != "pending" || tool.toolResult != "first second" {
		t.Fatalf("streaming tool card = %#v", tool)
	}
	s.handleToolEvent(1, toolEvent{Kind: toolEventResult, Name: "shell", ID: "call-1", Detail: "first second"})
	if s.messages[0].toolState != "success" || s.messages[0].toolResult != "first second" {
		t.Fatalf("completed tool card = %#v", s.messages[0])
	}
}

func TestWebSearchToolCardShowsSearchInsteadOfShellCommand(t *testing.T) {
	got := renderedToolMessage(message{
		role: "tool", toolName: "web_search", toolCommand: "latest Go release", toolState: "success",
	}, 80, time.Now())
	plain := stripANSI(got)
	if !strings.Contains(plain, `web_search "latest Go release"`) || strings.Contains(plain, "$ latest Go release") {
		t.Fatalf("web search tool card = %q", plain)
	}
}

func TestToolResultsUpdateMatchingCallID(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.nextRequestID = 1
	s.handleToolEvent(1, toolEvent{Kind: toolEventCall, Name: "shell", ID: "first", Detail: "one"})
	s.handleToolEvent(1, toolEvent{Kind: toolEventCall, Name: "shell", ID: "second", Detail: "two"})
	s.handleToolEvent(1, toolEvent{Kind: toolEventResult, Name: "shell", ID: "first", Detail: "one-result"})

	if s.messages[0].toolResult != "one-result" || s.messages[0].toolState != "success" {
		t.Fatalf("first tool card was not updated: %#v", s.messages)
	}
	if s.messages[1].toolResult != "" || s.messages[1].toolState != "pending" {
		t.Fatalf("second tool card was changed: %#v", s.messages)
	}
}

func TestResponseErrorIsRetained(t *testing.T) {
	s, runtime := newState(func(string, <-chan string, func(toolEvent), context.Context) response {
		return response{Err: errors.New("request failed")}
	})
	s.textarea.SetText("hello")
	s.submitInput(s.textarea.Text(), false)
	for s.responding {
		runtime.runNext(t)
	}
	if got := s.messages[len(s.messages)-2]; got.role != "error" || got.text != "request failed" {
		t.Fatalf("error missing: %#v", s.messages)
	}
	if got := s.messages[len(s.messages)-1]; got.role != "status" || !strings.HasPrefix(got.text, "Done in ") {
		t.Fatalf("completion duration missing after error: %#v", s.messages)
	}
}

func TestEnterSteersAndShiftEnterQueues(t *testing.T) {
	s, _ := newState(func(string, <-chan string, func(toolEvent), context.Context) response { return response{} })
	s.responding = true

	s.textarea.SetText("queued follow-up")
	s.handleKey(tui.KeyEvent{Key: tui.KeyEnter, Mod: tui.ModShift})
	s.textarea.SetText("priority correction")
	s.handleKey(tui.KeyEvent{Key: tui.KeyEnter})

	if len(s.queued) != 1 || s.queued[0] != "queued follow-up" {
		t.Fatalf("queue = %#v", s.queued)
	}
	if len(s.pendingSteer) != 1 || s.pendingSteer[0] != "priority correction" {
		t.Fatalf("steer = %#v", s.pendingSteer)
	}
	if len(s.pendingInputs) != 2 || s.pendingInputs[0].kind != "queued" || s.pendingInputs[1].kind != "steer" {
		t.Fatalf("pending inputs = %#v", s.pendingInputs)
	}
}

func TestCtrlUpMovesPendingInputsBackToEditor(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.steer = make(chan string, 2)
	s.queued = []string{"later"}
	s.pendingSteer = []string{"correction"}
	s.pendingInputs = []pendingInput{{kind: "queued", text: "later"}, {kind: "steer", text: "correction"}}
	s.steer <- "correction"
	s.textarea.SetText("draft")

	s.handleKey(tui.KeyEvent{Key: tui.KeyUp, Mod: tui.ModCtrl})

	if got, want := s.textarea.Text(), "later\n\ncorrection\n\ndraft"; got != want {
		t.Fatalf("editor = %q, want %q", got, want)
	}
	if len(s.queued) != 0 || len(s.pendingSteer) != 0 || len(s.pendingInputs) != 0 || len(s.steer) != 0 {
		t.Fatalf("pending inputs remain: queued=%#v steer=%#v inputs=%#v channel=%d", s.queued, s.pendingSteer, s.pendingInputs, len(s.steer))
	}
	if !s.responding {
		t.Fatal("restoring pending inputs stopped the active response")
	}
}

func TestSteerIsDeliveredIntoActiveResponse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s, runtime := newState(func(_ string, steer <-chan string, emit func(toolEvent), _ context.Context) response {
		close(started)
		text := <-steer
		emit(toolEvent{Kind: toolEventSteerConsumed, Detail: text})
		<-release
		return response{Text: "continued"}
	})
	s.submitInput("initial", false)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("initial response did not start")
	}

	s.submitInput("change direction", false)
	runtime.runNext(t)
	if !s.responding {
		t.Fatal("steering ended the active response")
	}
	if len(s.pendingSteer) != 0 || len(s.pendingInputs) != 0 {
		t.Fatalf("consumed steer still pending: steer=%#v inputs=%#v", s.pendingSteer, s.pendingInputs)
	}
	if got := s.messages[len(s.messages)-1]; got.role != "you" || got.text != "change direction" {
		t.Fatalf("consumed steer was not added to transcript: %#v", s.messages)
	}

	close(release)
	for s.responding {
		runtime.runNext(t)
	}
}

func TestSteerRunsBeforeQueuedMessage(t *testing.T) {
	started := make(chan string, 2)
	s, _ := newState(func(input string, _ <-chan string, _ func(toolEvent), ctx context.Context) response {
		started <- input
		<-ctx.Done()
		return response{}
	})
	s.responding = true
	s.nextRequestID = 1
	s.queued = []string{"queued"}
	s.pendingSteer = []string{"steer"}
	s.pendingInputs = []pendingInput{{kind: "queued", text: "queued"}, {kind: "steer", text: "steer"}}

	s.finishResponse(response{id: 1, Text: "first"})
	select {
	case got := <-started:
		if got != "steer" {
			t.Fatalf("started %q, want steer", got)
		}
	case <-time.After(time.Second):
		t.Fatal("steer did not start")
	}
	if len(s.queued) != 1 {
		t.Fatal("queued message ran before steer")
	}
	s.cancelRequest()
}

func TestCtrlCCancelsThenExitsOnSecondPress(t *testing.T) {
	s, _ := newState(func(string, <-chan string, func(toolEvent), context.Context) response { return response{} })
	ctx, cancel := context.WithCancel(context.Background())
	s.responding = true
	s.cancel = cancel
	s.nextRequestID = 3
	s.messages = append(s.messages, message{role: "tool", toolCommand: "sleep 10", toolState: "pending"})
	s.render(60)
	if len(s.messages[0].renderedLines) == 0 {
		t.Fatal("pending tool render cache was not populated before cancellation")
	}
	ctrlC := tui.KeyEvent{Key: tui.KeyRune, Rune: 'c', Mod: tui.ModCtrl}

	if keepRunning := s.handleKey(ctrlC); !keepRunning {
		t.Fatal("first Ctrl+C exited")
	}
	if ctx.Err() == nil || s.responding || s.cancel != nil {
		t.Fatalf("request was not cancelled: %#v", s)
	}
	if len(s.messages) != 2 || s.messages[1].text != "Cancelled." {
		t.Fatalf("cancellation message missing: %#v", s.messages)
	}
	if s.messages[0].toolState != "error" || s.messages[0].toolResult != "Cancelled." {
		t.Fatalf("pending tool was not marked cancelled: %#v", s.messages[0])
	}
	after, _, _ := s.render(60)
	rendered := stripANSI(strings.Join(after, "\n"))
	if strings.Contains(rendered, "Running...") || !strings.Contains(rendered, "Cancelled.") {
		t.Fatalf("cancelled tool retained stale rendered content:\n%s", rendered)
	}
	if keepRunning := s.handleKey(ctrlC); keepRunning {
		t.Fatal("second Ctrl+C did not exit")
	}
}

func TestCtrlCExitSequenceExpiresOrIsInterrupted(t *testing.T) {
	s, _ := newState(nil)
	ctrlC := tui.KeyEvent{Key: tui.KeyRune, Rune: 'c', Mod: tui.ModCtrl}
	s.lastCtrlC = time.Now().Add(-ctrlCDoublePressInterval - time.Millisecond)
	if keepRunning := s.handleKey(ctrlC); !keepRunning {
		t.Fatal("expired Ctrl+C sequence exited")
	}
	s.handleKey(tui.KeyEvent{Key: tui.KeyRune, Rune: 'x'})
	if keepRunning := s.handleKey(ctrlC); !keepRunning {
		t.Fatal("interrupted Ctrl+C sequence exited")
	}
}

func TestEscapeClearsInputWithoutCancelling(t *testing.T) {
	s, _ := newState(nil)
	ctx, cancel := context.WithCancel(context.Background())
	s.responding = true
	s.cancel = cancel
	s.textarea.SetText("draft")

	if keepRunning := s.handleKey(tui.KeyEvent{Key: tui.KeyEscape}); !keepRunning {
		t.Fatal("Escape exited")
	}
	if got := s.textarea.Text(); got != "" {
		t.Fatalf("input = %q, want empty", got)
	}
	if ctx.Err() != nil || !s.responding {
		t.Fatal("Escape cancelled the active request")
	}
	cancel()
}

func TestCancelMovesPendingInputsBackToEditor(t *testing.T) {
	s, _ := newState(nil)
	_, cancel := context.WithCancel(context.Background())
	s.responding = true
	s.cancel = cancel
	s.queued = []string{"queued follow-up"}
	s.pendingSteer = []string{"steer correction"}
	s.pendingInputs = []pendingInput{
		{kind: "queued", text: "queued follow-up"},
		{kind: "steer", text: "steer correction"},
	}
	s.textarea.SetText("unfinished draft")

	s.cancelRequest()

	if got, want := s.textarea.Text(), "queued follow-up\n\nsteer correction\n\nunfinished draft"; got != want {
		t.Fatalf("editor = %q, want %q", got, want)
	}
	if len(s.queued) != 0 || len(s.pendingSteer) != 0 || len(s.pendingInputs) != 0 {
		t.Fatalf("pending inputs remain: queued=%#v steer=%#v inputs=%#v", s.queued, s.pendingSteer, s.pendingInputs)
	}
}

func TestCancelIgnoresStaleResponse(t *testing.T) {
	s, _ := newState(func(string, <-chan string, func(toolEvent), context.Context) response { return response{} })
	ctx, cancel := context.WithCancel(context.Background())
	s.responding = true
	s.cancel = cancel
	s.nextRequestID = 3

	s.cancelRequest()
	if ctx.Err() == nil || s.responding || s.cancel != nil {
		t.Fatalf("request was not cancelled: %#v", s)
	}
	if len(s.messages) != 1 || s.messages[0].text != "Cancelled." {
		t.Fatalf("cancellation message missing: %#v", s.messages)
	}
	s.finishResponse(response{id: 3, Text: "stale"})
	if len(s.messages) != 1 {
		t.Fatalf("stale response changed transcript: %#v", s.messages)
	}
}

func TestFormatTokenCount(t *testing.T) {
	for input, want := range map[int64]string{0: "0", 999: "999", 1000: "1k", 12345: "12k", 12500: "13k", 1234567: "1235k"} {
		if got := formatTokenCount(input); got != want {
			t.Fatalf("formatTokenCount(%d) = %q, want %q", input, got, want)
		}
	}
}

func TestRenderedMultilineErrorRestoresStylePerLine(t *testing.T) {
	got := renderedMessage(message{role: "error", text: "first\nsecond"}, 80)
	if strings.Count(got, "38;2;204;102;102") != 2 {
		t.Fatalf("multiline error did not style each line independently: %q", got)
	}
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("multiline error lost content: %q", got)
	}
}

func TestRenderedMessagesUsePiDarkTheme(t *testing.T) {
	tests := []struct {
		message message
		code    string
	}{
		{message{role: "you", text: "hello"}, "38;2;212;212;212;48;2;36;44;56"},
		{message{role: "reasoning", text: "thinking"}, "3;38;2;128;128;128"},
		{message{role: "tool", toolCommand: "pwd", toolState: "pending"}, "48;2;38;40;46"},
		{message{role: "tool", toolResult: "/tmp", toolState: "success"}, "38;2;128;128;128;48;2;38;40;46"},
		{message{role: "error", text: "failed"}, "38;2;204;102;102"},
		{message{role: "system", text: "cancelled"}, "38;5;242"},
		{message{role: "status", text: "Done in 1.2s"}, "38;2;181;189;104"},
	}
	for _, test := range tests {
		if got := renderedMessage(test.message, 80); !strings.Contains(got, test.code) {
			t.Errorf("rendered %s message missing Pi theme %q: %q", test.message.role, test.code, got)
		}
	}
}

func TestAssistantPlainTextUsesTerminalForeground(t *testing.T) {
	got := renderedMessage(message{role: "agent", text: "hello\n\n- one"}, 80)
	if strings.Contains(got, "\x1b[38;") {
		t.Fatalf("assistant plain text overrides terminal foreground: %q", got)
	}
}

func TestToolOutputCannotEmitTerminalControlSequences(t *testing.T) {
	got := renderedMessage(message{
		role:        "tool",
		toolCommand: "printf evil\033[2A",
		toolResult:  "first\rsecond\033[8AOVER\bWRITE\tend\a",
		toolState:   "success",
	}, 40)
	if strings.Contains(got, "\x1b[8A") || strings.Contains(got, "\x1b[2A") {
		t.Fatalf("tool card retained cursor movement: %q", got)
	}
	plain := stripANSI(got)
	for _, control := range []string{"\r", "\b", "\a", "\t"} {
		if strings.Contains(plain, control) {
			t.Fatalf("tool card retained control character %q: %q", control, plain)
		}
	}
	for i, line := range strings.Split(got, "\n") {
		if width := lineWidth(line); width > 40 {
			t.Fatalf("tool card line %d width = %d: %q", i, width, line)
		}
	}
}

func TestToolCardPreviewsHeadAndTail(t *testing.T) {
	got := stripANSI(renderedMessage(message{
		role: "tool", toolCommand: "many", toolResult: buildNumberedLines("OUTPUT", 12), toolState: "success",
	}, 40))
	if !strings.Contains(got, "OUTPUT-01") || !strings.Contains(got, "OUTPUT-12") || strings.Contains(got, "OUTPUT-06") {
		t.Fatalf("tool preview did not keep head and tail: %q", got)
	}
	if !strings.Contains(got, "⋯ 7 lines omitted ⋯") {
		t.Fatalf("tool preview lacks omitted-line count: %q", got)
	}
}

func TestStreamingToolOutputBufferIsBounded(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.nextRequestID = 1
	s.handleToolEvent(1, toolEvent{Kind: toolEventCall, Name: "shell", ID: "large", Detail: "many"})
	chunk := strings.Repeat("a", maxToolDisplayBytes+4096)
	s.handleToolEvent(1, toolEvent{Kind: toolEventUpdate, Name: "shell", ID: "large", Detail: chunk})
	if got := len(s.messages[0].toolResult); got > maxToolDisplayBytes {
		t.Fatalf("streaming tool buffer = %d bytes, limit = %d", got, maxToolDisplayBytes)
	}
}

func TestRenderFitsNarrowTerminal(t *testing.T) {
	for _, mode := range []string{"status", "working", "retry", "undo"} {
		for _, width := range []int{10, 20, 50, 80} {
			t.Run(fmt.Sprintf("%s/%d", mode, width), func(t *testing.T) {
				s, _ := newState(nil)
				s.requestStartedAt = time.Now().Add(-time.Hour)
				s.responding = mode == "working" || mode == "retry"
				if mode == "status" {
					s.messages = []message{{role: "status", text: strings.Repeat("x", width)}}
				}
				if mode == "retry" {
					s.retryAttempt = 1
					s.retryMaxAttempts = 3
					s.retryDeadline = time.Now().Add(time.Minute)
				}
				if mode == "undo" {
					s.undoOptions = []undoOption{{text: "previous prompt"}}
				}
				lines, cursorRow, cursorCol := s.render(width)
				r := newMainScreenRenderer(io.Discard, width, 10)
				if err := r.render(lines, cursorRow, cursorCol); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestWorkingDurationFormatsElapsedRequestTime(t *testing.T) {
	started := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		duration time.Duration
		want     string
	}{
		{350 * time.Millisecond, " (0s)"},
		{12 * time.Second, " (12s)"},
		{65 * time.Second, " (1m 05s)"},
		{3*time.Hour + 27*time.Minute + 46*time.Second, " (3h 27m)"},
		{24*time.Hour + 26*time.Minute, " (1d 26m)"},
	} {
		if got := workingDurationLabel(started, started.Add(test.duration)); got != test.want {
			t.Errorf("workingDurationLabel(%s) = %q, want %q", test.duration, got, test.want)
		}
	}

	s, _ := newState(nil)
	s.responding = true
	s.requestStartedAt = time.Now().Add(-12 * time.Second)
	lines, _, _ := s.render(60)
	if plain := stripANSI(strings.Join(lines, "\n")); !strings.Contains(plain, "Working… (12s)") {
		t.Fatalf("working status missing elapsed time: %q", plain)
	}
}

func TestCompletedRequestMessage(t *testing.T) {
	started := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	if got := completedRequestMessage(started, started.Add(1250*time.Millisecond)); got != "Done in 1s at 00:00" {
		t.Fatalf("completed request message = %q", got)
	}
	if got := completedRequestMessage(time.Time{}, started); got != "" {
		t.Fatalf("zero start produced completion message %q", got)
	}
}

func TestToolDurationFormatsAndPersists(t *testing.T) {
	for _, test := range []struct {
		duration time.Duration
		want     string
	}{
		{350 * time.Millisecond, "0s"},
		{1250 * time.Millisecond, "1s"},
		{65 * time.Second, "1m 05s"},
		{3*time.Hour + 27*time.Minute + 46*time.Second, "3h 27m"},
		{24*time.Hour + 26*time.Minute, "1d 26m"},
		{25*time.Hour + 26*time.Minute, "1d 01h"},
	} {
		if got := formatToolDuration(test.duration); got != test.want {
			t.Errorf("formatToolDuration(%s) = %q, want %q", test.duration, got, test.want)
		}
	}

	started := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	finished := started.Add(1250 * time.Millisecond)
	msg := message{
		role: "tool", toolCommand: "sleep 1", toolState: "success",
		toolStartedAt: started, toolFinishedAt: finished,
	}
	rendered := strings.Split(stripANSI(renderedMessage(msg, 40)), "\n")
	if len(rendered) < 3 || strings.TrimSpace(rendered[1]) != "(1s)" || strings.TrimSpace(rendered[2]) != "$ sleep 1" {
		t.Fatalf("completed tool duration is not on its own line above command: %q", rendered)
	}
	if got := toolDurationLabel(msg, finished.Add(time.Hour)); got != " (1s)" {
		t.Fatalf("completed duration changed after completion: %q", got)
	}
	pending := message{toolStartedAt: started}
	if got := toolDurationLabel(pending, started.Add(350*time.Millisecond)); got != " (0s)" {
		t.Fatalf("pending duration did not use current time: %q", got)
	}
	if got := toolDurationLabel(pending, started.Add(1350*time.Millisecond)); got != " (1s)" {
		t.Fatalf("pending duration did not advance by seconds: %q", got)
	}
}

func TestToolEventsRecordDuration(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.nextRequestID = 1
	s.handleToolEvent(1, toolEvent{Kind: toolEventCall, Name: "shell", ID: "timed", Detail: "work"})
	if s.messages[0].toolStartedAt.IsZero() || !s.messages[0].toolFinishedAt.IsZero() {
		t.Fatalf("pending tool timestamps = %#v", s.messages[0])
	}
	s.handleToolEvent(1, toolEvent{Kind: toolEventResult, Name: "shell", ID: "timed", Detail: "done"})
	if s.messages[0].toolFinishedAt.IsZero() || s.messages[0].toolFinishedAt.Before(s.messages[0].toolStartedAt) {
		t.Fatalf("completed tool timestamps = %#v", s.messages[0])
	}
}

func TestPiBoxLineRestoresBackgroundAfterNestedStyle(t *testing.T) {
	nested := ansiRGBStyle(piGreen, piToolBg, false, false, "✓ ") + "repl"
	got := piBoxLine(nested, 20, piGray, piToolBg, false)
	baseStyle := strings.TrimSuffix(ansiRGBStyle(piGray, piToolBg, false, false, ""), "\x1b[0m")
	if !strings.Contains(got, "✓ "+baseStyle+"repl") {
		t.Fatalf("box line does not restore its background after nested style: %q", got)
	}
	if lineWidth(got) != 20 {
		t.Fatalf("box line width = %d, want 20", lineWidth(got))
	}
}

func TestToolCardClarifiesContextOmission(t *testing.T) {
	got := stripANSI(renderedMessage(message{role: "tool", toolName: "repl", toolCommand: "print(1)", toolResult: agent.OmittedToolResult, toolState: "success"}, 60))
	if !strings.Contains(got, "[tool output omitted after use]") {
		t.Fatalf("tool card omission = %q", got)
	}
}

func TestToolCardUsesNeutralBackgroundAndStateColor(t *testing.T) {
	for state, color := range map[string]string{"success": piGreen, "error": piError} {
		got := renderedMessage(message{role: "tool", toolName: "repl", toolCommand: "print(1)", toolState: state}, 40)
		if !strings.Contains(got, "48;2;"+piToolBg) || !strings.Contains(got, "38;2;"+color) {
			t.Errorf("%s tool card lacks neutral background or state color: %q", state, got)
		}
	}
}

func TestUserMessageUsesFullWidthPiBoxWithoutTimestamp(t *testing.T) {
	got := renderedMessage(message{role: "you", text: "hello"}, 30)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("user box has %d lines, want vertical padding and content: %q", len(lines), got)
	}
	for i, line := range lines {
		if width := lineWidth(line); width != 30 {
			t.Errorf("user box line %d width = %d, want 30", i, width)
		}
	}
	if strings.Contains(stripANSI(got), "[") {
		t.Fatalf("user box contains a timestamp: %q", got)
	}
}

func TestUserMessageUsesDistinctBackgroundAndRail(t *testing.T) {
	got := renderedMessage(message{role: "you", text: "hello"}, 30)
	plain := stripANSI(got)
	if !strings.Contains(got, "48;2;"+piUserMessageBg) || !strings.Contains(plain, "▌") {
		t.Fatalf("user message lacks distinct background or rail: %q", got)
	}
	if strings.Contains(plain, "❯") {
		t.Fatalf("user message contains redundant prompt marker: %q", plain)
	}
}

func TestUserMessageAlignsWrappedLinesAfterRail(t *testing.T) {
	got := stripANSI(renderedMessage(message{role: "you", text: strings.Repeat("word ", 12)}, 20))
	lines := strings.Split(got, "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[1], "▌ ") || !strings.HasPrefix(lines[2], "▌ ") {
		t.Fatalf("wrapped user message is not aligned after rail: %q", got)
	}
}

func TestUserMessageRendersPlainText(t *testing.T) {
	got := renderedMessage(message{role: "you", text: "Hello **world** and `code`"}, 50)
	plain := stripANSI(got)
	if !strings.Contains(plain, "Hello **world** and `code`") {
		t.Fatalf("user text was changed: %q", plain)
	}
}

func TestMainScreenRendererClearsStartupEchoFromCurrentLine(t *testing.T) {
	var out strings.Builder
	r := newMainScreenRenderer(&out, 20, 4)
	if err := r.render([]string{"input"}, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "\r\x1b[2Kinput") {
		t.Fatalf("initial render did not clear the current line: %q", got)
	}
}

func TestMainScreenRendererValidatesOnlyChangedLines(t *testing.T) {
	r := newMainScreenRenderer(io.Discard, 10, 4)
	if err := r.render([]string{"history", "input"}, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.render([]string{"history", "input is too wide"}, 1, 0); err == nil {
		t.Fatal("changed over-width line was not rejected")
	}
}

func TestMainScreenRendererAppendsWithoutAlternateScreen(t *testing.T) {
	var out strings.Builder
	r := newMainScreenRenderer(&out, 20, 4)
	if err := r.render([]string{"one", "input"}, 1, 2); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := r.render([]string{"one", "two", "input"}, 2, 2); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "\x1b[?1049") {
		t.Fatalf("entered alternate screen: %q", got)
	}
	if strings.Contains(got, "\x1b[3J") {
		t.Fatalf("append cleared scrollback: %q", got)
	}
	if !strings.Contains(got, "two") {
		t.Fatalf("append missing new line: %q", got)
	}
}

func TestMainScreenRendererRetainsChangedLineAboveViewportWithoutReplay(t *testing.T) {
	var out strings.Builder
	r := newMainScreenRenderer(&out, 20, 3)
	lines := []string{"zero", "one", "two", "input"}
	if err := r.render(lines, 3, 0); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	changed := append([]string(nil), lines...)
	changed[0] = "ZERO"
	if err := r.render(changed, 3, 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b[3J") {
		t.Fatalf("off-screen edit caused a replay: %q", out.String())
	}
	if r.previousLines[0] != "ZERO" {
		t.Fatalf("off-screen state was not retained: %q", r.previousLines[0])
	}
}

func TestMainScreenRendererUpdatesViewportWithoutReplayingOffscreenChanges(t *testing.T) {
	var out strings.Builder
	r := newMainScreenRenderer(&out, 20, 3)
	if err := r.render([]string{"spinner 1", "one", "status 1", "input"}, 3, 0); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := r.render([]string{"spinner 2", "one", "status 2", "input"}, 3, 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b[3J") {
		t.Fatalf("visible update replayed an off-screen change: %q", out.String())
	}
	if !strings.Contains(out.String(), "status 2") {
		t.Fatalf("visible update was not rendered: %q", out.String())
	}
}

func TestMainScreenRendererResizeUsesPiStyleReplay(t *testing.T) {
	var out strings.Builder
	r := newMainScreenRenderer(&out, 20, 4)
	if err := r.render([]string{"history", "input"}, 1, 0); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	r.resize(10, 4)
	if err := r.render([]string{"history", "input"}, 1, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\x1b[2J\x1b[H\x1b[3J") {
		t.Fatalf("resize did not fully replay: %q", out.String())
	}
}

func TestStreamingDeltaLivesInRenderedModel(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.nextRequestID = 1
	s.handleToolEvent(1, toolEvent{Kind: toolEventTextDelta, Detail: "hello "})
	s.handleToolEvent(1, toolEvent{Kind: toolEventTextDelta, Detail: "world"})
	lines, _, _ := s.render(40)
	if !strings.Contains(stripANSI(strings.Join(lines, "\n")), "hello world") {
		t.Fatalf("stream missing from render: %q", lines)
	}
	if len(s.messages) != 0 {
		t.Fatalf("stream was finalized early: %#v", s.messages)
	}
}

func TestReasoningStreamUsesPiStyleAndPersists(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.nextRequestID = 1
	s.handleToolEvent(1, toolEvent{Kind: toolEventReasoningDelta, Detail: "Inspecting "})
	s.handleToolEvent(1, toolEvent{Kind: toolEventReasoningDelta, Detail: "the project"})

	lines, _, _ := s.render(50)
	rendered := strings.Join(lines, "\n")
	if !strings.Contains(rendered, "Inspecting the project") {
		t.Fatalf("reasoning missing from render: %q", lines)
	}
	if !strings.Contains(rendered, "3;38;2;128;128;128") || strings.Contains(rendered, "48;2;") {
		t.Fatalf("reasoning is not italic gray without a background: %q", rendered)
	}
	if len(s.messages) != 0 {
		t.Fatalf("reasoning was finalized while still streaming: %#v", s.messages)
	}

	s.handleToolEvent(1, toolEvent{Kind: toolEventReasoningDone})
	if s.reasoningText.Len() > 0 || len(s.messages) != 1 || s.messages[0].role != "reasoning" {
		t.Fatalf("reasoning was not finalized into transcript: state=%q messages=%#v", s.reasoningText.String(), s.messages)
	}
	lines, _, _ = s.render(50)
	if !strings.Contains(strings.Join(lines, "\n"), "Inspecting the project") {
		t.Fatalf("finalized reasoning disappeared: %q", lines)
	}
}

func TestOutputAndToolsFinalizeReasoning(t *testing.T) {
	for _, event := range []toolEvent{
		{Kind: toolEventTextDelta, Detail: "answer"},
		{Kind: toolEventCall, Name: "shell", Detail: "pwd"},
	} {
		s, _ := newState(nil)
		s.responding = true
		s.nextRequestID = 1
		s.reasoningText.WriteString("completed reasoning")
		s.handleToolEvent(1, event)
		if s.reasoningText.Len() > 0 {
			t.Errorf("%v event did not finish reasoning", event.Kind)
		}
		if len(s.messages) == 0 || s.messages[0].role != "reasoning" || s.messages[0].text != "completed reasoning" {
			t.Errorf("%v event did not retain reasoning: %#v", event.Kind, s.messages)
		}
	}
}

func TestStreamedContentPreservesReasoningTextAndToolOrder(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.nextRequestID = 1
	for _, event := range []toolEvent{
		{Kind: toolEventTextDelta, Detail: "before tool"},
		{Kind: toolEventReasoningDelta, Detail: "more thought"},
		{Kind: toolEventTextDelta, Detail: "after thought"},
		{Kind: toolEventCall, Name: "shell", ID: "call-1", Detail: "pwd"},
	} {
		s.handleToolEvent(1, event)
	}
	roles := make([]string, len(s.messages))
	for i, message := range s.messages {
		roles[i] = message.role
	}
	if got := strings.Join(roles, ","); got != "agent,reasoning,agent,tool" {
		t.Fatalf("content order = %s, messages = %#v", got, s.messages)
	}
	if s.messages[0].text != "before tool" || s.messages[2].text != "after thought" {
		t.Fatalf("text blocks were not retained: %#v", s.messages)
	}
}

func TestAssistantRendersMarkdownAndReasoningRemainsPlain(t *testing.T) {
	assistant := renderedMessage(message{role: "agent", text: "## Result\n\nUse **bold** and `code`.\n\n- one\n- two\n\n```go\nfmt.Println(1)\n```"}, 50)
	plain := stripANSI(assistant)
	for _, want := range []string{"Result", "Use bold and code.", "• one", "• two", "fmt.Println(1)"} {
		if !strings.Contains(plain, want) {
			t.Errorf("rendered markdown missing %q: %q", want, plain)
		}
	}
	if strings.Contains(plain, "**bold**") || strings.Contains(plain, "```") {
		t.Fatalf("markdown syntax remained visible: %q", plain)
	}
	if !strings.Contains(assistant, ";1m") || !strings.Contains(assistant, "38;2;138;190;183m") {
		t.Fatalf("bold or inline code styling missing: %q", assistant)
	}

	reasoning := renderedMessage(message{role: "reasoning", text: "Check **carefully**"}, 50)
	if plain := stripANSI(reasoning); !strings.Contains(plain, "Check **carefully**") {
		t.Fatalf("reasoning text was changed: %q", plain)
	}
	if !strings.Contains(reasoning, "\x1b[3;38;2;128;128;128m") {
		t.Fatalf("reasoning is not italic gray: %q", reasoning)
	}
}

func TestWorkingAndPendingLabelsUsePiWording(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.pendingInputs = []pendingInput{{kind: "steer", text: "fix"}, {kind: "queued", text: "later"}}
	lines, _, _ := s.render(60)
	plain := stripANSI(strings.Join(lines, "\n"))
	for _, want := range []string{"Working…", "Steering: fix", "Queued: later", "↳ Ctrl+↑ to edit all queued messages"} {
		if !strings.Contains(plain, want) {
			t.Errorf("render missing %q: %q", want, plain)
		}
	}
	for _, line := range strings.Split(plain, "\n") {
		for _, label := range []string{"Working…", "Steering:", "Queued:"} {
			if strings.Contains(line, label) && strings.HasPrefix(line, " ") {
				t.Errorf("%s line has left padding: %q", label, line)
			}
		}
	}
}

func TestLongEditorKeepsCursorInLogicalOutput(t *testing.T) {
	s, _ := newState(nil)
	s.textarea.SetText(strings.Repeat("abcdefghij", 20))
	lines, row, col := s.render(20)
	if row < 1 || row >= len(lines)-1 {
		t.Fatalf("cursor row %d outside editor in %d lines", row, len(lines))
	}
	if col < 2 || col >= 20 {
		t.Fatalf("cursor column = %d", col)
	}
}

func TestEditorCursorUsesGraphemeClusters(t *testing.T) {
	text := "a👩‍💻b"
	_, row, col := renderEditor(text, 2, 20) // after a + one ZWJ cluster
	if row != 1 || col != 5 {                // border+padding (2), a (1), emoji (2)
		t.Fatalf("cursor = (%d,%d), want (1,5)", row, col)
	}
	if got := clusterToRuneIndex(text, 2); got != 4 {
		t.Fatalf("clusterToRuneIndex = %d, want 4", got)
	}
}

func TestLineWidthIgnoresANSI(t *testing.T) {
	if got := lineWidth("\x1b[38;5;39mhello\x1b[0m"); got != 5 {
		t.Fatalf("lineWidth = %d, want 5", got)
	}
}

func TestMainScreenRendererStopDoesNotOverwriteCursorCell(t *testing.T) {
	var out strings.Builder
	r := newMainScreenRenderer(&out, 20, 4)
	if err := r.render([]string{"input", "footer"}, 0, 0); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(out.String(), " ") {
		t.Fatalf("stop overwrote the cell under the hardware cursor: %q", out.String())
	}
}

func TestShortDocumentFillsAndBottomAlignsViewport(t *testing.T) {
	s, _ := newState(nil)
	lines, row, _ := s.render(40, 12)
	if len(lines) != 12 {
		t.Fatalf("rendered height = %d, want 12", len(lines))
	}
	if row != 9 { // blank padding, top border, then editor content row
		t.Fatalf("cursor row = %d, want 9", row)
	}
	if lines[0] != "" {
		t.Fatalf("first row = %q, want viewport filler", lines[0])
	}
	footer := stripANSI(lines[len(lines)-1])
	if !strings.Contains(footer, "test-model (medium)") {
		t.Fatalf("footer is not bottom-aligned: %q", lines[len(lines)-1])
	}
	if strings.HasPrefix(footer, "model ") {
		t.Fatalf("footer retains redundant model label: %q", footer)
	}
}

func TestFooterIsSimplifiedAndQuiet(t *testing.T) {
	footer := renderFooter("gpt-5", "high", 12345, "/tmp/project", "abc123", 100)
	if got, want := stripANSI(footer), "gpt-5 (high)  ·  12k context  ·  /tmp/project  ·  abc123"; got != want {
		t.Fatalf("footer text = %q, want %q", got, want)
	}
	if strings.Contains(footer, "38;2;181;189;104") || strings.Contains(footer, "38;2;138;190;183") {
		t.Fatalf("footer uses decorative accents: %q", footer)
	}
}

func TestStreamingMarkdownCacheTracksContentAndWidth(t *testing.T) {
	s, _ := newState(nil)
	s.responding = true
	s.nextRequestID = 1
	s.handleToolEvent(1, toolEvent{Kind: toolEventTextDelta, Detail: "hello"})
	first := s.renderedStreamingText(40)
	if again := s.renderedStreamingText(40); &again[0] != &first[0] {
		t.Fatal("unchanged streaming Markdown was rendered again")
	}
	s.handleToolEvent(1, toolEvent{Kind: toolEventTextDelta, Detail: " **world**"})
	updated := s.renderedStreamingText(40)
	if &updated[0] == &first[0] || !strings.Contains(stripANSI(strings.Join(updated, "\n")), "hello world") {
		t.Fatalf("streaming Markdown cache was not refreshed after a delta: %q", updated)
	}
	resized := s.renderedStreamingText(60)
	if &resized[0] == &updated[0] {
		t.Fatal("streaming Markdown cache was not refreshed after a resize")
	}
}

func TestFinalizedMessagesCacheRenderedLinesByWidth(t *testing.T) {
	s, _ := newState(nil)
	s.messages = []message{{role: "agent", text: "**cached** Markdown"}}
	s.render(40)
	first := s.messages[0].renderedLines
	if len(first) == 0 || s.messages[0].renderedWidth != 40 {
		t.Fatalf("render cache was not populated: %#v", s.messages[0])
	}
	s.render(40)
	if &s.messages[0].renderedLines[0] != &first[0] {
		t.Fatal("unchanged message was rendered again")
	}
	s.render(60)
	if s.messages[0].renderedWidth != 60 || &s.messages[0].renderedLines[0] == &first[0] {
		t.Fatal("width change did not refresh the render cache")
	}
}

func TestLongDocumentIsNotViewportPadded(t *testing.T) {
	s, _ := newState(nil)
	for i := 0; i < 20; i++ {
		s.messages = append(s.messages, message{role: "agent", text: "line"})
	}
	lines, _, _ := s.render(40, 12)
	if len(lines) <= 12 {
		t.Fatalf("long document height = %d, want > 12", len(lines))
	}
	if lines[0] == "" {
		t.Fatal("long document was incorrectly prefixed with viewport filler")
	}
}

func TestRetryShowsPiStyleCountdownAndEscapeCancels(t *testing.T) {
	s, _ := newState(nil)
	ctx, cancel := context.WithCancel(t.Context())
	s.responding = true
	s.nextRequestID = 1
	s.cancel = cancel
	s.handleToolEvent(1, toolEvent{Kind: toolEventTextReset})
	s.handleToolEvent(1, toolEvent{Kind: toolEventReasoningDelta, Detail: "partial reasoning"})
	s.handleToolEvent(1, toolEvent{Kind: toolEventTextDelta, Detail: "partial failed response"})

	s.handleToolEvent(1, toolEvent{Kind: toolEventRetry, Detail: "server temporarily unavailable", Attempt: 1, MaxAttempts: 3, Delay: 2 * time.Second})
	if s.streamingText.Len() > 0 || s.reasoningText.Len() > 0 || len(s.messages) != 1 {
		t.Fatalf("failed attempt was not replaced by one error card: text=%q reasoning=%q messages=%#v", s.streamingText.String(), s.reasoningText.String(), s.messages)
	}
	lines, _, _ := s.render(80)
	rendered := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(rendered, "LLM error · retry 1/3") || !strings.Contains(rendered, "server temporarily unavailable") {
		t.Fatalf("retry error card missing:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Retrying (1/3) in 2s... (Esc to cancel)") {
		t.Fatalf("retry status missing:\n%s", rendered)
	}

	// A later failure updates the same card instead of appending another one.
	s.handleToolEvent(1, toolEvent{Kind: toolEventTextReset})
	s.handleToolEvent(1, toolEvent{Kind: toolEventTextDelta, Detail: "another partial response"})
	s.handleToolEvent(1, toolEvent{Kind: toolEventAttemptFailed})
	s.handleToolEvent(1, toolEvent{Kind: toolEventRetry, Detail: "connection reset", Attempt: 2, MaxAttempts: 3, Delay: 4 * time.Second})
	if len(s.messages) != 1 || s.messages[0].toolName != "LLM error · retry 2/3" || s.messages[0].toolResult != "connection reset" {
		t.Fatalf("retry error card was not updated in place: %#v", s.messages)
	}
	lines, _, _ = s.render(80)
	rendered = stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(rendered, "connection reset") {
		t.Fatalf("updated retry card retained stale rendered content:\n%s", rendered)
	}

	s.handleKey(tui.KeyEvent{Key: tui.KeyEscape})
	if s.responding || s.retryAttempt != 0 {
		t.Fatalf("retry was not cancelled: responding=%v attempt=%d", s.responding, s.retryAttempt)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Escape did not cancel the request context")
	}
	if got := s.messages[len(s.messages)-1]; got.role != "system" || got.text != "Cancelled." {
		t.Fatalf("cancellation message = %#v", got)
	}
}

func BenchmarkRenderLongTranscript(b *testing.B) {
	s, _ := newState(nil)
	for i := 0; i < 250; i++ {
		s.messages = append(s.messages,
			message{role: "you", text: fmt.Sprintf("Request %d: inspect the implementation and explain the relevant behavior.", i)},
			message{role: "agent", text: fmt.Sprintf("## Result %d\n\nThe implementation keeps the transcript responsive while preserving **Markdown**, terminal history, and tool output.\n\n```go\nresult := %d\n```", i, i)},
		)
	}
	s.textarea.SetText("editing a new request")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s.render(100, 30)
	}
}

func BenchmarkRenderFrameLongTranscript(b *testing.B) {
	s, _ := newState(nil)
	for i := 0; i < 250; i++ {
		s.messages = append(s.messages,
			message{role: "you", text: fmt.Sprintf("Request %d: inspect the implementation and explain the relevant behavior.", i)},
			message{role: "agent", text: fmt.Sprintf("## Result %d\n\nThe implementation keeps the transcript responsive while preserving **Markdown**, terminal history, and tool output.", i)},
		)
	}
	r := newMainScreenRenderer(io.Discard, 100, 30)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		s.textarea.SetText(fmt.Sprintf("editing request %d", i))
		lines, row, col := s.render(100, 30)
		if err := r.render(lines, row, col); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderStreamingToolOutput(b *testing.B) {
	s, _ := newState(nil)
	s.messages = []message{{
		role: "tool", toolID: "call", toolCommand: "produce output", toolState: "pending",
		toolResult: strings.Repeat("streamed tool output with enough text to wrap across the terminal width\n", 700),
	}}
	r := newMainScreenRenderer(io.Discard, 100, 30)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s.handleToolActivity(toolEvent{Kind: toolEventUpdate, ID: "call", Detail: "next output line\n"})
		lines, row, col := s.render(100, 30)
		if err := r.render(lines, row, col); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderStreamingMarkdown(b *testing.B) {
	s, _ := newState(nil)
	s.responding = true
	s.streamingText.WriteString(strings.Repeat("A paragraph with **formatted text** and `inline code` that streams from the model.\n\n", 600))
	r := newMainScreenRenderer(io.Discard, 100, 30)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s.streamingText.WriteString("more text ")
		lines, row, col := s.render(100, 30)
		if err := r.render(lines, row, col); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamingTextAssembly(b *testing.B) {
	chunk := strings.Repeat("x", 32)
	b.ReportAllocs()
	for range b.N {
		s, _ := newState(nil)
		s.responding = true
		s.nextRequestID = 1
		for range 2000 {
			s.handleToolEvent(1, toolEvent{Kind: toolEventTextDelta, Detail: chunk})
		}
	}
}

func BenchmarkRenderUnchangedStreamingMarkdown(b *testing.B) {
	s, _ := newState(nil)
	s.responding = true
	s.streamingText.WriteString(strings.Repeat("A paragraph with **formatted text** and `inline code`.\n\n", 800))
	r := newMainScreenRenderer(io.Discard, 100, 30)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		lines, row, col := s.render(100, 30)
		if err := r.render(lines, row, col); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderUnchangedToolOutput(b *testing.B) {
	s, _ := newState(nil)
	s.messages = []message{{
		role: "tool", toolID: "call", toolCommand: "produce output", toolState: "pending",
		toolStartedAt: time.Now(),
		toolResult:    strings.Repeat("streamed tool output with enough text to wrap across the terminal width\n", 700),
	}}
	r := newMainScreenRenderer(io.Discard, 100, 30)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		lines, row, col := s.render(100, 30)
		if err := r.render(lines, row, col); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRenderedMarkdownNumbersLooseOrderedLists(t *testing.T) {
	lines := renderedMarkdownLines("1. first\n\n1. second\n\n1. third", 40)
	got := stripANSI(strings.Join(lines, "\n"))
	for _, item := range []string{"1. first", "2. second", "3. third"} {
		if !strings.Contains(got, item) {
			t.Errorf("rendered list missing %q:\n%s", item, got)
		}
	}
}

func TestRenderedMarkdownKeepsLooseListDescriptionsInTheirItems(t *testing.T) {
	input := `1. Context compaction and session persistence.
   These are the largest practical gaps.

1. Cancellation-history correctness.
   A cancelled tool call must not corrupt history.

1. Runtime configuration eventually.
   Environment variables or flags would be enough.

1. Keep parallel tool execution optional or absent.
   Concurrency would complicate event ordering.

Overall: already a credible personal daily-driver.`
	got := stripANSI(strings.Join(renderedMarkdownLines(input, 60), "\n"))
	for _, want := range []string{
		"1. Context compaction",
		"2. Cancellation-history",
		"3. Runtime configuration",
		"4. Keep parallel tool",
		"These are the",
		"largest practical gaps.",
		"Overall: already a credible personal daily-driver.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered list missing %q:\n%s", want, got)
		}
	}
}

func TestRenderedMarkdownShowsLinkDestination(t *testing.T) {
	rendered := strings.Join(renderedMarkdownLines("Read [the docs](https://example.com/docs).", 60), "\n")
	plain := stripANSI(rendered)
	if !strings.Contains(plain, "the docs https://example.com/docs") {
		t.Fatalf("rendered link does not show its destination: %q", plain)
	}
	if !strings.Contains(rendered, ";https://example.com/docs\a") {
		t.Fatalf("rendered link is not clickable: %q", rendered)
	}
}

func TestRenderedMarkdownPreservesAngleBracketText(t *testing.T) {
	input := "ECHO<queued-order> and <b>literal HTML</b>\n\n```go\nif a < b {}\n```"
	plain := stripANSI(strings.Join(renderedMarkdownLines(input, 80), "\n"))
	for _, want := range []string{"ECHO<queued-order>", "<b>literal HTML</b>", "if a < b {}"} {
		if !strings.Contains(plain, want) {
			t.Errorf("rendered markdown missing %q: %q", want, plain)
		}
	}
}

func TestRenderedMarkdownFitsWidth(t *testing.T) {
	inputs := []string{
		"This is a long paragraph that must wrap without exceeding the requested terminal width.",
		"1. A long list item that must wrap and retain its continuation indentation.",
		"[documentation](https://example.com/a/very/long/documentation/path)",
		"```go\nfmt.Println(\"a very long code line that exceeds the width\")\n```",
		"```\n" + strings.Repeat("-", 100) + "\n```",
	}
	for _, input := range inputs {
		lines := renderedMarkdownLines(input, 24)
		for _, line := range lines {
			if width := lineWidth(line); width > 25 {
				t.Fatalf("rendered line width = %d, want at most 25: %q", width, line)
			}
		}
	}
}

func TestRenderedMarkdownUsesPiColors(t *testing.T) {
	rendered := strings.Join(renderedMarkdownLines("# Heading\n\n[docs](https://example.com)\n\n- item\n\n> quote", 60), "\n")
	for name, color := range map[string]string{
		"heading":     "38;2;240;198;116",
		"link":        "38;2;129;162;190",
		"link URL":    "38;2;102;102;102",
		"list bullet": "38;2;128;128;128",
		"quote":       "38;2;128;128;128",
	} {
		if !strings.Contains(rendered, color) {
			t.Errorf("rendered Markdown missing Pi %s color %q: %q", name, color, rendered)
		}
	}
}

func TestHighlightPythonLines(t *testing.T) {
	line := `result = shell("pwd") # comment`
	highlighted := highlightedPythonLines(line, 80, piToolBg)
	if len(highlighted) != 1 || stripANSI(highlighted[0]) != line || !strings.Contains(highlighted[0], "\x1b[38;2;") {
		t.Fatalf("highlighted Python = %#v", highlighted)
	}
}

func TestRenderREPLToolAsCodeCell(t *testing.T) {
	msg := message{
		role: "tool", toolName: "repl", toolCommand: "value = shell(\"pwd\")", toolState: "success",
	}
	plain := stripANSI(renderedMessage(msg, 80))
	if !strings.Contains(plain, "repl") || !strings.Contains(plain, `value = shell("pwd")`) || strings.Contains(plain, ">>>") {
		t.Fatalf("REPL tool rendering = %q", plain)
	}
}

func TestRestoreConversationPopulatesTranscriptAndInputHistory(t *testing.T) {
	s := newUI("model", "medium", nil)
	s.restoreConversation([]agent.ConversationMessage{
		{Role: "user", Text: "inspect the project"},
		{Role: "assistant", Text: "It looks good."},
	})
	if len(s.messages) != 2 || s.messages[0].role != "you" || s.messages[1].role != "agent" {
		t.Fatalf("restored messages = %#v", s.messages)
	}
	if len(s.inputHistory) != 1 || s.inputHistory[0] != "inspect the project" {
		t.Fatalf("restored input history = %#v", s.inputHistory)
	}
}

func TestREPLRecoveryNoticeIsRetained(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%v", cancelled), func(t *testing.T) {
			s, _ := newState(nil)
			s.responding = true
			s.nextRequestID = 1
			if cancelled {
				s.cancel = func() {}
				s.cancelRequest()
			}
			s.handleToolEvent(1, toolEvent{Kind: toolEventREPLRecovery, Detail: "Python REPL restarted from last checkpoint"})
			notice := s.messages[len(s.messages)-1]
			if notice.role != "status" || !strings.Contains(notice.text, "restarted from last checkpoint") {
				t.Fatalf("recovery notice missing: %#v", s.messages)
			}
		})
	}
}
