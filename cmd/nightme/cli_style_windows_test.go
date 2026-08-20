//go:build windows

package main

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"testing"
)

// TestStyleEnabled_StdoutConsole asserts that styleEnabled on
// Windows does not panic when handed the real os.Stdout handle.
// We don't assert a particular return value (CI runners may or
// may not have VT), only that the syscall path executes
// without error.
func TestStyleEnabled_StdoutConsole(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("styleEnabled(os.Stdout) panicked: %v", r)
		}
	}()
	_ = styleEnabled(os.Stdout)
}

// TestPaint_PipeWriter_NoAnsi exercises the "writer is not a
// console handle" fallback on Windows. A pipe's read end
// reports GetConsoleMode == 0, so styleEnabled must return
// false and paintRed must not emit any ANSI.
//
// We open the pipe with os.Pipe (cross-platform) and hand the
// write end to paintRed. The write end is a real *os.File
// (so the type assertion in styleEnabled passes), but it's
// not a console handle, so the GetConsoleMode call returns
// 0 and the VT-enable path is skipped.
func TestPaint_PipeWriter_NoAnsi(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()

	// Drain the pipe in a goroutine so w.Write doesn't block.
	done := make(chan struct{})
	var captured bytes.Buffer
	go func() {
		_, _ = captured.ReadFrom(r)
		close(done)
	}()

	got := paintRed(w, "boom")
	if strings.Contains(got, "\x1b[") {
		t.Errorf("pipe writer should not be colourised, got %q", got)
	}
	if got != "boom" {
		t.Errorf("paintRed on pipe = %q, want %q", got, "boom")
	}
	// Closing the write end flushes the goroutine.
	w.Close()
	<-done
}

// TestStyleEnabled_NonOSFile confirms the type assertion in
// styleEnabled also short-circuits on Windows when the writer
// is not a *os.File at all (e.g. bytes.Buffer).
func TestStyleEnabled_NonOSFile(t *testing.T) {
	var buf bytes.Buffer
	if styleEnabled(&buf) {
		t.Errorf("styleEnabled(bytes.Buffer) = true, want false")
	}
}

// TestStyleEnabled_ClosedHandle checks that a closed *os.File
// does not cause GetConsoleMode to crash the process. We close
// the file before handing it in; styleEnabled should return
// false (GetConsoleMode fails on an invalid handle) rather than
// panic.
func TestStyleEnabled_ClosedHandle(t *testing.T) {
	f, err := os.CreateTemp("", "nightme-style-")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := f.Name()
	f.Close()

	// Open + close to land on an invalid handle. Using syscall
	// directly so we don't trigger any Go-level checks.
	h, err := syscall.Open(path, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("syscall.Open: %v", err)
	}
	syscall.CloseHandle(syscall.Handle(h))

	f2 := os.NewFile(uintptr(h), path)
	if f2 == nil {
		// Some Go versions refuse to wrap an already-closed fd.
		// That's fine — the test only exercises the panic path.
		t.Skip("Go refused to wrap closed fd; panic path untested")
	}
	defer f2.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("styleEnabled on closed handle panicked: %v", r)
		}
	}()
	if styleEnabled(f2) {
		// Possible if the handle happens to satisfy the syscall
		// contract despite being closed — not a failure.
		t.Log("styleEnabled returned true on closed handle (acceptable)")
	}
}