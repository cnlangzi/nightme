// Internal tests for the verifyHandoffFile helper. Lives in the
// handoff package (not handoff_test) so it can call the
// package-private verifyHandoffFile directly — there is no
// public surface for it, by design (it's a pure verification
// gate, not a reusable utility).
package handoff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// File exists, non-zero size → success reply naming the path
// and the byte count.
func TestVerifyHandoffFile_FileExists_ReportsSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".nightme"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, ".nightme", "handoff.md")
	want := "# Handoff\n\n## Task\nX\n"
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatalf("seed handoff.md: %v", err)
	}

	got := verifyHandoffFile(path)
	if !strings.HasPrefix(got, "✅") {
		t.Errorf("expected success prefix in %q", got)
	}
	if !strings.Contains(got, ".nightme/handoff.md") {
		t.Errorf("expected canonical path in %q", got)
	}
	if !strings.Contains(got, "saved") {
		t.Errorf("expected 'saved' phrasing in %q", got)
	}
}

// File missing → ❌ "did not write" hint so the user can recover
// (rerun /handoff or write the handoff into the file manually).
func TestVerifyHandoffFile_FileMissing_ReportsFailure(t *testing.T) {
	dir := t.TempDir()
	// Intentionally no mkdir, no file — Agent failed to write.
	path := filepath.Join(dir, ".nightme", "handoff.md")

	got := verifyHandoffFile(path)
	if !strings.HasPrefix(got, "❌") {
		t.Errorf("expected error prefix in %q", got)
	}
	if !strings.Contains(got, "did not write") {
		t.Errorf("expected 'did not write' phrasing in %q", got)
	}
}

// File exists but is zero bytes → ❌ "is empty" so the user
// knows the write produced a stub rather than a real handoff.
func TestVerifyHandoffFile_FileEmpty_ReportsFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".nightme"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, ".nightme", "handoff.md")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("seed empty handoff.md: %v", err)
	}

	got := verifyHandoffFile(path)
	if !strings.HasPrefix(got, "❌") {
		t.Errorf("expected error prefix in %q", got)
	}
	if !strings.Contains(got, "empty") {
		t.Errorf("expected 'empty' phrasing in %q", got)
	}
}
