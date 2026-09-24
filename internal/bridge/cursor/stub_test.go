// stub_test.go — unit tests for the store.db stub workaround.
//
// These tests verify the embedded stub itself is a valid SQLite
// database with cursor's schema. The e2e probe (gated on
// NIGHTME_CURSOR_E2E=1) drives a real cursor-agent and is in
// resume_e2e_test.go.
package cursor

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestStubStoreDBTemplate_IsValidSQLite verifies the embedded
// stub bytes are a valid empty SQLite database.
func TestStubStoreDBTemplate_IsValidSQLite(t *testing.T) {
	if len(storeDBStub) == 0 {
		t.Fatal("storeDBStub is empty (go:embed misconfig)")
	}
	if !bytes.HasPrefix(storeDBStub, []byte("SQLite format 3\x00")) {
		t.Fatalf("stub missing SQLite magic header; first 16 bytes = %q", storeDBStub[:16])
	}
	if len(storeDBStub) < 4096 {
		t.Fatalf("stub too small (%d bytes); SQLite needs at least 1 page", len(storeDBStub))
	}
}

// TestEnsureStubStoreDB_WritesValidFile verifies ensureStubStoreDB
// actually creates a usable file under a session id.
func TestEnsureStubStoreDB_WritesValidFile(t *testing.T) {
	// Use a temporary HOME so we don't pollute the user's real
	// ~/.cursor/acp-sessions tree.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// On Windows os.UserHomeDir() reads USERPROFILE.
	t.Setenv("USERPROFILE", fakeHome)

	sessionID := "test-stub-session-aaaa-bbbb-cccc"
	if err := ensureStubStoreDB(sessionID); err != nil {
		t.Fatalf("ensureStubStoreDB: %v", err)
	}

	path := filepath.Join(fakeHome, ".cursor", "acp-sessions", sessionID, "store.db")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() != int64(len(storeDBStub)) {
		t.Fatalf("written size %d != embedded stub size %d", info.Size(), len(storeDBStub))
	}

	// Verify mode bits. Windows ignores os.WriteFile's mode
	// parameter (it has no Unix-style permission bits) and
	// reports a default 0666 from os.Stat; the test only makes
	// sense on POSIX. The 0o600 in ensureStubStoreDB itself is
	// still correct — it's a no-op on Windows but harmless.
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("store.db mode = %o, want 0600", mode)
		}
	}

	// Verify magic on disk matches embedded bytes.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back %s: %v", path, err)
	}
	if !bytes.HasPrefix(got, []byte("SQLite format 3\x00")) {
		t.Fatalf("on-disk stub missing SQLite magic; first 16 bytes = %q", got[:16])
	}
	if !bytes.Equal(got, storeDBStub) {
		t.Fatalf("on-disk stub differs from embedded bytes")
	}
}

// TestEnsureStubStoreDB_EmptySessionIDRejected — sanity check the
// pre-condition guard.
func TestEnsureStubStoreDB_EmptySessionIDRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := ensureStubStoreDB(""); err == nil {
		t.Fatal("ensureStubStoreDB(\"\") returned nil; want error")
	} else if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error = %v; want 'empty sessionId' message", err)
	}
}

// TestEnsureStubStoreDB_Idempotent — calling twice on the same id
// must not error (overwrite is fine; SQLite accepts a fresh db).
func TestEnsureStubStoreDB_Idempotent(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	sessionID := "test-stub-session-eeee-ffff-dddd"
	for i := 0; i < 3; i++ {
		if err := ensureStubStoreDB(sessionID); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
}
