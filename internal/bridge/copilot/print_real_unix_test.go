//go:build !windows

// Real-machine print-mode test for the copilot bridge.
//
// Spawns `copilot --allow-all-tools -p "..." -s` against the
// real binary on PATH, verifies clean text output (no stats
// decoration bleeding into RunResult.Text). Skips if no
// `copilot` binary on PATH.
//
// Mirrors cursor/print_stub_test.go shape.
package copilot

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestPrintMode_RealBinary_RunsAndReturnsText runs runPrintMode
// against the real copilot binary with a trivial prompt.
// Skipped when copilot isn't on PATH.
func TestPrintMode_RealBinary_RunsAndReturnsText(t *testing.T) {
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skipf("copilot binary not on PATH: %v", err)
	}

	s := NewStarter("copilot", "copilot", DefaultACPArgs)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	blocks := []agent.ContentBlock{
		{Type: agent.ContentText, Text: "Reply with just the word: PONG"},
	}

	result, err := runPrintMode(ctx, s, agent.StartConfig{
		Workspace: t.TempDir(),
	}, blocks)
	if err != nil {
		t.Fatalf("runPrintMode: %v", err)
	}
	if !strings.Contains(result.Text, "PONG") {
		t.Errorf("result.Text = %q, want contains PONG", result.Text)
	}
	// Without `-s` we'd see "Changes / AI Credits / Tokens /
	// Resume" in stdout. Lock the contract that printModeArgs
	// appends `-s` so downstream RunResult.Text is clean.
	if strings.Contains(result.Text, "AI Credits") ||
		strings.Contains(result.Text, "Resume ") ||
		strings.Contains(result.Text, "Changes") {
		t.Errorf("result.Text contains stats decoration; `-s` not honored: %q", result.Text)
	}
	t.Logf("got text: %q (elapsed %dms)", result.Text, result.DurationMs)
}

// TestPrintMode_RealBinary_EmptyPromptFails asserts the
// empty-prompt early-return path (matches cursor's behavior).
func TestPrintMode_RealBinary_EmptyPromptFails(t *testing.T) {
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skipf("copilot binary not on PATH: %v", err)
	}
	s := NewStarter("copilot", "copilot", DefaultACPArgs)
	_, err := runPrintMode(context.Background(), s, agent.StartConfig{
		Workspace: t.TempDir(),
	}, nil)
	if err == nil {
		t.Errorf("runPrintMode with empty blocks: err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "empty prompt") {
		t.Errorf("err = %v, want contains 'empty prompt'", err)
	}
}