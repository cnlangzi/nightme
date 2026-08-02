package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateV1ToV2_NoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	count, err := MigrateV1ToV2(path)
	if err != nil {
		t.Fatalf("MigrateV1ToV2: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected count=0, got %d", count)
	}

	// No backup should exist.
	if _, err := os.Stat(path + ".v1.bak"); !os.IsNotExist(err) {
		t.Fatalf("backup should not exist, got err=%v", err)
	}
}

func TestMigrateV1ToV2_WithEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	// Seed a v1.x registry file.
	v1Entries := map[string]Entry{
		"sess_1": {
			SessionID: "sess_1",
			Workspace: "/code/bailing",
			Agent:     "claude",
			PID:       12345,
			Status:    StatusRunning,
			StartedAt: time.Now(),
			LastRunAt: time.Now(),
		},
		"sess_2": {
			SessionID: "sess_2",
			Workspace: "/code/nightme",
			Agent:     "codex",
			PID:       67890,
			Status:    StatusDetached,
			StartedAt: time.Now(),
			LastRunAt: time.Now(),
		},
	}
	data, err := json.MarshalIndent(v1Entries, "", "  ")
	if err != nil {
		t.Fatalf("marshal v1 entries: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write v1 file: %v", err)
	}

	// Run migration.
	count, err := MigrateV1ToV2(path)
	if err != nil {
		t.Fatalf("MigrateV1ToV2: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected count=2, got %d", count)
	}

	// Backup should exist.
	bakPath := path + ".v1.bak"
	bakData, err := os.ReadFile(bakPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var bakEntries map[string]Entry
	if err := json.Unmarshal(bakData, &bakEntries); err != nil {
		t.Fatalf("unmarshal backup: %v", err)
	}
	if len(bakEntries) != 2 {
		t.Fatalf("backup should have 2 entries, got %d", len(bakEntries))
	}

	// Original v1 file is untouched (caller decides what to do).
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("v1 file should still exist: %v", err)
	}
}

func TestMigrateV1ToV2_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	// Seed v1 file.
	entries := map[string]Entry{
		"sess_1": {SessionID: "sess_1", Workspace: "/x", Agent: "claude", PID: 1, Status: StatusRunning},
	}
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// First migration.
	count1, err := MigrateV1ToV2(path)
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if count1 != 1 {
		t.Fatalf("expected count=1, got %d", count1)
	}

	// Modify the v1 file (simulate new session added after migration).
	entries["sess_2"] = Entry{SessionID: "sess_2", Workspace: "/y", Agent: "codex", PID: 2, Status: StatusRunning}
	data, _ = json.Marshal(entries)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	// Second migration: backup should NOT be overwritten (idempotent).
	bakPath := path + ".v1.bak"
	bakDataBefore, _ := os.ReadFile(bakPath)

	count2, err := MigrateV1ToV2(path)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	// count2 still reflects current v1 file (2 entries), even though
	// we did not re-back-up.
	if count2 != 2 {
		t.Fatalf("expected count=2, got %d", count2)
	}

	bakDataAfter, _ := os.ReadFile(bakPath)
	if string(bakDataBefore) != string(bakDataAfter) {
		t.Fatalf("backup was overwritten; expected idempotent")
	}
}

func TestMigrateV1ToV2_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	// Write garbage that won't unmarshal.
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	count, err := MigrateV1ToV2(path)
	if err != nil {
		t.Fatalf("MigrateV1ToV2 (corrupt): %v", err)
	}
	if count != 0 {
		t.Fatalf("expected count=0 for corrupt, got %d", count)
	}

	// Corrupt file should still be backed up (so user can recover).
	if _, err := os.Stat(path + ".v1.bak"); err != nil {
		t.Fatalf("corrupt backup should exist: %v", err)
	}
}

func TestV1RegistryHasData(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	if V1RegistryHasData(missing) {
		t.Fatalf("expected false for missing file")
	}

	exists := filepath.Join(dir, "exists.json")
	if err := os.WriteFile(exists, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !V1RegistryHasData(exists) {
		t.Fatalf("expected true for existing file")
	}
}

// TestNewChatSessionsFile verifies the container is initialized at
// the current schema version with an empty map (not nil).
func TestNewChatSessionsFile(t *testing.T) {
	f := NewChatSessionsFile()
	if f.Version != ChatSessionFileVersion {
		t.Fatalf("expected version=%d, got %d", ChatSessionFileVersion, f.Version)
	}
	if f.ChatSessions == nil {
		t.Fatalf("ChatSessions should be non-nil")
	}
	if len(f.ChatSessions) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(f.ChatSessions))
	}
}

func TestNewAgentSessionsFile(t *testing.T) {
	f := NewAgentSessionsFile()
	if f.Version != AgentSessionFileVersion {
		t.Fatalf("expected version=%d, got %d", AgentSessionFileVersion, f.Version)
	}
	if f.AgentSessions == nil {
		t.Fatalf("AgentSessions should be non-nil")
	}
	if len(f.AgentSessions) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(f.AgentSessions))
	}
}