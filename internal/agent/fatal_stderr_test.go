package agent

import "testing"

// TestFatalStderrError_KeepsTheLineVerbatim pins the contract's
// rendering rule: the chat shows the CLI's own words, so the line
// reaches the error text unchanged — no trimming, no prefix, no
// reworded summary.
func TestFatalStderrError_KeepsTheLineVerbatim(t *testing.T) {
	const line = `[claude-code:unrecognized_model] {"model":"MiniMax-M3[1m]","query_source":"sdk"}`
	err := FatalStderrError(line)
	if err == nil {
		t.Fatal("FatalStderrError returned nil")
	}
	if err.Error() != line {
		t.Errorf("Error() = %q, want %q", err.Error(), line)
	}
}

// TestFatalStderrError_EmptyLine pins the degenerate case: an empty
// line is still an error, it just says nothing.
func TestFatalStderrError_EmptyLine(t *testing.T) {
	err := FatalStderrError("")
	if err == nil {
		t.Fatal("FatalStderrError returned nil")
	}
	if err.Error() != "" {
		t.Errorf("Error() = %q, want empty", err.Error())
	}
}

// TestFatalStderr_IsAPredicate pins the shape bridges use: a plain
// func over one line.
func TestFatalStderr_IsAPredicate(t *testing.T) {
	var isFatal FatalStderr = func(line string) bool {
		return line == "boom"
	}
	if !isFatal("boom") {
		t.Error("isFatal(\"boom\") = false, want true")
	}
	if isFatal("calm") {
		t.Error("isFatal(\"calm\") = true, want false")
	}
}
