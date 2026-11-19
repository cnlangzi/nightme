package me

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteStateTemp_TheNameIsItsOwn — two writes that happen at the
// same time do not write into the same file.
func TestWriteStateTemp_TheNameIsItsOwn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "one.json")

	first, err := writeStateTemp(path, []byte("first"))
	if err != nil {
		t.Fatalf("write the first state: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(first) })
	second, err := writeStateTemp(path, []byte("second"))
	if err != nil {
		t.Fatalf("write the second state: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(second) })

	if first == second {
		t.Fatalf("the two writes share the file %q", first)
	}
	for name, want := range map[string]string{first: "first", second: "second"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %q: %v", name, err)
		}
		if string(raw) != want {
			t.Errorf("%q holds %q, want %q", name, raw, want)
		}
	}
}

// TestReplaceStateFile_TheStateIsReplaced — the state that lands is
// the state that was written, whole, and the temp file is gone.
func TestReplaceStateFile_TheStateIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "one.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("write the old state: %v", err)
	}

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
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("the temp file %q is still there", tmp)
	}
}

// TestReplaceFileRetrying_TheTransientErrorIsTriedAgain — a rename
// that a reader refuses is tried again.
func TestReplaceFileRetrying_TheTransientErrorIsTriedAgain(t *testing.T) {
	calls := 0
	err := replaceFileRetrying("a", "b", func(string, string) error {
		calls++
		if calls < 3 {
			return errors.New("the file is held open")
		}
		return nil
	}, func(error) bool { return true })
	if err != nil {
		t.Fatalf("replace the file: %v", err)
	}
	if calls != 3 {
		t.Errorf("the rename was tried %d times, want 3", calls)
	}
}

// TestReplaceFileRetrying_TheRealErrorIsNotTriedAgain — a rename that
// fails for a real reason is not tried again.
func TestReplaceFileRetrying_TheRealErrorIsNotTriedAgain(t *testing.T) {
	calls := 0
	want := errors.New("the disk is full")
	err := replaceFileRetrying("a", "b", func(string, string) error {
		calls++
		return want
	}, func(error) bool { return false })
	if !errors.Is(err, want) {
		t.Fatalf("the error is %v, want %v", err, want)
	}
	if calls != 1 {
		t.Errorf("the rename was tried %d times, want 1", calls)
	}
}
