package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/Baitinq/fn-agent/src/internal/assert"
)

type ConversationMessage struct {
	Type      string
	Role      string
	Text      string
	ToolID    string
	ToolName  string
	ToolInput string
	ToolError bool
}

func (a *Agent) assertSessionInitialized() {
	assert.That(a.sessionID != "", "agent has no session ID")
	assert.That(a.sessionDir != "", "agent has no session directory")
	assert.That(filepath.IsAbs(a.sessionDir), "session directory is not absolute")
	assert.That(filepath.Base(a.sessionDir) == a.sessionID, "session directory does not match session ID")
}

func (a *Agent) assertSessionUninitialized() {
	assert.That(a.sessionID == "", "agent already has a session ID")
	assert.That(a.sessionDir == "", "agent already has a session directory")
}

func assertSessionArguments(id, sessionsDir string) {
	assert.That(id != "", "empty session ID")
	assert.That(filepath.Base(id) == id, "session ID contains a path separator")
	assert.That(sessionsDir != "", "empty sessions directory")
	assert.That(filepath.IsAbs(sessionsDir), "sessions directory is not absolute")
}

func (a *Agent) Conversation() []ConversationMessage {
	var conversation []ConversationMessage
	for _, item := range a.history {
		restored := ConversationMessage{Type: item.Type, Role: item.Role, Text: item.Text, ToolID: item.CallID, ToolName: item.Name, ToolError: item.ToolError}
		switch item.Type {
		case "message":
			if item.Role == "user" {
				restored.Text = userMessageText(item.Text)
			}
			if restored.Text == "" {
				continue
			}
		case "reasoning":
			if restored.Text == "" {
				continue
			}
		case "tool_call":
			var arguments struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(item.Arguments, &arguments)
			restored.ToolInput = arguments.Code
		case "tool_result":
		default:
			continue
		}
		conversation = append(conversation, restored)
	}
	return conversation
}

func userMessageText(text string) string {
	if end := strings.Index(text, "]\n\n"); strings.HasPrefix(text, "[") && end >= 0 {
		if _, err := time.Parse(time.RFC3339, text[1:end]); err == nil {
			return text[end+3:]
		}
	}
	return text
}

// Undo removes the selected user message and everything after it. steps is zero for the latest message.
func (a *Agent) Undo(steps int) (string, error) {
	a.assertSessionInitialized()
	assert.That(steps >= 0, "negative undo steps")
	a.respondMu.Lock()
	defer a.respondMu.Unlock()
	for i := len(a.history) - 1; i >= 0; i-- {
		if !isUserHistoryItem(a.history[i]) {
			continue
		}
		if steps > 0 {
			steps--
			continue
		}
		text := userMessageText(a.history[i].Text)
		checkpoint := a.history[i].REPLCheckpoint
		rollbackCheckpoint := ""
		if checkpoint != "" {
			var err error
			rollbackCheckpoint, err = a.snapshotREPL()
			if err != nil {
				return "", err
			}
			defer os.Remove(filepath.Join(a.sessionDir, rollbackCheckpoint))
			if err := a.pythonREPL().restore(filepath.Join(a.sessionDir, checkpoint), filepath.Join(a.sessionDir, "repl-objects")); err != nil {
				return "", fmt.Errorf("restore Python checkpoint: %w", err)
			}
		}
		history, compaction := a.history, a.compaction
		a.history = a.history[:i]
		if a.compaction != nil && i < a.compaction.FirstKeptItem {
			a.compaction = nil
		}
		if err := a.SaveSession(); err != nil {
			a.history, a.compaction = history, compaction
			if rollbackCheckpoint != "" {
				if rollbackErr := a.pythonREPL().restore(filepath.Join(a.sessionDir, rollbackCheckpoint), filepath.Join(a.sessionDir, "repl-objects")); rollbackErr != nil {
					return "", fmt.Errorf("%w; restore Python state after failed undo: %v", err, rollbackErr)
				}
				if rollbackErr := a.pythonREPL().snapshot(filepath.Join(a.sessionDir, "repl.json"), filepath.Join(a.sessionDir, "repl-objects")); rollbackErr != nil {
					return "", fmt.Errorf("%w; save Python state after failed undo: %v", err, rollbackErr)
				}
			}
			return "", err
		}
		return text, nil
	}
	return "", fmt.Errorf("nothing to undo")
}

func copyFile(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func copyDirectory(source, destination string) error {
	entries, err := os.ReadDir(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := copyFile(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Fork continues the current agent state in a new session.
func (a *Agent) Fork(id, sessionsDir string) error {
	a.assertSessionInitialized()
	assertSessionArguments(id, sessionsDir)
	assert.That(id != a.sessionID, "fork session ID matches current session")
	a.respondMu.Lock()
	defer a.respondMu.Unlock()
	oldID, oldDir := a.sessionID, a.sessionDir
	oldHistory, oldUsage, oldCompaction := a.savedHistory, a.savedUsage, a.savedCompaction
	restoreSession := func() {
		a.sessionID, a.sessionDir = oldID, oldDir
		a.savedHistory, a.savedUsage, a.savedCompaction = oldHistory, oldUsage, oldCompaction
	}
	a.sessionID = id
	a.sessionDir = filepath.Join(sessionsDir, id)
	if err := copyDirectory(filepath.Join(oldDir, "repl-objects"), filepath.Join(a.sessionDir, "repl-objects")); err != nil {
		restoreSession()
		return err
	}
	for _, item := range a.history {
		if item.REPLCheckpoint == "" {
			continue
		}
		if err := copyFile(filepath.Join(oldDir, item.REPLCheckpoint), filepath.Join(a.sessionDir, item.REPLCheckpoint)); err != nil {
			restoreSession()
			return err
		}
	}
	a.savedHistory, a.savedUsage, a.savedCompaction = nil, nil, nil
	if err := a.createSessionLog(); err != nil {
		restoreSession()
		return err
	}
	if err := a.SaveSession(); err != nil {
		restoreSession()
		return err
	}
	return nil
}

const sessionVersion = 1
const sessionFilename = "session.jsonl"

type sessionHeader struct {
	Type     string `json:"type"`
	Version  int    `json:"version"`
	CWD      string `json:"cwd"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

type sessionUpdate struct {
	Type        string           `json:"type"`
	HistoryFrom int              `json:"history_from"`
	History     []historyItem    `json:"history"`
	UsageFrom   int              `json:"usage_from"`
	Usage       []Usage          `json:"usage"`
	Compaction  *compactionState `json:"compaction"`
}

type sessionFile struct {
	Version    int
	CWD        string
	Provider   string
	Model      string
	Compaction *compactionState
	History    []historyItem
	Usage      []Usage
}

func (a *Agent) createSessionLog() error {
	if err := os.MkdirAll(a.sessionDir, 0700); err != nil {
		return err
	}
	header := sessionHeader{Type: "session", Version: sessionVersion, CWD: a.cwd, Provider: a.provider, Model: a.modelName}
	data, err := json.Marshal(header)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(a.sessionDir, sessionFilename), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (a *Agent) StartSession(id, sessionsDir string) error {
	a.assertSessionUninitialized()
	assertSessionArguments(id, sessionsDir)
	a.sessionID = id
	a.sessionDir = filepath.Join(sessionsDir, id)
	if err := a.createSessionLog(); err != nil {
		return err
	}
	return a.SaveSession()
}

func (a *Agent) completeInterruptedToolCalls() bool {
	pending := make(map[string]historyItem)
	var order []string
	for _, item := range a.history {
		switch item.Type {
		case "tool_call":
			if _, exists := pending[item.CallID]; !exists {
				order = append(order, item.CallID)
			}
			pending[item.CallID] = item
		case "tool_result":
			delete(pending, item.CallID)
		}
	}
	changed := false
	for _, callID := range order {
		call, exists := pending[callID]
		if !exists {
			continue
		}
		a.history = append(a.history, historyItem{
			Type: "tool_result", CallID: callID, Name: call.Name,
			Text: "tool error: execution was interrupted before completion", ToolError: true,
		})
		changed = true
	}
	return changed
}

func readSession(path string) (sessionFile, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return sessionFile{}, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var saved sessionFile
	lineNumber := 0
	var validBytes int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		if readErr == io.EOF {
			if err := file.Truncate(validBytes); err != nil {
				return sessionFile{}, err
			}
			break
		}
		validBytes += int64(len(line))
		lineNumber++
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			return sessionFile{}, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		switch envelope.Type {
		case "session":
			if lineNumber != 1 {
				return sessionFile{}, fmt.Errorf("line %d: unexpected session header", lineNumber)
			}
			var header sessionHeader
			if err := json.Unmarshal(line, &header); err != nil {
				return sessionFile{}, fmt.Errorf("line %d: %w", lineNumber, err)
			}
			saved.Version, saved.CWD, saved.Provider, saved.Model = header.Version, header.CWD, header.Provider, header.Model
		case "update":
			var update sessionUpdate
			if err := json.Unmarshal(line, &update); err != nil {
				return sessionFile{}, fmt.Errorf("line %d: %w", lineNumber, err)
			}
			if update.HistoryFrom < 0 || update.HistoryFrom > len(saved.History) || update.UsageFrom < 0 || update.UsageFrom > len(saved.Usage) {
				return sessionFile{}, fmt.Errorf("line %d: invalid update", lineNumber)
			}
			saved.History = append(saved.History[:update.HistoryFrom], update.History...)
			saved.Usage = append(saved.Usage[:update.UsageFrom], update.Usage...)
			saved.Compaction = update.Compaction
		default:
			return sessionFile{}, fmt.Errorf("line %d: unknown entry type %q", lineNumber, envelope.Type)
		}
		if readErr != nil {
			return sessionFile{}, readErr
		}
	}
	if lineNumber == 0 {
		return sessionFile{}, fmt.Errorf("empty session log")
	}
	return saved, nil
}

func sameDirectory(first, second string) bool {
	if first == second {
		return true
	}
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	return firstErr == nil && secondErr == nil && os.SameFile(firstInfo, secondInfo)
}

func (a *Agent) ResumeSession(id, sessionsDir string) error {
	a.assertSessionUninitialized()
	assertSessionArguments(id, sessionsDir)
	dir := filepath.Join(sessionsDir, id)
	saved, err := readSession(filepath.Join(dir, sessionFilename))
	if err != nil {
		return fmt.Errorf("load session %s: %w", id, err)
	}
	if saved.Version != sessionVersion {
		return fmt.Errorf("load session %s: unsupported version %d (expected %d)", id, saved.Version, sessionVersion)
	}
	if !sameDirectory(saved.CWD, a.cwd) {
		return fmt.Errorf("session %s belongs to %s", id, saved.CWD)
	}
	if saved.Provider != "" && (saved.Provider != a.provider || saved.Model != a.modelName) && (os.Getenv("FN_PROVIDER") == "" || os.Getenv("FN_MODEL") == "") {
		return fmt.Errorf("session %s uses %s/%s, set both FN_PROVIDER and FN_MODEL to resume with %s/%s", id, saved.Provider, saved.Model, a.provider, a.modelName)
	}
	a.sessionID, a.sessionDir = id, dir
	a.compaction = saved.Compaction
	a.history = saved.History
	a.usage = saved.Usage
	a.markSessionSaved()
	for _, usage := range saved.Usage {
		a.tokensUsed += usage.TotalTokens
	}
	statePath := filepath.Join(dir, "repl.json")
	if _, err := os.Stat(statePath); err == nil {
		if err := a.pythonREPL().restore(statePath, filepath.Join(dir, "repl-objects")); err != nil {
			return fmt.Errorf("restore Python state: %w", err)
		}
	}
	if a.completeInterruptedToolCalls() {
		if err := a.SaveSession(); err != nil {
			return fmt.Errorf("repair interrupted session: %w", err)
		}
	}
	return nil
}

func commonHistoryPrefix(left, right []historyItem) int {
	length := min(len(left), len(right))
	for i := 0; i < length; i++ {
		if !reflect.DeepEqual(left[i], right[i]) {
			return i
		}
	}
	return length
}

func commonUsagePrefix(left, right []Usage) int {
	length := min(len(left), len(right))
	for i := 0; i < length; i++ {
		if left[i] != right[i] {
			return i
		}
	}
	return length
}

func (a *Agent) markSessionSaved() {
	a.savedHistory = append(a.savedHistory[:0], a.history...)
	a.savedUsage = append(a.savedUsage[:0], a.Usage()...)
	if a.compaction == nil {
		a.savedCompaction = nil
	} else {
		copy := *a.compaction
		a.savedCompaction = &copy
	}
}

func (a *Agent) SaveSession() error {
	a.assertSessionInitialized()
	var replSnapshots []string
	var replObjectsPath string
	if a.repl != nil {
		snapshotPath := filepath.Join(a.sessionDir, "repl.json")
		replObjectsPath = filepath.Join(a.sessionDir, "repl-objects")
		if err := a.repl.snapshot(snapshotPath, replObjectsPath); err != nil {
			return fmt.Errorf("save Python state: %w", err)
		}
		replSnapshots = append(replSnapshots, snapshotPath)
		for _, item := range a.history {
			if item.REPLCheckpoint != "" {
				replSnapshots = append(replSnapshots, filepath.Join(a.sessionDir, item.REPLCheckpoint))
			}
		}
	}
	usage := a.Usage()
	historyFrom := commonHistoryPrefix(a.savedHistory, a.history)
	usageFrom := commonUsagePrefix(a.savedUsage, usage)
	if historyFrom == len(a.history) && historyFrom == len(a.savedHistory) &&
		usageFrom == len(usage) && usageFrom == len(a.savedUsage) && reflect.DeepEqual(a.savedCompaction, a.compaction) {
		return nil
	}
	update := sessionUpdate{
		Type: "update", HistoryFrom: historyFrom, History: a.history[historyFrom:],
		UsageFrom: usageFrom, Usage: usage[usageFrom:], Compaction: a.compaction,
	}
	data, err := json.Marshal(update)
	if err != nil {
		return err
	}
	path := filepath.Join(a.sessionDir, sessionFilename)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Truncate(info.Size())
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Truncate(info.Size())
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	a.markSessionSaved()
	if a.repl != nil {
		// Cleanup is best-effort; it must not prevent session persistence.
		_ = a.repl.gcObjects(replSnapshots, replObjectsPath)
	}
	return nil
}
