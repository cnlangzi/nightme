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

// TestYesNoPrompt_BufferFallback verifies the no-VT fallback
// path: when the writer is not a *os.File (here bytes.Buffer,
// which is what every test uses), styleEnabled returns false
// and paint emits the SGR parameter as visible ASCII text
// rather than a real CSI sequence. The "[y/N]" marker and
// the question text are still readable; the user sees the
// "[36m" prefix as a hint of which style would have applied.
func TestYesNoPrompt_BufferFallback(t *testing.T) {
	var buf bytes.Buffer
	got := yesNoPrompt(&buf, "Update now?")
	if !strings.Contains(got, "Update now?") || !strings.Contains(got, "[y/N]") {
		t.Errorf("yesNoPrompt = %q", got)
	}
	// No raw ESC byte — the fallback strips it.
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("buffer fallback should not emit ESC byte: %q", got)
	}
	// The visible SGR prefix is present.
	if !strings.Contains(got, "[36m") {
		t.Errorf("buffer fallback should emit visible [36m, got %q", got)
	}
}

// TestPaint_NonVT_FallbackAscii exercises every helper on a
// non-*os.File writer and confirms none emit a real CSI byte.
// Each helper also leaves a visible "[Nm" trail so an
// operator looking at logs from a no-VT host can still tell
// what style would have applied.
func TestPaint_NonVT_FallbackAscii(t *testing.T) {
	var buf bytes.Buffer
	cases := []struct {
		name    string
		got     string
		wantTag string
	}{
		{"red", paintRed(&buf, "x"), "[31m"},
		{"green", paintGreen(&buf, "x"), "[32m"},
		{"yellow", paintYellow(&buf, "x"), "[33m"},
		{"cyan", paintCyan(&buf, "x"), "[36m"},
		{"dim", paintDim(&buf, "x"), "[2m"},
	}
	for _, c := range cases {
		if strings.ContainsRune(c.got, '\x1b') {
			t.Errorf("%s: fallback must not emit ESC byte, got %q", c.name, c.got)
		}
		if !strings.Contains(c.got, c.wantTag) {
			t.Errorf("%s: fallback should emit %s, got %q", c.name, c.wantTag, c.got)
		}
		if !strings.Contains(c.got, "[0m") {
			t.Errorf("%s: fallback should emit [0m reset, got %q", c.name, c.got)
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

// TestStripCSI confirms the helper that backs the no-VT
// fallback path. CSI sequences ("\x1b[Nm") become their
// visible ASCII equivalent ("[Nm"); non-CSI strings are
// returned unchanged.
func TestStripCSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[33m", "[33m"},
		{"\x1b[33m\x1b[1m", "[33m[1m"},
		{"plain", "plain"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripCSI(c.in); got != c.want {
			t.Errorf("stripCSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}