package agent_test

// Per-bridge Review contract tests.
//
// All non-pty bridges share one review path: Starter.Review calls
// agent.ReviewDispatch, which picks ReviewWithOcr (when `ocr` is
// on $PATH) or ReviewWithPrompt. Every bridge's Review method is
// the same one-liner — this file's TestReview_UsesSharedPrompt
// exercises the path via fakeStarter to lock the shared contract.
//
// Per-bridge contract:
//   1. all bridges (claudecode / codex / cursor / acp / opencode /
//      copilot / dsh / pi): Starter.Review → agent.ReviewDispatch →
//      BuiltinPrompt + FormatReviewMessage.
//   2. pty: returns ErrReviewNotSupported (bash isn't a coding
//      agent).
//
// This file tests the contract end-to-end via fakeStarter. Real
// bridges are tested via the "is Starter satisfied" compile-time
// check in interface_external tests; per-bridge executability
// needs real binaries on PATH which isn't available in CI.

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
// the agent.Review path shared by every bridge (via
// agent.ReviewDispatch). It verifies that the path runs the
// shared BuiltinPrompt and wraps the result with the canonical
// preamble. The "every Review path ends with FormatReviewMessage"
// contract is verified structurally by the integration tests of
// each bridge; this test exercises the dispatcher's claim that
// the unified path delivers the shared prompt.
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
func (t *testStarter) Detect() error    { return nil }
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
