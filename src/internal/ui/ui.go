package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	tui "github.com/grindlemire/go-tui"

	"github.com/Baitinq/fn-agent/src/internal/agent"
	"github.com/Baitinq/fn-agent/src/internal/assert"
)

type message struct {
	role, text                    string
	toolID, toolName, toolCommand string
	toolResult, toolState         string
	toolProgress                  string
	toolStartedAt, toolFinishedAt time.Time
	renderedWidth                 int
	renderedLines                 []string
	renderedToolDuration          string
}

func (m *message) invalidateRender() {
	m.renderedLines = nil
}

type response struct {
	id            int
	Text          string
	ContextTokens int64
	Err           error
}
type toolEvent = agent.ToolEvent

const (
	toolEventAttemptFailed          = agent.ToolEventAttemptFailed
	toolEventCompactionStart        = agent.ToolEventCompactionStart
	toolEventCompactionDone         = agent.ToolEventCompactionDone
	toolEventREPLRecovery           = agent.ToolEventREPLRecovery
	toolEventCompactionFailed       = agent.ToolEventCompactionFailed
	toolEventRetry                  = agent.ToolEventRetry
	toolEventRetryDone              = agent.ToolEventRetryDone
	toolEventTextReset              = agent.ToolEventTextReset
	toolEventToolResultsPruned      = agent.ToolEventToolResultsPruned
	toolEventTransientHistoryPruned = agent.ToolEventTransientHistoryPruned
	toolEventReasoningDelta         = agent.ToolEventReasoningDelta
	toolEventReasoningDone          = agent.ToolEventReasoningDone
	toolEventSteerConsumed          = agent.ToolEventSteerConsumed
	toolEventContextTokens          = agent.ToolEventContextTokens
	toolEventTextDelta              = agent.ToolEventTextDelta
	toolEventCall                   = agent.ToolEventCall
	toolEventUpdate                 = agent.ToolEventUpdate
	toolEventProgress               = agent.ToolEventProgress
	toolEventResult                 = agent.ToolEventResult
	toolEventError                  = agent.ToolEventError
)

type pendingInput struct{ kind, text string }

type undoOption struct {
	text         string
	messageIndex int
}

// Commands contains session operations handled by the interactive UI.
type Commands struct {
	Undo func(int) (string, error)
	Fork func() (string, error)
}

type fnUI struct {
	modelName              string
	cwd                    string
	sessionID              string
	reasoningEffort        string
	contextTokens          int64
	respond                func(string, <-chan string, func(toolEvent), context.Context) response
	textarea               *tui.TextArea
	textareaWidth          int
	messages               []message
	streamingText          strings.Builder
	streamingRenderedWidth int
	streamingRenderedLines []string
	frameLines             []string
	liveStart              int
	reasoningText          strings.Builder
	responding             bool
	spinnerFrame           int
	retryAttempt           int
	retryMaxAttempts       int
	retryDeadline          time.Time
	requestStartedAt       time.Time
	retryMessageIndex      int
	attemptMessageStart    int
	requestMessageStart    int
	queued                 []string
	pendingSteer           []string
	steer                  chan string
	pendingInputs          []pendingInput
	inputHistory           []string
	historyIndex           int
	historyDraft           string
	nextRequestID          int
	lastCtrlC              time.Time
	cancel                 context.CancelFunc
	dispatch               func(func())
	invalidate             func()
	emit                   func(message)
	commands               Commands
	undoOptions            []undoOption
	undoSelected           int
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	spinnerInterval          = 80 * time.Millisecond
	ctrlCDoublePressInterval = time.Second
	// Background streams can produce many updates per second. Rendering each
	// one is especially expensive for growing tool cards because the card pushes
	// the spinner and editor down. Cap background rendering at 60 frames per
	// second; direct input and resize events bypass this limit below.
	backgroundRenderInterval = time.Second / 60
	eventPollInterval        = 10 * time.Millisecond
	maxUpdatesPerIteration   = 64
	toolOutputLines          = 10
	maxToolProgressBytes     = 16 * 1024
)

func newUI(modelName, reasoningEffort string, respond func(string, <-chan string, func(toolEvent), context.Context) response) *fnUI {
	s := &fnUI{
		modelName: modelName, reasoningEffort: reasoningEffort, respond: respond,
		historyIndex: -1, retryMessageIndex: -1,
	}
	s.ensureTextarea()
	s.emit = func(message) {}
	return s
}

func (s *fnUI) ensureTextarea() {
	s.setTextareaWidth(76)
}

func (s *fnUI) setTextareaWidth(width int) {
	width = max(width, 1)
	if s.textarea != nil && s.textareaWidth == width {
		return
	}
	text, cursor := "", 0
	if s.textarea != nil {
		text, cursor = s.textarea.Text(), s.textarea.CursorPos()
	}
	s.textarea = tui.NewTextArea(tui.WithTextAreaWidth(width), tui.WithTextAreaAutoFocus(true))
	s.textarea.SetText(text)
	s.textarea.SetCursorPos(cursor)
	s.textareaWidth = width
}

func (s *fnUI) submitInput(text string, queue bool) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return true
	}
	if text == "/undo" || text == "/fork" || text == "/exit" {
		return s.runCommand(text)
	}
	s.inputHistory = append(s.inputHistory, text)
	s.historyIndex, s.historyDraft = -1, ""
	s.textarea.Clear()
	if !s.responding {
		s.startRequest(text, true)
		return true
	}
	if queue {
		s.queued = append(s.queued, text)
		s.pendingInputs = append(s.pendingInputs, pendingInput{"queued", text})
		s.markDirty()
		return true
	}
	s.pendingSteer = append(s.pendingSteer, text)
	s.pendingInputs = append(s.pendingInputs, pendingInput{"steer", text})
	if s.steer != nil {
		select {
		case s.steer <- text:
		default:
		}
	}
	s.markDirty()
	return true
}

func (s *fnUI) runCommand(command string) bool {
	assert.That(command == "/undo" || command == "/fork" || command == "/exit", "unknown session command")
	s.textarea.Clear()
	s.historyIndex, s.historyDraft = -1, ""
	if command == "/exit" {
		return false
	}
	if s.responding {
		s.addMessage(message{role: "error", text: command + " is unavailable while a response is running."})
		return true
	}
	switch command {
	case "/undo":
		assert.That(s.commands.Undo != nil, "undo command has no handler")
		s.undoOptions = s.undoOptions[:0]
		for i := range s.messages {
			if s.messages[i].role == "you" {
				s.undoOptions = append(s.undoOptions, undoOption{s.messages[i].text, i})
			}
		}
		if len(s.undoOptions) == 0 {
			s.addMessage(message{role: "error", text: "nothing to undo"})
			return true
		}
		s.undoSelected = len(s.undoOptions) - 1
		s.markDirty()
	case "/fork":
		assert.That(s.commands.Fork != nil, "fork command has no handler")
		id, err := s.commands.Fork()
		if err != nil {
			s.addMessage(message{role: "error", text: err.Error()})
			return true
		}
		assert.That(id != "", "fork command returned an empty session ID")
		assert.That(id != s.sessionID, "fork command returned the current session ID")
		s.sessionID = id
		s.addMessage(message{role: "system", text: "Forked session " + id + "."})
	}
	return true
}

func (s *fnUI) startRequest(text string, showUser bool) {
	assert.That(s.respond != nil, "start request without responder")
	assert.That(s.dispatch != nil, "start request without dispatcher")
	s.nextRequestID++
	id := s.nextRequestID
	ctx, cancel := context.WithCancel(context.Background())
	steer := make(chan string, 256)
	s.responding, s.spinnerFrame, s.cancel = true, 0, cancel
	s.resetStreamingText()
	s.reasoningText.Reset()
	s.requestStartedAt = time.Now()
	s.retryAttempt, s.retryMaxAttempts, s.retryDeadline = 0, 0, time.Time{}
	s.retryMessageIndex = -1
	s.steer = steer
	s.requestMessageStart = len(s.messages)
	if showUser {
		s.addMessage(message{role: "you", text: text})
	}
	s.markDirty()
	finished := make(chan struct{})
	go s.spin(ctx, finished, id)
	go func() {
		emit := func(ev toolEvent) { s.dispatch(func() { s.handleToolEvent(id, ev) }) }
		resp := s.respond(text, steer, emit, ctx)
		resp.id = id
		close(finished)
		s.dispatch(func() { s.finishResponse(resp) })
	}()
}

func (s *fnUI) spin(ctx context.Context, finished <-chan struct{}, id int) {
	t := time.NewTicker(spinnerInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-finished:
			return
		case <-t.C:
			s.dispatch(func() {
				if s.responding && s.nextRequestID == id {
					s.spinnerFrame = (s.spinnerFrame + 1) % len(spinnerFrames)
					s.markDirty()
				}
			})
		}
	}
}

func (s *fnUI) handleToolEvent(id int, ev toolEvent) {
	// Recovery remains relevant after the originating turn was cancelled.
	if ev.Kind == toolEventREPLRecovery {
		s.addMessage(message{role: "system", text: "⚠ " + ev.Detail})
		return
	}
	if id != s.nextRequestID || !s.responding {
		return
	}
	switch ev.Kind {
	case toolEventContextTokens:
		s.contextTokens = ev.ContextTokens
		s.markDirty()
		return
	case toolEventCompactionStart:
		s.addMessage(message{role: "system", text: "Compacting context…"})
		return
	case toolEventCompactionDone:
		s.addMessage(message{role: "status", text: ev.Detail})
		return
	case toolEventCompactionFailed:
		s.addMessage(message{role: "error", text: "Context compaction failed: " + ev.Detail})
		return
	case toolEventAttemptFailed:
		s.reasoningText.Reset()
		s.resetStreamingText()
		if s.attemptMessageStart <= len(s.messages) {
			s.messages = s.messages[:s.attemptMessageStart]
		}
		s.markDirty()
		return
	case toolEventRetry:
		// A failed stream may have emitted partial reasoning, text, or tool
		// calls. Drop that partial attempt, but keep one persistent error card
		// and update it with the latest failure on every retry.
		s.reasoningText.Reset()
		s.resetStreamingText()
		if s.attemptMessageStart <= len(s.messages) {
			s.messages = s.messages[:s.attemptMessageStart]
		}
		title := fmt.Sprintf("LLM error · retry %d/%d", ev.Attempt, ev.MaxAttempts)
		if s.retryMessageIndex >= 0 && s.retryMessageIndex < len(s.messages) {
			msg := &s.messages[s.retryMessageIndex]
			msg.toolName, msg.toolResult = title, ev.Detail
			msg.toolFinishedAt = time.Now()
			msg.invalidateRender()
		} else {
			s.retryMessageIndex = len(s.messages)
			s.addMessage(message{
				role: "tool", toolID: "llm-retry", toolName: title,
				toolResult: ev.Detail, toolState: "error", toolFinishedAt: time.Now(),
			})
		}
		s.retryAttempt, s.retryMaxAttempts = ev.Attempt, ev.MaxAttempts
		s.retryDeadline = time.Now().Add(ev.Delay)
		s.markDirty()
		return
	case toolEventRetryDone:
		s.retryAttempt, s.retryMaxAttempts, s.retryDeadline = 0, 0, time.Time{}
		s.markDirty()
		return
	case toolEventTextReset:
		// The backoff has ended and a fresh request is now active.
		s.retryAttempt, s.retryMaxAttempts, s.retryDeadline = 0, 0, time.Time{}
		s.finishReasoning()
		s.finishStreamingText()
		s.attemptMessageStart = len(s.messages)
		return
	case toolEventToolResultsPruned:
		for i := range s.messages {
			msg := &s.messages[i]
			if msg.role == "tool" && msg.toolState != "pending" && msg.toolID != "llm-retry" {
				msg.toolResult = agent.OmittedToolResult
				msg.invalidateRender()
			}
		}
		s.markDirty()
		return
	case toolEventTransientHistoryPruned:
		s.reasoningText.Reset()
		s.resetStreamingText()
		kept := s.messages[:s.requestMessageStart]
		for _, msg := range s.messages[s.requestMessageStart:] {
			if msg.role != "agent" && msg.role != "reasoning" {
				kept = append(kept, msg)
			}
		}
		s.messages = kept
		s.markDirty()
		return
	case toolEventReasoningDelta:
		s.finishStreamingText()
		s.reasoningText.WriteString(ev.Detail)
		s.markDirty()
		return
	case toolEventReasoningDone:
		s.finishReasoning()
		return
	case toolEventSteerConsumed:
		s.finishReasoning()
		s.finishStreamingText()
		s.consumePendingSteer(ev.Detail)
		s.addMessage(message{role: "you", text: ev.Detail})
		return
	case toolEventTextDelta:
		s.finishReasoning()
		s.streamingText.WriteString(ev.Detail)
		s.streamingRenderedLines = nil
		s.markDirty()
		return
	}
	s.finishReasoning()
	s.finishStreamingText()
	s.handleToolActivity(ev)
}

func keepToolProgressTail(progress string) string {
	if len(progress) <= maxToolProgressBytes {
		return progress
	}
	return strings.ToValidUTF8(progress[len(progress)-maxToolProgressBytes:], "")
}

func (s *fnUI) handleToolActivity(ev toolEvent) {
	if ev.Kind == toolEventCall {
		s.addMessage(message{
			role: "tool", toolID: ev.ID, toolName: ev.Name,
			toolCommand: ev.Detail, toolState: "pending", toolStartedAt: time.Now(),
		})
		return
	}
	if ev.Kind != toolEventUpdate && ev.Kind != toolEventProgress && ev.Kind != toolEventResult && ev.Kind != toolEventError {
		return
	}
	for i := len(s.messages) - 1; i >= 0; i-- {
		msg := &s.messages[i]
		if msg.role != "tool" || msg.toolState != "pending" {
			continue
		}
		if ev.ID != "" && msg.toolID != ev.ID {
			continue
		}
		if ev.Kind == toolEventUpdate {
			msg.toolResult += ev.Detail
		} else if ev.Kind == toolEventProgress {
			msg.toolProgress = keepToolProgressTail(msg.toolProgress + ev.Detail)
		} else {
			msg.toolResult = ev.Detail
			msg.toolProgress = ""
			msg.toolFinishedAt = time.Now()
			if ev.Kind == toolEventError {
				msg.toolState = "error"
			} else {
				msg.toolState = "success"
			}
		}
		msg.invalidateRender()
		s.markDirty()
		return
	}
	state := "success"
	if ev.Kind == toolEventError {
		state = "error"
	} else if ev.Kind == toolEventUpdate {
		state = "pending"
	}
	msg := message{
		role: "tool", toolID: ev.ID, toolName: ev.Name,
		toolResult: ev.Detail, toolState: state,
	}
	if state == "pending" {
		msg.toolStartedAt = time.Now()
	}
	s.addMessage(msg)
}

func (s *fnUI) resetStreamingText() {
	s.streamingText.Reset()
	s.streamingRenderedLines = nil
}

func (s *fnUI) renderedStreamingText(width int) []string {
	if s.streamingRenderedWidth == width && s.streamingRenderedLines != nil {
		return s.streamingRenderedLines
	}
	lines := renderedMessageLines(message{role: "agent", text: s.streamingText.String()}, width)
	s.streamingRenderedWidth = width
	s.streamingRenderedLines = lines
	return lines
}

func (s *fnUI) finishReasoning() {
	if s.reasoningText.Len() == 0 {
		return
	}
	text := s.reasoningText.String()
	s.reasoningText.Reset()
	s.addMessage(message{role: "reasoning", text: text})
}

func (s *fnUI) finishStreamingText() {
	if s.streamingText.Len() == 0 {
		return
	}
	text := s.streamingText.String()
	s.resetStreamingText()
	s.addMessage(message{role: "agent", text: text})
}

func (s *fnUI) finishResponse(resp response) {
	if resp.id != s.nextRequestID {
		return
	}
	finishedAt := time.Now()
	startedAt := s.requestStartedAt
	s.cancel, s.responding, s.steer = nil, false, nil
	s.requestStartedAt = time.Time{}
	s.retryAttempt, s.retryMaxAttempts, s.retryDeadline = 0, 0, time.Time{}
	hadStreamingText := s.streamingText.Len() > 0
	s.finishReasoning()
	s.finishStreamingText()
	if resp.Err == nil {
		s.contextTokens = resp.ContextTokens
	}
	if errors.Is(resp.Err, context.Canceled) {
		s.addMessage(message{role: "system", text: "Canceled"})
	} else if resp.Err != nil {
		s.addMessage(message{role: "error", text: resp.Err.Error()})
	}
	if !hadStreamingText && resp.Text != "" {
		s.addMessage(message{role: "agent", text: resp.Text})
	}
	if status := completedRequestMessage(startedAt, finishedAt); status != "" {
		s.addMessage(message{role: "status", text: status})
	}
	if len(s.pendingSteer) > 0 {
		pending := append([]string(nil), s.pendingSteer...)
		s.pendingSteer = nil
		s.removePendingInputs("steer", -1)
		s.startRequest(pending[0], true)
		for _, text := range pending[1:] {
			s.pendingSteer = append(s.pendingSteer, text)
			s.pendingInputs = append(s.pendingInputs, pendingInput{"steer", text})
			s.steer <- text
		}
		return
	}
	if len(s.queued) > 0 {
		text := s.queued[0]
		s.queued = s.queued[1:]
		s.removePendingInputs("queued", 1)
		s.startRequest(text, true)
		return
	}
	s.markDirty()
}

func (s *fnUI) restorePendingInputs() bool {
	if len(s.pendingInputs) == 0 {
		return false
	}
	var input []string
	for _, pending := range s.pendingInputs {
		input = append(input, pending.text)
	}
	if draft := s.textarea.Text(); draft != "" {
		input = append(input, draft)
	}
	text := strings.Join(input, "\n\n")
	s.textarea.SetText(text)
	s.textarea.SetCursorPos(runeToClusterIndex(text, len([]rune(text))))
	s.queued = nil
	s.pendingSteer = nil
	s.pendingInputs = nil
	for s.steer != nil {
		select {
		case <-s.steer:
		default:
			s.markDirty()
			return true
		}
	}
	s.markDirty()
	return true
}

func (s *fnUI) cancelRequest() {
	if !s.responding || s.cancel == nil {
		return
	}
	s.cancel()
	s.restorePendingInputs()
	s.nextRequestID++
	s.cancel = nil
	s.steer = nil
	s.responding = false
	s.requestStartedAt = time.Time{}
	s.retryAttempt, s.retryMaxAttempts, s.retryDeadline = 0, 0, time.Time{}
	s.finishReasoning()
	s.finishStreamingText()
	for i := range s.messages {
		if s.messages[i].role == "tool" && s.messages[i].toolState == "pending" {
			s.messages[i].invalidateRender()
			s.messages[i].toolState = "error"
			s.messages[i].toolFinishedAt = time.Now()
			if s.messages[i].toolResult == "" {
				s.messages[i].toolResult = "Canceled"
			}
		}
	}
	s.addMessage(message{role: "system", text: "Canceled"})
	s.markDirty()
}

func (s *fnUI) addMessage(msg message) {
	s.messages = append(s.messages, msg)
	if s.emit != nil {
		s.emit(msg)
	}
	s.markDirty()
}
func (s *fnUI) markDirty() {
	if s.invalidate != nil {
		s.invalidate()
	}
}

func (s *fnUI) consumePendingSteer(text string) {
	for i, pending := range s.pendingSteer {
		if pending == text {
			s.pendingSteer = append(s.pendingSteer[:i], s.pendingSteer[i+1:]...)
			break
		}
	}
	for i, pending := range s.pendingInputs {
		if pending.kind == "steer" && pending.text == text {
			s.pendingInputs = append(s.pendingInputs[:i], s.pendingInputs[i+1:]...)
			break
		}
	}
}

func (s *fnUI) removePendingInputs(kind string, limit int) {
	kept := s.pendingInputs[:0]
	removed := 0
	for _, item := range s.pendingInputs {
		if item.kind == kind && (limit < 0 || removed < limit) {
			removed++
			continue
		}
		kept = append(kept, item)
	}
	s.pendingInputs = kept
}

func (s *fnUI) moveWord(direction int) {
	raw := s.textarea.Text()
	text := []rune(raw)
	pos := clusterToRuneIndex(raw, s.textarea.CursorPos())
	if direction < 0 {
		for pos > 0 && unicode.IsSpace(text[pos-1]) {
			pos--
		}
		for pos > 0 && !unicode.IsSpace(text[pos-1]) {
			pos--
		}
	} else {
		for pos < len(text) && !unicode.IsSpace(text[pos]) {
			pos++
		}
		for pos < len(text) && unicode.IsSpace(text[pos]) {
			pos++
		}
	}
	s.textarea.SetCursorPos(pos)
	s.markDirty()
}
func (s *fnUI) deleteWord(direction int) {
	raw := s.textarea.Text()
	text := []rune(raw)
	start := clusterToRuneIndex(raw, s.textarea.CursorPos())
	end := start
	if direction < 0 {
		for start > 0 && unicode.IsSpace(text[start-1]) {
			start--
		}
		for start > 0 && !unicode.IsSpace(text[start-1]) {
			start--
		}
	} else {
		for end < len(text) && unicode.IsSpace(text[end]) {
			end++
		}
		for end < len(text) && !unicode.IsSpace(text[end]) {
			end++
		}
	}
	s.replaceTextRange(text, start, end)
}
func (s *fnUI) deleteToLineStart() {
	raw := s.textarea.Text()
	text := []rune(raw)
	end := clusterToRuneIndex(raw, s.textarea.CursorPos())
	start := end
	for start > 0 && text[start-1] != '\n' {
		start--
	}
	s.replaceTextRange(text, start, end)
}
func (s *fnUI) deleteToLineEnd() {
	raw := s.textarea.Text()
	text := []rune(raw)
	start := clusterToRuneIndex(raw, s.textarea.CursorPos())
	end := start
	for end < len(text) && text[end] != '\n' {
		end++
	}
	s.replaceTextRange(text, start, end)
}
func (s *fnUI) replaceTextRange(text []rune, start, end int) {
	next := string(append(append([]rune{}, text[:start]...), text[end:]...))
	s.textarea.SetText(next)
	s.textarea.SetCursorPos(runeToClusterIndex(next, start))
	s.markDirty()
}

func (s *fnUI) navigateHistory(direction int) bool {
	if len(s.inputHistory) == 0 {
		return false
	}
	if s.historyIndex < 0 {
		if direction > 0 {
			return false
		}
		text := s.textarea.Text()
		cursor := clusterToRuneIndex(text, s.textarea.CursorPos())
		if strings.Contains(string([]rune(text)[:cursor]), "\n") {
			return false
		}
		s.historyDraft = s.textarea.Text()
		s.historyIndex = len(s.inputHistory)
	}

	s.historyIndex = min(max(s.historyIndex+direction, 0), len(s.inputHistory))
	text := s.historyDraft
	if s.historyIndex < len(s.inputHistory) {
		text = s.inputHistory[s.historyIndex]
	} else {
		s.historyIndex = -1
	}
	s.textarea.SetText(text)
	s.textarea.SetCursorPos(runeToClusterIndex(text, len([]rune(text))))
	s.markDirty()
	return true
}

func (s *fnUI) handleUndoKey(k tui.KeyEvent) bool {
	assert.That(len(s.undoOptions) > 0, "handle undo key without undo options")
	assert.That(s.undoSelected >= 0 && s.undoSelected < len(s.undoOptions), "undo selection out of bounds")
	assert.That(s.commands.Undo != nil, "undo selector without undo command")
	switch k.Key {
	case tui.KeyEscape:
		s.undoOptions = nil
	case tui.KeyUp:
		s.undoSelected = max(0, s.undoSelected-1)
	case tui.KeyDown:
		s.undoSelected = min(len(s.undoOptions)-1, s.undoSelected+1)
	case tui.KeyEnter:
		selected := s.undoSelected
		option := s.undoOptions[selected]
		text, err := s.commands.Undo(len(s.undoOptions) - 1 - selected)
		s.undoOptions = nil
		if err != nil {
			s.addMessage(message{role: "error", text: err.Error()})
			return true
		}
		assert.That(text == option.text, "undo command returned a different input")
		assert.That(option.messageIndex >= 0 && option.messageIndex < len(s.messages), "undo message index out of bounds")
		assert.That(s.messages[option.messageIndex].role == "you", "undo message is not a user message")
		assert.That(s.messages[option.messageIndex].text == option.text, "undo message text changed")
		s.messages = s.messages[:option.messageIndex]
		s.inputHistory = s.inputHistory[:0]
		for _, msg := range s.messages {
			if msg.role == "you" {
				s.inputHistory = append(s.inputHistory, msg.text)
			}
		}
		assert.That(len(s.inputHistory) == selected, "undo options do not match user messages")
		s.contextTokens = 0
		s.textarea.SetText(text)
		s.textarea.SetCursorPos(runeToClusterIndex(text, len([]rune(text))))
	default:
		return true
	}
	s.markDirty()
	return true
}

func (s *fnUI) handleKey(k tui.KeyEvent) bool {
	if len(s.undoOptions) > 0 {
		return s.handleUndoKey(k)
	}
	if k.Key == tui.KeyRune && k.Rune == 'c' && k.Mod.Has(tui.ModCtrl) {
		now := time.Now()
		if !s.lastCtrlC.IsZero() && now.Sub(s.lastCtrlC) <= ctrlCDoublePressInterval {
			return false
		}
		s.lastCtrlC = now
		s.cancelRequest()
		return true
	}
	s.lastCtrlC = time.Time{}
	if k.Key == tui.KeyEscape {
		if s.retryAttempt > 0 {
			s.cancelRequest()
			return true
		}
		s.historyIndex, s.historyDraft = -1, ""
		s.textarea.Clear()
		s.markDirty()
		return true
	}
	if k.Key == tui.KeyEnter && k.Mod.Has(tui.ModShift) {
		return s.submitInput(s.textarea.Text(), true)
	}
	if k.Key == tui.KeyEnter && k.Mod == tui.ModNone {
		return s.submitInput(s.textarea.Text(), false)
	}
	if k.Key == tui.KeyUp && k.Mod.Has(tui.ModCtrl) && s.restorePendingInputs() {
		return true
	}
	if k.Mod == tui.ModNone && k.Key == tui.KeyUp && s.navigateHistory(-1) {
		return true
	}
	if k.Mod == tui.ModNone && k.Key == tui.KeyDown && s.navigateHistory(1) {
		return true
	}
	if (k.Mod.Has(tui.ModAlt) || k.Mod.Has(tui.ModCtrl)) && k.Key == tui.KeyLeft || k.Mod.Has(tui.ModAlt) && k.Key == tui.KeyRune && k.Rune == 'b' {
		s.moveWord(-1)
		return true
	}
	if (k.Mod.Has(tui.ModAlt) || k.Mod.Has(tui.ModCtrl)) && k.Key == tui.KeyRight || k.Mod.Has(tui.ModAlt) && k.Key == tui.KeyRune && k.Rune == 'f' {
		s.moveWord(1)
		return true
	}
	if k.Key == tui.KeyRune && k.Rune == 'w' && k.Mod.Has(tui.ModCtrl) {
		s.deleteWord(-1)
		return true
	}
	if k.Key == tui.KeyRune && k.Rune == 'd' && k.Mod.Has(tui.ModAlt) {
		s.deleteWord(1)
		return true
	}
	if k.Key == tui.KeyRune && k.Rune == 'u' && k.Mod.Has(tui.ModCtrl) {
		s.deleteToLineStart()
		return true
	}
	if k.Key == tui.KeyRune && k.Rune == 'k' && k.Mod.Has(tui.ModCtrl) {
		s.deleteToLineEnd()
		return true
	}
	s.historyIndex, s.historyDraft = -1, ""
	s.textarea.HandleEvent(k)
	s.markDirty()
	return true
}

func (s *fnUI) restoreConversation(conversation []agent.ConversationMessage) {
	for _, restored := range conversation {
		switch restored.Type {
		case "tool_call":
			s.messages = append(s.messages, message{
				role: "tool", toolID: restored.ToolID, toolName: restored.ToolName,
				toolCommand: restored.ToolInput, toolState: "pending",
			})
		case "tool_result":
			state := "success"
			if restored.ToolError {
				state = "error"
			}
			for i := len(s.messages) - 1; i >= 0; i-- {
				if s.messages[i].role == "tool" && s.messages[i].toolState == "pending" && s.messages[i].toolID == restored.ToolID {
					s.messages[i].toolResult = restored.Text
					s.messages[i].toolState = state
					break
				}
			}
		case "reasoning":
			s.messages = append(s.messages, message{role: "reasoning", text: restored.Text})
		default:
			role := "agent"
			if restored.Role == "user" {
				role = "you"
				s.inputHistory = append(s.inputHistory, restored.Text)
			}
			s.messages = append(s.messages, message{role: role, text: restored.Text})
		}
	}
}

func Run(modelName, reasoningEffort, sessionID, cwd string, conversation []agent.ConversationMessage, commands Commands, respond func(string, <-chan string, func(agent.ToolEvent), context.Context) agent.Response) error {
	term, err := tui.NewANSITerminal(os.Stdout, os.Stdin)
	if err != nil {
		return err
	}
	if err = term.EnterRawMode(); err != nil {
		return err
	}
	defer term.ExitRawMode()
	// Request xterm extended-key mode as well as Kitty keyboard mode. tmux
	// recognizes the former and, when configured for CSI-u output, forwards
	// Shift+Enter in the same form understood by go-tui. Unsupported terminals
	// ignore both sequences.
	if _, err := io.WriteString(os.Stdout, "\x1b[>4;1m"); err != nil {
		return err
	}
	defer io.WriteString(os.Stdout, "\x1b[>4m")

	// Enable Kitty keyboard mode directly instead of querying terminal
	// capabilities. Negotiation reads stdin during startup and can consume text
	// the user has already started typing; unsupported terminals ignore this.
	term.EnableKittyKeyboard()
	defer term.DisableKittyKeyboard()
	term.HideCursor()
	defer term.ShowCursor()
	reader, err := tui.NewEventReader(os.Stdin)
	if err != nil {
		return err
	}
	defer reader.Close()
	updates := make(chan func(), 256)
	root := newUI(modelName, reasoningEffort, func(input string, steer <-chan string, emit func(toolEvent), ctx context.Context) response {
		result := respond(input, steer, emit, ctx)
		return response{Text: result.Text, ContextTokens: result.ContextTokens, Err: result.Err}
	})
	root.sessionID = sessionID
	root.commands = commands
	root.restoreConversation(conversation)
	root.cwd = cwd
	root.dispatch = func(fn func()) { updates <- fn }
	// Run serializes every state mutation below and marks the corresponding
	// branch dirty, so state-level invalidation does not need a second wakeup.
	root.invalidate = func() {}
	w, h := term.Size()
	renderer := newMainScreenRenderer(os.Stdout, w, h)
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	defer func() {
		if root.cancel != nil {
			root.cancel()
		}
		_ = renderer.stop()
	}()
	dirty, urgentRender := true, true
	var lastRender time.Time
	for {
		now := time.Now()
		if dirty && (urgentRender || lastRender.IsZero() || now.Sub(lastRender) >= backgroundRenderInterval) {
			lines, cr, cc := root.render(w, h)
			if err := renderer.renderWithLiveStart(lines, cr, cc, root.liveStart); err != nil {
				return err
			}
			dirty, urgentRender = false, false
			lastRender = time.Now()
		}

		// Check stdin before draining background work so a fast stream of model
		// or shell updates cannot prevent the editor from receiving input.
		if ev, ok := reader.PollEvent(0); ok {
			if k, ok := ev.(tui.KeyEvent); ok {
				if !root.handleKey(k) {
					return nil
				}
				dirty, urgentRender = true, true
			}
			continue
		}

		handledWork := false
	forWork:
		for range maxUpdatesPerIteration {
			select {
			case fn := <-updates:
				fn()
				dirty = true
				handledWork = true
			case <-resize:
				w, h = term.Size()
				renderer.resize(w, h)
				dirty, urgentRender = true, true
				handledWork = true
			default:
				break forWork
			}
		}
		if handledWork {
			continue
		}

		// Nothing is ready. Poll briefly for terminal input, but wake often
		// enough to render a dirty frame at the capped refresh rate.
		timeout := eventPollInterval
		if dirty {
			untilRender := backgroundRenderInterval - time.Since(lastRender)
			if untilRender < timeout {
				timeout = max(untilRender, 0)
			}
		}
		if ev, ok := reader.PollEvent(timeout); ok {
			if k, ok := ev.(tui.KeyEvent); ok {
				if !root.handleKey(k) {
					return nil
				}
				dirty, urgentRender = true, true
			}
		}
	}
}
