//go:build windows

package me

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReplaceStateFile_AReaderHoldsTheFileOpen — Windows refuses to
// replace a file that a reader holds open: the rename comes back with
// "access denied" until the reader lets go. The state of a run is
// worth a few tries.
func TestReplaceStateFile_AReaderHoldsTheFileOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "one.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("write the old state: %v", err)
	}

	reader, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the state: %v", err)
	}
	// The reader lets go after a moment, the way a daemon that reads
	// the state lets go.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = reader.Close()
	}()

	tmp, err := writeStateTemp(path, []byte("new"))
	if err != nil {
		t.Fatalf("write the new state: %v", err)
	}
	if err := replaceStateFile(tmp, path); err != nil {
		t.Fatalf("replace the state: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the state: %v", err)
	}
	if string(raw) != "new" {
		t.Errorf("the state holds %q, want %q", raw, "new")
	}
}
