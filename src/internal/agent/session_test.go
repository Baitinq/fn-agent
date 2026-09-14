package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionRoundTrip(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	a := &Agent{
		cwd: cwd, provider: "openai", modelName: "test",
		history:    []historyItem{{Type: "message", Role: "user", Text: "hello"}, {Type: "message", Role: "assistant", Text: "hi"}},
		compaction: &compactionState{Summary: "earlier context", FirstKeptItem: 1},
		usage:      []Usage{{InputTokens: 40, CachedInputTokens: 10, OutputTokens: 2, ReasoningOutputTokens: 1, TotalTokens: 42}},
	}
	if err := a.StartSession(id, root); err != nil {
		t.Fatal(err)
	}
	loaded := &Agent{cwd: cwd, provider: "openai", modelName: "test"}
	if err := loaded.ResumeSession(id, root); err != nil {
		t.Fatal(err)
	}
	if loaded.compaction.Summary != a.compaction.Summary || len(loaded.history) != 2 || loaded.history[0].Type != "message" || loaded.TokensUsed() != 42 {
		t.Fatalf("loaded session = summary %q, history %#v", loaded.compaction.Summary, loaded.history)
	}
	if loaded.history[1].Text != "hi" {
		t.Fatalf("loaded output message = %#v", loaded.history[1])
	}
	sessionPath := filepath.Join(root, id, sessionFilename)
	if mode := fileMode(t, sessionPath); mode != 0o600 {
		t.Fatalf("session mode = %o", mode)
	}
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("session entries = %d, want 2", len(lines))
	}
	var header sessionHeader
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatal(err)
	}
	if header.Type != "session" || header.Version != sessionVersion || header.CWD != cwd {
		t.Fatalf("session header = %#v", header)
	}
}

func TestSessionLogRecoversFromTornFinalEntry(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	a := &Agent{cwd: cwd, provider: "openai", modelName: "test"}
	if err := a.StartSession(id, root); err != nil {
		t.Fatal(err)
	}
	a.history = append(a.history, historyMessage("user", "preserved"))
	if err := a.SaveSession(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, id, sessionFilename)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"update","history_from":1`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	loaded := &Agent{cwd: cwd, provider: "openai", modelName: "test"}
	if err := loaded.ResumeSession(id, root); err != nil {
		t.Fatal(err)
	}
	if len(loaded.history) != 1 || loaded.history[0].Text != "preserved" {
		t.Fatalf("recovered history = %#v", loaded.history)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), string(before)) || len(after) != len(before) {
		t.Fatal("recovery did not remove only the torn entry")
	}
	loaded.history = append(loaded.history, historyMessage("assistant", "continued"))
	if err := loaded.SaveSession(); err != nil {
		t.Fatal(err)
	}
	reloaded := &Agent{cwd: cwd, provider: "openai", modelName: "test"}
	if err := reloaded.ResumeSession(id, root); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.history) != 2 || reloaded.history[1].Text != "continued" {
		t.Fatalf("continued history = %#v", reloaded.history)
	}
}

func TestUndoRemovesLatestTurnAndSaves(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	a := &Agent{cwd: cwd, provider: "openai", modelName: "test", history: []historyItem{
		{Type: "message", Role: "user", Text: "[2026-08-30T12:00:00+02:00]\n\nfirst"},
		{Type: "message", Role: "assistant", Text: "one"},
		{Type: "message", Role: "user", Text: "[2026-08-30T12:01:00+02:00]\n\nsecond"},
		{Type: "tool_call", CallID: "call-1", Name: "repl"},
		{Type: "tool_result", CallID: "call-1", Text: "result"},
		{Type: "message", Role: "assistant", Text: "two"},
	}}
	if err := a.StartSession(id, root); err != nil {
		t.Fatal(err)
	}
	text, err := a.Undo(0)
	if err != nil || text != "second" || len(a.history) != 2 {
		t.Fatalf("Undo() = %q, %v; history = %#v", text, err, a.history)
	}
	loaded := &Agent{cwd: cwd, provider: "openai", modelName: "test"}
	if err := loaded.ResumeSession(id, root); err != nil {
		t.Fatal(err)
	}
	if len(loaded.history) != 2 || loaded.history[1].Text != "one" {
		t.Fatalf("saved history = %#v", loaded.history)
	}
}

func TestUndoRestoresPythonCheckpoint(t *testing.T) {
	a := &Agent{}
	startTestSession(t, a)
	defer a.Close()
	if err := a.appendUserMessage("first"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.pythonREPL().execute(t.Context(), "value = 1"); err != nil {
		t.Fatal(err)
	}
	if err := a.appendUserMessage("second"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.pythonREPL().execute(t.Context(), "value = 2; added = 3"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Undo(0); err != nil {
		t.Fatal(err)
	}
	output, failed, err := a.pythonREPL().execute(t.Context(), "value, 'added' in globals()")
	if err != nil || failed || output != "(1, False)" {
		t.Fatalf("restored state = %q, failed %v, err %v", output, failed, err)
	}
}

func TestUserMessageCheckpointsAreUnique(t *testing.T) {
	a := &Agent{}
	startTestSession(t, a)
	defer a.Close()
	if err := a.appendUserMessage("first"); err != nil {
		t.Fatal(err)
	}
	first := a.history[0].REPLCheckpoint
	a.history = nil
	if err := a.appendUserMessage("second"); err != nil {
		t.Fatal(err)
	}
	if second := a.history[0].REPLCheckpoint; second == first {
		t.Fatalf("checkpoint reused: %q", second)
	}
}

func TestPythonCheckpointsReuseUnchangedValues(t *testing.T) {
	a := &Agent{}
	startTestSession(t, a)
	defer a.Close()
	if _, _, err := a.pythonREPL().execute(t.Context(), "value = 'unchanged'"); err != nil {
		t.Fatal(err)
	}
	if err := a.appendUserMessage("first"); err != nil {
		t.Fatal(err)
	}
	if err := a.appendUserMessage("second"); err != nil {
		t.Fatal(err)
	}
	objects, err := os.ReadDir(filepath.Join(a.sessionDir, "repl-objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 {
		t.Fatalf("stored objects = %d, want 1", len(objects))
	}
	if _, _, err := a.pythonREPL().execute(t.Context(), "value = 'changed'"); err != nil {
		t.Fatal(err)
	}
	if err := a.appendUserMessage("third"); err != nil {
		t.Fatal(err)
	}
	objects, err = os.ReadDir(filepath.Join(a.sessionDir, "repl-objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 {
		t.Fatalf("stored objects after change = %d, want 2", len(objects))
	}
}

func TestAppendUserMessageCompletesInterruptedToolCall(t *testing.T) {
	a := &Agent{}
	startTestSession(t, a)
	defer a.Close()
	a.history = []historyItem{{Type: "tool_call", CallID: "call-1", Name: "repl", Text: `{"code":"1"}`}}

	if err := a.appendUserMessage("continue"); err != nil {
		t.Fatal(err)
	}

	if len(a.history) != 3 || a.history[1].Type != "tool_result" || a.history[1].CallID != "call-1" || a.history[2].Type != "message" || a.history[2].Role != "user" {
		t.Fatalf("history = %#v", a.history)
	}
}

func TestFailedUndoRestoresPythonState(t *testing.T) {
	a := &Agent{}
	startTestSession(t, a)
	defer a.Close()
	if err := a.appendUserMessage("first"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.pythonREPL().execute(t.Context(), "value = 1"); err != nil {
		t.Fatal(err)
	}
	if err := a.appendUserMessage("second"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.pythonREPL().execute(t.Context(), "value = 2"); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveSession(); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(a.sessionDir, sessionFilename)
	if err := os.Chmod(sessionPath, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Undo(0); err == nil {
		t.Fatal("expected undo to fail")
	}
	if err := os.Chmod(sessionPath, 0o600); err != nil {
		t.Fatal(err)
	}
	output, failed, err := a.pythonREPL().execute(t.Context(), "value")
	if err != nil || failed || output != "2" || len(a.history) != 2 {
		t.Fatalf("state after failed undo: output=%q failed=%v err=%v history=%#v", output, failed, err, a.history)
	}
	loaded := &Agent{cwd: a.cwd, provider: a.provider, modelName: a.modelName}
	defer loaded.Close()
	if err := loaded.ResumeSession(a.sessionID, filepath.Dir(a.sessionDir)); err != nil {
		t.Fatal(err)
	}
	output, failed, err = loaded.pythonREPL().execute(t.Context(), "value")
	if err != nil || failed || output != "2" || len(loaded.history) != 2 {
		t.Fatalf("saved state after failed undo: output=%q failed=%v err=%v history=%#v", output, failed, err, loaded.history)
	}
}

func TestUndoRejectsEmptyHistory(t *testing.T) {
	a := &Agent{}
	startTestSession(t, a)
	if _, err := a.Undo(0); err == nil {
		t.Fatal("expected nothing to undo")
	}
}

func TestForkCopiesSessionState(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	oldID := "550e8400-e29b-41d4-a716-446655440000"
	newID := "550e8400-e29b-41d4-a716-446655440001"
	a := &Agent{cwd: cwd, provider: "openai", modelName: "test", compaction: &compactionState{Summary: "checkpoint", FirstKeptItem: 1},
		usage: []Usage{{TotalTokens: 42}}, tokensUsed: 42}
	if _, _, err := a.pythonREPL().execute(t.Context(), "fork_value = 7"); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.StartSession(oldID, root); err != nil {
		t.Fatal(err)
	}
	if err := a.appendUserMessage("hello"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.pythonREPL().execute(t.Context(), "fork_value = 8"); err != nil {
		t.Fatal(err)
	}
	if err := a.Fork(newID, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, oldID, sessionFilename)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, newID, a.history[0].REPLCheckpoint)); err != nil {
		t.Fatal(err)
	}
	loaded := &Agent{cwd: cwd, provider: "openai", modelName: "test"}
	defer loaded.Close()
	if err := loaded.ResumeSession(newID, root); err != nil {
		t.Fatal(err)
	}
	output, failed, err := loaded.pythonREPL().execute(t.Context(), "fork_value")
	if err != nil || failed || output != "8" || loaded.compaction == nil || loaded.compaction.Summary != "checkpoint" || len(loaded.history) != 1 || loaded.TokensUsed() != 42 {
		t.Fatalf("forked state: output=%q failed=%v err=%v summary=%q history=%#v tokens=%d", output, failed, err, loaded.compaction.Summary, loaded.history, loaded.TokensUsed())
	}
	if _, err := loaded.Undo(0); err != nil {
		t.Fatal(err)
	}
	output, failed, err = loaded.pythonREPL().execute(t.Context(), "fork_value")
	if err != nil || failed || output != "7" {
		t.Fatalf("forked checkpoint state: output=%q failed=%v err=%v", output, failed, err)
	}
}

func TestSessionRejectsUnsupportedVersion(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(sessionHeader{Type: "session", Version: sessionVersion + 1, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionFilename), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Agent{cwd: cwd}).ResumeSession(id, root); err == nil {
		t.Fatal("expected unsupported version")
	}
}

func TestSessionRestoresPythonState(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	a := &Agent{cwd: cwd}
	if _, _, err := a.pythonREPL().execute(t.Context(), "items = [1, 2, 3]"); err != nil {
		t.Fatal(err)
	}
	if err := a.StartSession(id, root); err != nil {
		t.Fatal(err)
	}
	a.Close()

	loaded := &Agent{cwd: cwd}
	defer loaded.Close()
	if err := loaded.ResumeSession(id, root); err != nil {
		t.Fatal(err)
	}
	output, failed, err := loaded.pythonREPL().execute(t.Context(), "items")
	if err != nil || failed || output != "[1, 2, 3]" {
		t.Fatalf("restored value = %q, failed %v, err %v", output, failed, err)
	}
}

func TestResumeSessionAcceptsSameDirectoryThroughSymlink(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	alias := filepath.Join(t.TempDir(), "cwd")
	if err := os.Symlink(cwd, alias); err != nil {
		t.Fatal(err)
	}

	a := &Agent{cwd: alias, provider: "openai", modelName: "test"}
	if err := a.StartSession("abc", root); err != nil {
		t.Fatal(err)
	}

	loaded := &Agent{cwd: cwd, provider: "openai", modelName: "test"}
	if err := loaded.ResumeSession("abc", root); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRejectsDifferentWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	a := &Agent{cwd: t.TempDir()}
	if err := a.StartSession(id, root); err != nil {
		t.Fatal(err)
	}
	if err := (&Agent{cwd: t.TempDir()}).ResumeSession(id, root); err == nil {
		t.Fatal("expected working directory mismatch")
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestResumeRestoresPrunedModelHistory(t *testing.T) {
	root := t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	a := &Agent{cwd: t.TempDir(), provider: "openai", modelName: "test", history: []historyItem{
		{Type: "message", Role: "user", Text: "first"},
		{Type: "reasoning", Text: "thinking"},
		{Type: "message", Role: "assistant", Text: "checking", transient: true},
		{Type: "tool_call", CallID: "call-1", Name: "repl", Arguments: json.RawMessage(`{"code":"print(42)"}`)},
		{Type: "tool_result", CallID: "call-1", Name: "repl", Text: "42\n"},
		{Type: "message", Role: "assistant", Text: "done"},
	}}
	a.pruneTransientHistory()
	if err := a.StartSession(id, root); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveSession(); err != nil {
		t.Fatal(err)
	}
	a.Close()

	loaded := &Agent{cwd: a.cwd, provider: "openai", modelName: "test"}
	defer loaded.Close()
	if err := loaded.ResumeSession(id, root); err != nil {
		t.Fatal(err)
	}
	conversation := loaded.Conversation()
	if len(conversation) != 4 || conversation[1].Type != "tool_call" || conversation[2].Text != OmittedToolResult || conversation[3].Text != "done" {
		t.Fatalf("restored conversation = %#v", conversation)
	}
	loaded.history = append(loaded.history, historyItem{Type: "message", Role: "user", Text: "next"})
	input := loaded.input()
	if len(input) != 5 || input[1].Type != "tool_call" || input[2].Text != OmittedToolResult || input[3].Text != "done" || input[4].Text != "next" {
		t.Fatalf("model input = %#v", input)
	}
}

func TestConversationRestoresDisplayableMessages(t *testing.T) {
	a := &Agent{history: []historyItem{
		{Type: "message", Role: "user", Text: "[2026-08-27T14:03:01+02:00]\n\nfix session restoring"},
		{Type: "tool_call", CallID: "call-1", Name: "repl", Arguments: json.RawMessage(`{"code":"pwd"}`)},
		{Type: "tool_result", CallID: "call-1", Name: "repl", Text: "/tmp\n"},
		{Type: "message", Role: "assistant", Text: "Fixed."},
	}}
	got := a.Conversation()
	if len(got) != 4 || got[0].Role != "user" || got[0].Text != "fix session restoring" ||
		got[1].Type != "tool_call" || got[1].ToolID != "call-1" || got[1].ToolName != "repl" || got[1].ToolInput != "pwd" ||
		got[2].Type != "tool_result" || got[2].Text != "/tmp\n" || got[3].Role != "assistant" || got[3].Text != "Fixed." {
		t.Fatalf("conversation = %#v", got)
	}
}

func TestSessionSaveReplacesCurrentState(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	a := &Agent{cwd: cwd, provider: "openai", modelName: "test"}
	if err := a.StartSession(id, root); err != nil {
		t.Fatal(err)
	}
	a.history = append(a.history, historyMessage("user", "hello"))
	if err := a.SaveSession(); err != nil {
		t.Fatal(err)
	}

	saved, err := readSession(filepath.Join(root, id, sessionFilename))
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.History) != 1 || saved.History[0].Text != "hello" {
		t.Fatalf("saved history = %#v", saved.History)
	}
}

func TestUndoCompactedTurnUsesCanonicalHistory(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	a := &Agent{
		cwd: cwd, provider: "openai", modelName: "test",
		history: []historyItem{
			historyMessage("user", "first"),
			historyMessage("assistant", "one"),
			historyMessage("user", "second"),
			historyMessage("assistant", "two"),
			historyMessage("user", "third"),
			historyMessage("assistant", "three"),
		},
		compaction: &compactionState{Summary: "first and second", FirstKeptItem: 4},
	}
	if err := a.StartSession(id, root); err != nil {
		t.Fatal(err)
	}
	text, err := a.Undo(1)
	if err != nil || text != "second" {
		t.Fatalf("Undo() = %q, %v", text, err)
	}
	if len(a.history) != 2 || a.compaction != nil {
		t.Fatalf("history = %#v, compaction = %#v", a.history, a.compaction)
	}

	loaded := &Agent{cwd: cwd, provider: "openai", modelName: "test"}
	if err := loaded.ResumeSession(id, root); err != nil {
		t.Fatal(err)
	}
	if len(loaded.history) != 2 || loaded.compaction != nil {
		t.Fatalf("loaded history = %#v, compaction = %#v", loaded.history, loaded.compaction)
	}
}

func TestResumeSessionAllowsDifferentModel(t *testing.T) {
	t.Setenv("FN_PROVIDER", "anthropic")
	t.Setenv("FN_MODEL", "claude-test")
	root, cwd := t.TempDir(), t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	original := &Agent{cwd: cwd, provider: "openai", modelName: "gpt-test"}
	if err := original.StartSession(id, root); err != nil {
		t.Fatal(err)
	}

	resumed := &Agent{cwd: cwd, provider: "anthropic", modelName: "claude-test"}
	if err := resumed.ResumeSession(id, root); err != nil {
		t.Fatal(err)
	}
	if resumed.provider != "anthropic" || resumed.modelName != "claude-test" {
		t.Fatalf("resumed with %s/%s", resumed.provider, resumed.modelName)
	}
}

func TestResumeSessionRejectsImplicitDifferentModel(t *testing.T) {
	t.Setenv("FN_PROVIDER", "")
	t.Setenv("FN_MODEL", "")
	root, cwd := t.TempDir(), t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	original := &Agent{cwd: cwd, provider: "openai", modelName: "gpt-test"}
	if err := original.StartSession(id, root); err != nil {
		t.Fatal(err)
	}

	resumed := &Agent{cwd: cwd, provider: "anthropic", modelName: "claude-test"}
	err := resumed.ResumeSession(id, root)
	if err == nil || !strings.Contains(err.Error(), "set both FN_PROVIDER and FN_MODEL") {
		t.Fatalf("ResumeSession() error = %v", err)
	}
}
