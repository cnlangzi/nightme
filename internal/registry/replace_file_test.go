//go:build !windows

package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReplaceFile_HappyPath: rename src → dst, dst doesn't exist
// before, ends up with the new content.
func TestReplaceFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("fresh"), 0o600); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	if err := replaceFile(src, dst); err != nil {
		t.Fatalf("replaceFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "fresh" {
		t.Errorf("dst = %q, want %q", got, "fresh")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src should be gone after rename, stat err = %v", err)
	}
}

// TestReplaceFile_OverExisting: rename src → dst where dst already
// exists. The POSIX path uses os.Rename which atomically replaces
// dst; the Windows path uses MoveFileEx with MOVEFILE_REPLACE_EXISTING
// to do the same. This test is gated to !windows because the bug
// we're guarding against is Windows-only; the POSIX path is well
// covered by os.Rename's own tests.
func TestReplaceFile_OverExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	if err := replaceFile(src, dst); err != nil {
		t.Fatalf("replaceFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("dst = %q, want %q (old content should be replaced)", got, "new")
	}
}

// TestWriteLocked_ReplacesExistingAgentSessions: integration
// coverage of the rename path that was failing on Windows
// (TestFlushHook_BusyQueues) — AgentSessionFile.Upsert on an
// existing file should not error on POSIX, and the new content
// should win.
func TestWriteLocked_ReplacesExistingAgentSessions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent_sessions.json")

	f, err := OpenAgentSessionFile(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// First write: seed an entry.
	f.entries["as_seed"] = &AgentSessionEntry{
		ID:    "as_seed",
		Agent: "claude",
		Cwd:   "/x",
	}
	if err := f.writeLocked(); err != nil {
		t.Fatalf("write1: %v", err)
	}

	// Second write: change content; replaceFile must succeed on
	// the existing file rather than fail with "Access is denied".
	f.entries["as_seed"].Cwd = "new"
	if err := f.writeLocked(); err != nil {
		t.Fatalf("write2 (replace): %v", err)
	}

	// Third writer: also succeeds. Sanity check that multiple
	// replacements in a row all go through.
	f.entries["as_seed"].Cwd = "newer"
	if err := f.writeLocked(); err != nil {
		t.Fatalf("write3 (replace again): %v", err)
	}
}
