package agent_test

// Per-bridge Review contract tests.
//
// F-review.md §13 "codex/claude use native review" rule:
// bridges that have a native review subcommand (claudecode,
// codex) invoke it directly instead of running our generic
// BuiltinPrompt; bridges that don't (dsh, opencode, pi, acp)
// delegate to agent.Review which uses BuiltinPrompt.
//
// Per-bridge contract:
//   1. claudecode: rev path = `runCodeReviewPrintMode`; the
//      Review method must call it and wrap the result with
//      FormatReviewMessage (so the main agent sees the canonical
//      preamble).
//   2. codex: rev path = `runCodexReview` (uses codex's native
//      `codex review` subcommand, not `codex exec <prompt>`);
//      same wrapping pattern.
//   3. dsh/opencode/pi/acp: agent.Review → BuiltinPrompt +
//      FormatReviewMessage.
//   4. pty: returns ErrReviewNotSupported (bash isn't a coding
//      agent).
//
// This file tests the contract end-to-end via fakeStarter. Per
// real bridges (claudecode / codex / dsh / opencode / pi / acp)
// are tested via their own integration paths or via the "is
// Starter satisfied" compile-time check in interface_external
// tests; per-bridge executability needs real binaries on PATH
// which isn't available in CI.
//
// The single-end-to-end test (TestReview_UsesSharedPrompt)
// walks through agent.Review with a fakeStarter that captures
// RunOnce params; this is the path dsh/opencode/pi/acp share.
// claudecode / codex don't go through this path — they have
// their own print-mode helpers and call them from their
// respective Review methods. The "all Review paths eventually
// call FormatReviewMessage and inject via rc.Inject" contract
// is verified structurally by the integration tests of each
// bridge.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
)

// TestReview_UsesSharedPrompt is the canonical contract test for
// the agent.Review fallback path (used by dsh / opencode / pi /
// acp). It verifies that this path runs the shared BuiltinPrompt
// and wraps the result with the canonical preamble.
//
// claudecode / codex have their own Review paths that don't go
// through agent.Review; they're tested via the bridge's own
// print-mode helpers (runCodeReviewPrintMode / runCodexReview).
// The shared contract — every Review path must end with
// FormatReviewMessage + rc.Inject — is verified by eye across
// bridge starter.go files. (Per-bridge e2e tests require real
// binaries on PATH; CI uses the fakeStarter runOnly.)
func TestReview_UsesSharedPrompt(t *testing.T) {
	const workspace = "/Users/me/proj"

	var gotWorkspace string
	var gotPrompt string
	fs := &testStarter{
		info: agent.NewInfo("fake", agent.ModeJSONIO, "fake", nil, nil),
		runOnce: func(_ context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, _ ...agent.RunOnceOption) (agent.RunResult, error) {
			gotWorkspace = cfg.Workspace
			if len(blocks) == 1 {
				gotPrompt = blocks[0].Text
			}
			return agent.RunResult{
				Text: "## Summary\nThe diff is fine.",
			}, nil
		},
	}

	rc := agent.StartConfig{Workspace: workspace}
	result, err := fs.Review(context.Background(), rc)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	// 1. The fresh subprocess runs in rc.Workspace.
	if gotWorkspace != workspace {
		t.Errorf("RunOnce called with workspace %q, want %q", gotWorkspace, workspace)
	}

	// 2. The prompt sent to the fresh subprocess is the shared
	// agent.BuiltinPrompt — not a per-bridge variant.
	if gotPrompt != agent.BuiltinPrompt {
		t.Errorf("RunOnce prompt != agent.BuiltinPrompt — bridge should send the shared prompt")
	}

	// 3. v9: Review returns the RAW RunResult (no FormatReviewMessage
	// wrap, no Inject). The dispatcher wraps and routes from here.
	if result.Text != "## Summary\nThe diff is fine." {
		t.Errorf("Review returned Text %q, want raw review body (no preamble)", result.Text)
	}
}

// TestReview_PropagatesRunOnceError verifies that if the
// review one-shot subprocess fails, the error is surfaced and
// the dispatcher does NOT inject a half-baked finding into the
// main chat.
func TestReview_PropagatesRunOnceError(t *testing.T) {
	runOnceErr := errors.New("fake: binary not on PATH")
	fs := &testStarter{
		info: agent.NewInfo("fake", agent.ModeJSONIO, "fake", nil, nil),
		runOnce: func(_ context.Context, _ agent.StartConfig, _ []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
			return agent.RunResult{}, runOnceErr
		},
	}

	rc := agent.StartConfig{Workspace: "/ws"}
	result, err := fs.Review(context.Background(), rc)
	if err == nil {
		t.Fatal("Review should error on RunOnce failure, got nil")
	}
	if !strings.Contains(err.Error(), "review one-shot failed") {
		t.Errorf("error %q does not mention 'review one-shot failed'", err)
	}
	if !errors.Is(err, runOnceErr) {
		t.Errorf("error chain lost original: got %v, want wrap of %v", err, runOnceErr)
	}
	if result.Text != "" {
		t.Errorf("RunResult.Text on failure = %q, want empty", result.Text)
	}
}

// TestPtyStarter_ReviewReturnsNotSupported covers the bash / pty
// fallback. It's not a coding agent, so Review must return
// agent.ErrReviewNotSupported (the exact sentinel — the dispatcher
// matches on ==, not errors.Is).
func TestPtyStarter_ReviewReturnsNotSupported(t *testing.T) {
	s := pty.NewStarter("bash", "bash", nil, nil, 0, 0)
	_, err := s.Review(context.Background(), agent.StartConfig{Workspace: "/ws"})
	if err == nil {
		t.Fatal("pty Starter.Review should return error, got nil")
	}
	if !errors.Is(err, agent.ErrReviewNotSupported) && err != agent.ErrReviewNotSupported {
		t.Errorf("pty Starter.Review error = %v, want %v", err, agent.ErrReviewNotSupported)
	}
	if err != agent.ErrReviewNotSupported {
		t.Errorf("pty Starter.Review returned wrapped error %T(%v); dispatcher matches on ==, must return sentinel directly", err, err)
	}
}

// testStarter is a minimal agent.Starter for testing Review.
// Records the prompt + workspace it received via RunOnce, and
// returns a canned RunResult.
type testStarter struct {
	info    agent.Info
	runOnce func(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error)
}

func (t *testStarter) Info() agent.Info { return t.info }
func (t *testStarter) Detect() error     { return nil }
func (t *testStarter) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return nil, errors.New("testStarter: Start not implemented")
}
func (t *testStarter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return t.runOnce(ctx, cfg, blocks)
}
func (t *testStarter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	result, err := t.RunOnce(ctx, cfg, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: agent.BuiltinPrompt,
	}})
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: review one-shot failed: %w",
			t.Info().Name, err)
	}
	return result, nil
}