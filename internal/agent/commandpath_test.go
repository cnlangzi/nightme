package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveCommand_Empty rejects an empty command so a missing
// bridge wire-up surfaces as an error rather than silently
// passing through.
func TestResolveCommand_Empty(t *testing.T) {
	if err := ResolveCommand(""); err == nil {
		t.Errorf("empty command should fail")
	}
}

// TestResolveCommand_AbsolutePath verifies an absolute path is
// stat-checked directly (no PATH walk).
func TestResolveCommand_AbsolutePath(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "fake")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := ResolveCommand(bin); err != nil {
		t.Errorf("ResolveCommand(%q) = %v, want nil", bin, err)
	}
}

// TestResolveCommand_AbsolutePathMissing returns the underlying
// stat error so the caller can surface "the file you configured
// does not exist".
func TestResolveCommand_AbsolutePathMissing(t *testing.T) {
	if err := ResolveCommand("/nonexistent/path/does/not/exist"); err == nil {
		t.Errorf("missing absolute path should fail")
	}
}

// TestResolveCommand_DirectoryRejected treats a directory at the
// configured absolute path as a failure so cfg.Agents pointing
// at a folder does not silently pass.
func TestResolveCommand_DirectoryRejected(t *testing.T) {
	tmp := t.TempDir()
	if err := ResolveCommand(tmp); err == nil {
		t.Errorf("directory path should fail ResolveCommand")
	}
}
