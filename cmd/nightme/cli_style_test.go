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

func TestYesNoPromptPlain(t *testing.T) {
	var buf bytes.Buffer
	got := yesNoPrompt(&buf, "Update now?")
	if !strings.Contains(got, "Update now?") || !strings.Contains(got, "[y/N]") {
		t.Errorf("yesNoPrompt = %q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("buffer writer should not be colourised: %q", got)
	}
}

// TestPaint_EmptyString verifies the empty-input short-circuit
// — paint on "" returns "" without even consulting styleEnabled.
func TestPaint_EmptyString(t *testing.T) {
	var buf bytes.Buffer
	if got := paintRed(&buf, ""); got != "" {
		t.Errorf("paintRed(\"\") = %q, want \"\"", got)
	}
	if got := paintGreen(&buf, ""); got != "" {
		t.Errorf("paintGreen(\"\") = %q, want \"\"", got)
	}
	if got := paintYellow(&buf, ""); got != "" {
		t.Errorf("paintYellow(\"\") = %q, want \"\"", got)
	}
	if got := paintCyan(&buf, ""); got != "" {
		t.Errorf("paintCyan(\"\") = %q, want \"\"", got)
	}
	if got := paintDim(&buf, ""); got != "" {
		t.Errorf("paintDim(\"\") = %q, want \"\"", got)
	}
}

// TestPaint_NonOSFileWriter verifies any writer that is not a
// real *os.File (e.g. bytes.Buffer used by tests) bypasses the
// platform TTY probe and gets plain text. This is the "tests
// see plain text" contract documented in cli_style.go.
func TestPaint_NonOSFileWriter(t *testing.T) {
	var buf bytes.Buffer
	helpers := []struct {
		name string
		got  string
	}{
		{"red", paintRed(&buf, "boom")},
		{"green", paintGreen(&buf, "ok")},
		{"yellow", paintYellow(&buf, "warn")},
		{"cyan", paintCyan(&buf, "?")},
		{"dim", paintDim(&buf, "x")},
	}
	for _, c := range helpers {
		if strings.Contains(c.got, "\x1b[") {
			t.Errorf("%s: bytes.Buffer writer should not be colourised, got %q", c.name, c.got)
		}
	}
}

// TestPaint_NoColor_Buffer exercises the NO_COLOR short-circuit
// using the bytes.Buffer writer path (no real TTY required).
// styleEnabled looks at os.Getenv("NO_COLOR") first, so the
// helper must skip ANSI even on hosts where the env path is
// the only branch tested.
func TestPaint_NoColor_Buffer(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	if got := paintRed(&buf, "boom"); strings.Contains(got, "\x1b[") {
		t.Errorf("NO_COLOR should disable styling via buffer path, got %q", got)
	}
}