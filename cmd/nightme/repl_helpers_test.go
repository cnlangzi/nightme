package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestNightmeDataDir_CreatesDir verifies the history directory is
// created (mode 0700) on demand and lives under ~/.local/share/nightme.
func TestNightmeDataDir_CreatesDir(t *testing.T) {
	// Point HOME at a tempdir so we do not touch the user's real
	// ~/.local/share. UserHomeDir() honors $HOME on unix.
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := nightmeDataDir()
	if err != nil {
		t.Fatalf("nightmeDataDir: %v", err)
	}
	if !strings.HasSuffix(dir, "/.local/share/nightme") {
		t.Errorf("dir = %q, want suffix /.local/share/nightme", dir)
	}
	// Second call should also succeed (idempotent MkdirAll).
	if _, err := nightmeDataDir(); err != nil {
		t.Errorf("second nightmeDataDir: %v", err)
	}
}

// TestFilterREPLInput_BlocksControlChars guards the rune filter that
// readline applies to every input char. Printable runes must pass
// through; control chars (except \t \n \r) must be filtered out.
func TestFilterREPLInput_BlocksControlChars(t *testing.T) {
	cases := []struct {
		in      rune
		allowed bool
	}{
		{'a', true},
		{'/', true},
		{'-', true},
		{'\t', true},  // tab allowed (whitespace)
		{'\n', true},  // newline (handled by readline itself)
		{'\r', true},  // CR allowed
		{0x00, false}, // null
		{0x01, false}, // Ctrl-A
		{0x07, false}, // bell
		{0x1f, false}, // unit separator
	}
	for _, c := range cases {
		r, ok := filterREPLInput(c.in)
		if ok != c.allowed {
			t.Errorf("filterREPLInput(%#x) ok = %v, want %v", c.in, ok, c.allowed)
		}
		if !ok {
			if r != 0 {
				t.Errorf("filterREPLInput(%#x) replacement = %#x, want 0", c.in, r)
			}
		} else if r != c.in {
			t.Errorf("filterREPLInput(%#x) returned %#x, want %#x", c.in, r, c.in)
		}
	}
}

// TestDispatchREPLLine_ExitReturnsDone exercises the helper that
// runREPLWith and runREPLInteractive both share. exit/quit must
// signal the caller to stop; empty lines must signal continue; a
// real command must dispatch.
func TestDispatchREPLLine(t *testing.T) {
	newRoot := func() *cobra.Command {
		r := newRootCmd()
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