package discord

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateStore_AtomicSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "discord_state.json")

	s, err := newStateStore(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.setSession("sess-abc", "wss://gateway.discord.gg"); err != nil {
		t.Fatalf("setSession: %v", err)
	}
	if err := s.setSeq(42); err != nil {
		t.Fatalf("setSeq: %v", err)
	}

	// Reload from disk to verify atomic save.
	s2, err := newStateStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	sessionID, lastSeq, resumeURL, version := s2.snapshot()
	if sessionID != "sess-abc" {
		t.Errorf("session_id = %q, want sess-abc", sessionID)
	}
	if lastSeq != 42 {
		t.Errorf("last_seq = %d, want 42", lastSeq)
	}
	if resumeURL != "wss://gateway.discord.gg" {
		t.Errorf("resume_gateway_url = %q", resumeURL)
	}
	if version != intentsVersion {
		t.Errorf("intents_version = %d, want %d", version, intentsVersion)
	}
}

func TestStateStore_Clear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "discord_state.json")

	s, _ := newStateStore(path)
	_ = s.setSession("sess-1", "wss://x")
	_ = s.setSeq(7)
	if err := s.clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}

	s2, _ := newStateStore(path)
	sessionID, lastSeq, _, _ := s2.snapshot()
	if sessionID != "" || lastSeq != 0 {
		t.Errorf("clear didn't reset: session=%q seq=%d", sessionID, lastSeq)
	}
}

func TestStateStore_MissingFile(t *testing.T) {
	dir := t.TempDir()
	s, err := newStateStore(filepath.Join(dir, "absent.json"))
	if err != nil {
		t.Fatalf("missing-file new: %v", err)
	}
	sessionID, _, _, _ := s.snapshot()
	if sessionID != "" {
		t.Errorf("missing file should yield empty store, got session=%q", sessionID)
	}
}

func TestStateStore_NoPathIsNoOp(t *testing.T) {
	s, err := newStateStore("")
	if err != nil {
		t.Fatalf("empty path: %v", err)
	}
	if err := s.setSession("x", "y"); err != nil {
		t.Errorf("setSession with no path: %v", err)
	}
	if err := s.setSeq(1); err != nil {
		t.Errorf("setSeq with no path: %v", err)
	}
}

func TestStateStore_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "discord_state.json")
	if err := os.WriteFile(path, []byte("not json {"), 0o600); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	if _, err := newStateStore(path); err == nil {
		t.Error("expected error on corrupt state file")
	}
}

func TestStateStore_DirCreatedOnSave(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b", "c", "discord_state.json")
	s, _ := newStateStore(deep)
	if err := s.setSession("s", "wss://x"); err != nil {
		t.Fatalf("setSession: %v", err)
	}
	if _, err := os.Stat(deep); err != nil {
		t.Errorf("state file not created: %v", err)
	}
}
