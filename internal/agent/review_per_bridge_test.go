package agent_test

// Per-bridge Review contract tests.
//
// Each coding bridge's Starter.Review method is a 1-line
// delegation to agent.Review — but the *contract* matters:
// the bridge must
//   1. call Review (which uses RunOnce internally)
//   2. return nil on success
//   3. propagate RunOnce errors as wrapped errors
//   4. NOT call rc.Inject when RunOnce fails (no point — the
//     review never completed)
//
// We test against a fakeStarter that records the RunOnce prompt
// + workspace it received and returns a canned RunResult. This
// replaces an earlier approach that tried to invoke real bridges
// (which need binaries on PATH and would fail in CI).
//
// The per-bridge "5 bridges all call agent.Review" assertion
// is a 1-line eyeball-check on the bridge starter.go files
// (each one is literally `return agent.Review(ctx, s, rc)`).
// The deeper contract is verified by the per-bridge path that
// goes through agent.Review with the fakeStarter.
//
// pty is covered by TestPtyStarter_ReviewReturnsNotSupported
// below — bash isn't a coding agent, so pty's Review returns
// the ErrReviewNotSupported sentinel rather than running a
// review one-shot.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
)

// TestReview_UsesSharedPrompt is the canonical contract
// test for the Review implementation — verified end-to-end with
// a fakeStarter. It replaces the old per-bridge tests that
// tried to invoke real bridges (which need binaries on PATH
// and would fail in CI). The bridge-side test that "all
// bridges call agent.Review" is now a 1-line eyeball-check
// (see the bridge starter.go files).
func TestReview_UsesSharedPrompt(t *testing.T) {
	const workspace = "/Users/me/proj"

	var gotWorkspace string
	var gotPrompt string
	fs := &testStarter{
		info: agent.NewInfo("fake", agent.ModeJSONIO, "fake", nil, nil),
		runOnce: func(_ context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
			gotWorkspace = cfg.Workspace
			if len(blocks) == 1 {
				gotPrompt = blocks[0].Text
			}
			return agent.RunResult{
				Text: "## Summary\nThe diff is fine.",
			}, nil
		},
	}

	var gotInjected []agent.ContentBlock
	rc := agent.ReviewContext{
		Workspace: workspace,
		Inject: func(_ context.Context, blocks []agent.ContentBlock) error {
			gotInjected = append([]agent.ContentBlock(nil), blocks...)
			return nil
		},
	}
	if err := agent.Review(context.Background(), fs, rc); err != nil {
		t.Fatalf("Review: %v", err)
	}

	// 1. The fresh subprocess runs in rc.Workspace.
	if gotWorkspace != workspace {
		t.Errorf("RunOnce called with workspace %q, want %q", gotWorkspace, workspace)
	}

	// 2. The prompt sent to the fresh subprocess is the shared
	// StandardPrompt() — not a per-bridge variant.
	if gotPrompt != agent.StandardPrompt() {
		t.Errorf("RunOnce prompt != StandardPrompt() — bridge should send the shared prompt")
	}

	// 3. The injected message is the formatted review result
	// (## Code review of <workspace> preamble + raw review),
	// not the raw prompt.
	if len(gotInjected) != 1 {
		t.Fatalf("Inject received %d blocks, want 1", len(gotInjected))
	}
	if gotInjected[0].Type != agent.ContentText {
		t.Errorf("injected block type %q, want %q", gotInjected[0].Type, agent.ContentText)
	}
	want := "## Code review of " + workspace + "\n\n(current branch vs default branch; run via /review)\n\n## Summary\nThe diff is fine."
	if gotInjected[0].Text != want {
		t.Errorf("injected text mismatch:\n got  %q\n want %q", gotInjected[0].Text, want)
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
		runOnce: func(_ context.Context, _ agent.StartConfig, _ []agent.ContentBlock) (agent.RunResult, error) {
			return agent.RunResult{}, runOnceErr
		},
	}

	var injected int
	rc := agent.ReviewContext{
		Workspace: "/ws",
		Inject: func(_ context.Context, _ []agent.ContentBlock) error {
			injected++
			return nil
		},
	}
	err := agent.Review(context.Background(), fs, rc)
	if err == nil {
		t.Fatal("Review should error on RunOnce failure, got nil")
	}
	if !strings.Contains(err.Error(), "review one-shot failed") {
		t.Errorf("error %q does not mention 'review one-shot failed'", err)
	}
	if !errors.Is(err, runOnceErr) {
		t.Errorf("error chain lost original: got %v, want wrap of %v", err, runOnceErr)
	}
	if injected != 0 {
		t.Errorf("Inject was called %d times after RunOnce failure, want 0", injected)
	}
}

// TestPtyStarter_ReviewReturnsNotSupported covers the bash / pty
// fallback. It's not a coding agent, so Review must return
// agent.ErrReviewNotSupported (the exact sentinel — the dispatcher
// matches on ==, not errors.Is).
func TestPtyStarter_ReviewReturnsNotSupported(t *testing.T) {
	s := pty.NewStarter("bash", "bash", nil, nil, 0, 0)
	err := s.Review(context.Background(), agent.ReviewContext{
		Workspace: "/ws",
		Inject:    func(_ context.Context, _ []agent.ContentBlock) error { return nil },
	})
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
	runOnce func(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error)
}

func (t *testStarter) Info() agent.Info { return t.info }
func (t *testStarter) Detect() error     { return nil }
func (t *testStarter) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return nil, errors.New("testStarter: Start not implemented")
}
func (t *testStarter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	return t.runOnce(ctx, cfg, blocks)
}
func (t *testStarter) Review(context.Context, agent.ReviewContext) error {
	return errors.New("testStarter: Review not implemented (we test Review directly)")
}