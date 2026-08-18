package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestNightmeDataDir_Obsoleted was removed when the REPL switched to
// in-memory history — the helper that created ~/.nightme subdirs
// is no longer needed. History now lives only in the readline ring
// buffer and is discarded when the session ends (per Devin:
// "history in memory is enough").

// TestDispatchREPLLine_ExitReturnsDone exercises the helper that
// runREPLWith and runREPLInteractive both share. exit/quit must
// signal the caller to stop; empty lines must signal continue; a
// real command must dispatch.
func TestDispatchREPLLine(t *testing.T) {
	newRoot := func() *cobra.Command {
		r, _ := newRootCmd()
		r.SetOut(&bytes.Buffer{})
		r.SetErr(&bytes.Buffer{})
		return r
	}

	t.Run("exit returns done", func(t *testing.T) {
		root := newRoot()
		var buf bytes.Buffer
		done, err := dispatchREPLLine(root, nil, "exit", &buf)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if !done {
			t.Errorf("done = false, want true for exit")
		}
		if !strings.Contains(buf.String(), "bye") {
			t.Errorf("output missing bye: %q", buf.String())
		}
	})

	t.Run("quit returns done", func(t *testing.T) {
		root := newRoot()
		var buf bytes.Buffer
		done, err := dispatchREPLLine(root, nil, "  quit  ", &buf)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if !done {
			t.Errorf("done = false, want true for quit")
		}
	})

	t.Run("empty line returns not-done", func(t *testing.T) {
		root := newRoot()
		var buf bytes.Buffer
		done, err := dispatchREPLLine(root, nil, "   ", &buf)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if done {
			t.Errorf("done = true, want false for empty input")
		}
	})

	t.Run("command dispatches and re-prompts", func(t *testing.T) {
		root := newRoot()
		var buf bytes.Buffer
		done, err := dispatchREPLLine(root, nil, "version", &buf)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if done {
			t.Errorf("done = true, want false for normal command")
		}
		if !strings.Contains(buf.String(), "nightme version") {
			t.Errorf("output missing version line: %q", buf.String())
		}
		if !strings.HasSuffix(buf.String(), "nightme> ") {
			t.Errorf("output does not end with re-prompt: %q", buf.String())
		}
	})
}
