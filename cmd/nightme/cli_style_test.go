package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDisplayVer(t *testing.T) {
	if got := displayVer("v0.3.10"); got != "0.3.10" {
		t.Errorf("displayVer(v0.3.10) = %q, want 0.3.10", got)
	}
	if got := displayVer("0.3.10"); got != "0.3.10" {
		t.Errorf("displayVer(0.3.10) = %q, want 0.3.10", got)
	}
}

// TestYesNoPrompt_BufferFallback verifies the no-VT path:
// when the writer is not a *os.File (here bytes.Buffer),
// styleEnabled returns false and paint emits pure plain
// text — no ANSI codes, no visible "[36m" labels either.
// The "?" question-mark icon and the [y/N] marker stay
// readable; the terminal sees a clean ASCII stream.
func TestYesNoPrompt_BufferFallback(t *testing.T) {
	var buf bytes.Buffer
	got := yesNoPrompt(&buf, "Update now?")
	if !strings.Contains(got, "Update now?") || !strings.Contains(got, "[y/N]") {
		t.Errorf("yesNoPrompt = %q", got)
	}
	// No raw ESC byte — identity fallback.
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("buffer fallback should not emit ESC byte: %q", got)
	}
	// No visible SGR labels either — pure plain text.
	if strings.Contains(got, "[36m") {
		t.Errorf("buffer fallback should not emit visible [36m, got %q", got)
	}
	if !strings.Contains(got, "?") {
		t.Errorf("yesNoPrompt should contain the ? glyph, got %q", got)
	}
}

// TestPaint_NonVT_Identity confirms every helper on a
// non-*os.File writer (the test path) returns its argument
// verbatim. No ANSI, no visible labels — pure identity.
func TestPaint_NonVT_Identity(t *testing.T) {
	var buf bytes.Buffer
	cases := []struct {
		name string
		got  string
	}{
		{"red", paintRed(&buf, "x")},
		{"green", paintGreen(&buf, "x")},
		{"yellow", paintYellow(&buf, "x")},
		{"cyan", paintCyan(&buf, "x")},
		{"dim", paintDim(&buf, "x")},
	}
	for _, c := range cases {
		if c.got != "x" {
			t.Errorf("%s: identity fallback should return %q, got %q", c.name, "x", c.got)
		}
		if strings.ContainsRune(c.got, '\x1b') {
			t.Errorf("%s: identity fallback must not emit ESC byte, got %q", c.name, c.got)
		}
		if strings.Contains(c.got, "[") {
			t.Errorf("%s: identity fallback must not emit visible [, got %q", c.name, c.got)
		}
	}
}

// TestPaint_EmptyString verifies the empty-input short-circuit
// — paint on "" returns "" without consulting styleEnabled.
func TestPaint_EmptyString(t *testing.T) {
	var buf bytes.Buffer
	if got := paintRed(&buf, ""); got != "" {
		t.Errorf("paintRed(\"\") = %q, want \"\"", got)
	}
}