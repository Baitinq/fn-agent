package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPythonREPLGCObjects(t *testing.T) {
	dir := t.TempDir()
	objectsPath := filepath.Join(dir, "repl-objects")
	if err := os.Mkdir(objectsPath, 0o755); err != nil {
		t.Fatal(err)
	}

	writeFile := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []string{"current", "history", "shared", "garbage"} {
		writeFile(filepath.Join(objectsPath, object), object)
	}
	currentSnapshot := filepath.Join(dir, "repl.json")
	historySnapshot := filepath.Join(dir, "checkpoint.json")
	writeFile(currentSnapshot, `{"a":"current","b":"shared"}`)
	writeFile(historySnapshot, `{"a":"history","b":"shared"}`)

	repl := &pythonREPL{}
	if err := repl.gcObjects([]string{currentSnapshot, historySnapshot}, objectsPath); err != nil {
		t.Fatal(err)
	}

	for _, object := range []string{"current", "history", "shared"} {
		if _, err := os.Stat(filepath.Join(objectsPath, object)); err != nil {
			t.Errorf("referenced object %q was removed: %v", object, err)
		}
	}
	if _, err := os.Stat(filepath.Join(objectsPath, "garbage")); !os.IsNotExist(err) {
		t.Errorf("unreferenced object still exists: %v", err)
	}
}

func TestPythonREPLGCObjectsPreservesObjectsWhenManifestIsInvalid(t *testing.T) {
	dir := t.TempDir()
	objectsPath := filepath.Join(dir, "repl-objects")
	if err := os.Mkdir(objectsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(objectsPath, "object")
	if err := os.WriteFile(objectPath, []byte("object"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(dir, "repl.json")
	if err := os.WriteFile(snapshotPath, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	repl := &pythonREPL{}
	if err := repl.gcObjects([]string{snapshotPath}, objectsPath); err == nil {
		t.Fatal("expected invalid manifest error")
	}
	if _, err := os.Stat(objectPath); err != nil {
		t.Fatalf("object was removed after manifest error: %v", err)
	}
}
