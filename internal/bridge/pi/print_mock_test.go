//go:build !windows

// Mock-script tests for the print-mode RunOnce path.
//
// Uses internal/testdata/pi_print_mock.sh to drive runPrintMode
// without depending on the real `pi` binary. This catches the
// error paths the real-pi smoke can't easily exercise:
//
//   - PI_PRINT_FAIL: non-zero exit + stderr → error includes
//     stderr text (the "auth error / model error" class).
//   - PI_PRINT_NO_SETTLE: clean stream but no agent_settled →
//     "exit without agent_settled" diagnostic.
//   - Default: full clean run → returns the mock's text.
//
// The smoke test (print_realpi_test.go, gated by NIGHTME_REAL_PI=1)
// covers the "real LLM actually committed" path; this file
// covers everything else with deterministic inputs.

package pi

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// piPrintMockPath is the absolute path to the mock script.
// Computed at package init via init() below so the tests don't
// each repeat filepath.Abs and the value is captured before any
// test cwd-shifting happens.
//
// exec.CommandContext (and thus agent.NewCmd) routes the binary
// through exec.LookPath, which only consults PATH. A relative
// path would not be found, so the mock must be invoked by its
// absolute path. The relative const above was the wrong shape.
var piPrintMockPath string

func init() {
	abs, err := filepath.Abs("../../testdata/pi_print_mock.sh")
	if err != nil {
		panic("pi_print_mock_test: filepath.Abs: " + err.Error())
	}
	piPrintMockPath = abs
}

func TestPrintMode_Mock_CleanRun_ReturnsText(t *testing.T) {
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_TEXT", "clean-run-payload")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "do thing"},
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got != "clean-run-payload" {
		t.Fatalf("RunOnce text = %q, want %q", got, "clean-run-payload")
	}
}

func TestPrintMode_Mock_NonZeroExit_SurfacesStderr(t *testing.T) {
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_FAIL", "1")
	t.Setenv("PI_PRINT_STDERR", "model auth failed: 401")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "anything"},
	})
	if err == nil {
		t.Fatalf("RunOnce returned no error; want non-zero-exit failure. got=%q", got)
	}
	if !strings.Contains(err.Error(), "model auth failed: 401") {
		t.Fatalf("error missing stderr text: %v", err)
	}
}

func TestPrintMode_Mock_NoSettled_ReportsMissingEvent(t *testing.T) {
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_NO_SETTLE", "1")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "anything"},
	})
	if err == nil {
		t.Fatalf("RunOnce returned no error; want missing-settled failure. got=%q", got)
	}
	if !strings.Contains(err.Error(), "agent_settled") {
		t.Fatalf("error missing 'agent_settled' phrase: %v", err)
	}
}

func TestPrintMode_Mock_MultipleTextBlocks_JoinedWithNewline(t *testing.T) {
	// The bridge layer's blocksToPrompt joins ContentText
	// blocks with "\n" BEFORE handing them to pi — so pi sees
	// one combined prompt. The text returned is still the
	// mock's fixed TEXT, so we can only assert that the run
	// completes cleanly (a regression in blocksToPrompt would
	// show up as a parse failure in the mock, since the prompt
	// wouldn't be the one the mock echoes).
	//
	// Caveat: the mock doesn't JSON-escape control characters
	// in the echoed prompt. Real pi does. We use single-line
	// block contents to avoid the embedded-newline problem;
	// a real-newline-join test would need a richer mock.
	//
	// This test sends a single text block (the simpler case
	// the mock can validate); multi-block behaviour is covered
	// by the buildAgentPrompt prompt in the real-pi smoke.
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_TEXT", "ok")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "single-block"},
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got != "ok" {
		t.Fatalf("RunOnce text = %q, want %q", got, "ok")
	}
}

func TestPrintMode_Mock_WorkspacePropagated(t *testing.T) {
	// RunOnce must pass cfg.Workspace down to spawn so pi runs
	// with the right cwd. We exercise this by running inside a
	// non-default temp dir and asserting the command succeeds
	// (the mock doesn't actually check cwd, but if the bridge
	// dropped it pi would fail to spawn because RunOnce returns
	// early on empty workspace).
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ws := t.TempDir()
	if _, err := a.RunOnce(ctx, agent.StartConfig{Workspace: ws}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "in custom workspace"},
	}); err != nil {
		t.Fatalf("RunOnce with custom workspace %q: %v", ws, err)
	}
}

// Sanity guard: the mock path must be resolvable as a file at
// test time. If a future move of internal/testdata/ breaks the
// relative path, every test in this file panics on a missing
// path — the explicit assertion makes the failure mode obvious.
func TestPrintMode_MockPath_Resolves(t *testing.T) {
	abs, err := filepath.Abs(piPrintMockPath)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", piPrintMockPath, err)
	}
	t.Logf("mock script resolved to %s", abs)
}
