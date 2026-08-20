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
