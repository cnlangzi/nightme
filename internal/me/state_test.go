package me

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/wfe"
)

func TestStateStore_SaveLoad(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStateStore(dir)
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}
	state := newRunState("test-run-1", nil, "/tmp",
		wfe.Event{}, "chat-1",
		map[string]string{"X": "1"}, time.Now())
	if err := store.Save(state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load("test-run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ChatID != "chat-1" {
		t.Errorf("ChatID = %q", loaded.ChatID)
	}
	if loaded.Env["X"] != "1" {
		t.Errorf("Env[X] = %q", loaded.Env["X"])
	}
	// Verify atomic write: no .tmp files left behind
	if _, err := os.Stat(filepath.Join(dir, "test-run-1.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("tmp file should not exist after Save")
	}
}

// TestStateStore_Load_MissingRun — an unknown run has never been
// persisted; the caller tells that apart from a real failure by the
// error it gets.
func TestStateStore_Load_MissingRun(t *testing.T) {
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}
	if _, err := store.Load("nope"); !os.IsNotExist(err) {
		t.Errorf("err = %v, want os.ErrNotExist", err)
	}
}
