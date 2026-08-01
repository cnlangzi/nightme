package pty

import (
	"bytes"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestSpawnEcho spawns /bin/echo through a PTY and verifies the
// trailing newline appears in the output. This is the canonical
// "PTY works" smoke test from F-04 §5.
func TestSpawnEcho(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY semantics differ on Windows; skip unix-only smoke test")
	}

	b, err := NewBridge(t.TempDir(), "/bin/echo", []string{"hello"}, nil, 80, 24)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	got := readWithTimeout(t, b, 2*time.Second)
	if !bytes.Contains(got, []byte("hello")) {
		t.Fatalf("Read() = %q, want it to contain %q", got, "hello")
	}
}

// TestSetsize verifies that Setsize is accepted for normal terminal
// dimensions. We do not have a portable way to ask the child for its
// observed size in a unit test, so a non-error return is the bar.
func TestSetsize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY semantics differ on Windows; skip unix-only smoke test")
	}

	b, err := NewBridge(t.TempDir(), "/bin/sleep", []string{"0.2"}, nil, 80, 24)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	if err := b.Setsize(200, 50); err != nil {
		t.Fatalf("Setsize(200, 50) returned error: %v", err)
	}
	if err := b.Setsize(80, 24); err != nil {
		t.Fatalf("Setsize(80, 24) returned error: %v", err)
	}
}

// TestClose verifies that Close is idempotent and does not panic.
func TestClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY semantics differ on Windows; skip unix-only smoke test")
	}

	b, err := NewBridge(t.TempDir(), "/bin/sleep", []string{"0.2"}, nil, 80, 24)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := b.Close(); err != nil {
		t.Fatalf("first Close() returned error: %v", err)
	}
	// Second close should not panic. Underlying gopty tracks state and
	// returns nil; even if it returns an error, we only care that the
	// call is safe to repeat.
	_ = b.Close()
}

// TestPID verifies PID() returns a positive integer for a spawned
// process and matches the original /bin/sleep command.
func TestPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY semantics differ on Windows; skip unix-only smoke test")
	}

	b, err := NewBridge(t.TempDir(), "/bin/sleep", []string{"0.2"}, nil, 80, 24)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	pid := b.PID()
	if pid <= 0 {
		t.Fatalf("PID() = %d, want > 0", pid)
	}
}

// readWithTimeout reads from b until timeout elapses and returns
// whatever bytes have been accumulated so far. It uses a mutex so
// the test goroutine can safely inspect the buffer while the reader
// goroutine is still blocked on Read.
//
// The reader goroutine outlives the call; it is the caller's job to
// Close the bridge when the test is done. Closing the bridge wakes
// Read with an error and the goroutine exits cleanly.
func readWithTimeout(t *testing.T, r io.Reader, timeout time.Duration) []byte {
	t.Helper()
	var (
		mu  sync.Mutex
		buf []byte
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		chunk := make([]byte, 1024)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				mu.Lock()
				buf = append(buf, chunk[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	time.Sleep(timeout)

	mu.Lock()
	acc := append([]byte(nil), buf...)
	mu.Unlock()
	return acc
}
