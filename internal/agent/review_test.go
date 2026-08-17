package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeStarter is a minimal agent.Starter for testing Review.
// It records the prompt + workspace it received and returns a
// canned RunResult.Text — no real subprocess is spawned.
type fakeStarter struct {
	name    string
	mode    Mode
	runOnce func(ctx context.Context, cfg StartConfig, blocks []ContentBlock) (RunResult, error)
}

func (f *fakeStarter) Info() Info { return NewInfo(f.name, f.mode, "fake-cmd", nil, nil) }
func (f *fakeStarter) Detect() error { return nil }
func (f *fakeStarter) Start(context.Context, StartConfig) (*Agent, error) {
	return nil, errors.New("fakeStarter: Start not implemented")
}
func (f *fakeStarter) RunOnce(ctx context.Context, cfg StartConfig, blocks []ContentBlock) (RunResult, error) {
	return f.runOnce(ctx, cfg, blocks)
}
func (f *fakeStarter) Review(context.Context, ReviewContext) error {
	return errors.New("fakeStarter: Review not implemented (we test Review directly)")
}

// TestStandardPrompt_Structure verifies the prompt contains the
// sections / severity rubric the chat agent is supposed to follow.
// The agent is free to add words around these, but it MUST emit
// the canonical structure or downstream IM rendering (and our
// future finding-card parser, v2) breaks.
//
// This is a smoke test, not a full LLM-output test — the real
// behavior is verified by /tmp/review-smoke e2e (see F-review.md
// §3.5). The point of these checks is to catch accidental edits
// that drop a required section from the prompt template.
func TestStandardPrompt_Structure(t *testing.T) {
	p := StandardPrompt()

	// Required sections the agent must follow.
	wantSections := []string{
		"# What to review",
		"# How to review",
		"# What to look for",
		"# Output format",
		"## Summary",
		"## Findings",
		"## Suggestions",
	}
	for _, s := range wantSections {
		if !strings.Contains(p, s) {
			t.Errorf("StandardPrompt() missing required section %q", s)
		}
	}

	// Required severity tags on findings.
	for _, sev := range []string{"blocker", "major", "minor", "nit"} {
		if !strings.Contains(p, "**"+sev+"**") {
			t.Errorf("StandardPrompt() missing severity tag %q", sev)
		}
	}

	// Required guardrails (the 6 "How to review" rules).
	wantRules := []string{
		"Read the diff first",
		"Distinguish BLOCKERS from nits",
		"Cite file:line",
		"Skip linter / typechecker",
		"Skip pre-existing issues",
		"False-positive filter",
	}
	for _, r := range wantRules {
		if !strings.Contains(p, r) {
			t.Errorf("StandardPrompt() missing rule %q", r)
		}
	}
}

// TestStandardPrompt_IncludesGitCommands verifies the prompt
// references the three git diff commands the chat agent must run.
// Each is essential to capturing the full "diff a PR would have".
func TestStandardPrompt_IncludesGitCommands(t *testing.T) {
	p := StandardPrompt()
	cmds := []string{
		"git fetch origin",
		"git diff <default-branch>...HEAD",
		"git diff --staged",
		"git diff", // bare for unstaged
	}
	for _, c := range cmds {
		if !strings.Contains(p, c) {
			t.Errorf("StandardPrompt() missing git command %q", c)
		}
	}
}

// TestErrReviewNotSupported_Sentinel verifies the error is
// non-nil and has a stable message — the dispatcher checks
// for this exact value (`if err == agent.ErrReviewNotSupported`)
// not via errors.Is, so a refactor that wraps it in
// fmt.Errorf would silently break the special-case reply.
func TestErrReviewNotSupported_Sentinel(t *testing.T) {
	if ErrReviewNotSupported == nil {
		t.Fatal("ErrReviewNotSupported is nil")
	}
	want := "agent: /review not supported"
	if ErrReviewNotSupported.Error() != want {
		t.Errorf("ErrReviewNotSupported message = %q, want %q",
			ErrReviewNotSupported.Error(), want)
	}
}

// TestReview_RejectsNilInject verifies the defensive guard.
// The dispatcher wires Inject to as.SendBlocks, so nil would
// only happen on a misuse — but failing fast beats panicking
// inside the bridge loop.
func TestReview_RejectsNilInject(t *testing.T) {
	err := Review(context.Background(), nil, ReviewContext{
		Workspace: "/some/path",
		Inject:    nil,
	})
	if err == nil {
		t.Fatal("Review with nil Inject should error, got nil")
	}
	if !strings.Contains(err.Error(), "Inject is nil") {
		t.Errorf("Review nil-Inject error %q, want it to mention Inject", err)
	}
}

// TestReview_InjectsFormattedReview verifies the happy path:
// Review calls s.RunOnce with StandardPrompt, captures the
// returned RunResult.Text, wraps it in formatReviewMessage (which
// adds a "## Code review of <workspace>" preamble), and injects
// the wrapped result via rc.Inject.
//
// Uses a fakeStarter that records the prompt it received and
// returns a canned RunResult.Text — no real subprocess is spawned.
func TestReview_InjectsFormattedReview(t *testing.T) {
	const reviewText = "## Summary\nThe diff is fine."

	var gotInjected []ContentBlock
	var capturedRunOnceBlocks []ContentBlock
	var capturedRunOnceWorkspace string

	fs := &fakeStarter{
		name: "fake",
		mode: ModeJSONIO,
		runOnce: func(_ context.Context, cfg StartConfig, blocks []ContentBlock) (RunResult, error) {
			capturedRunOnceWorkspace = cfg.Workspace
			capturedRunOnceBlocks = append([]ContentBlock(nil), blocks...)
			return RunResult{Text: reviewText}, nil
		},
	}

	rc := ReviewContext{
		Workspace: "/ws",
		Inject: func(_ context.Context, blocks []ContentBlock) error {
			gotInjected = append([]ContentBlock(nil), blocks...)
			return nil
		},
	}
	if err := Review(context.Background(), fs, rc); err != nil {
		t.Fatalf("Review returned error: %v", err)
	}

	// Verify RunOnce was called with rc.Workspace and StandardPrompt.
	if capturedRunOnceWorkspace != "/ws" {
		t.Errorf("RunOnce called with workspace %q, want %q",
			capturedRunOnceWorkspace, "/ws")
	}
	if len(capturedRunOnceBlocks) != 1 {
		t.Fatalf("RunOnce called with %d blocks, want 1", len(capturedRunOnceBlocks))
	}
	if capturedRunOnceBlocks[0].Type != ContentText {
		t.Errorf("RunOnce block type %q, want %q",
			capturedRunOnceBlocks[0].Type, ContentText)
	}
	if capturedRunOnceBlocks[0].Text != StandardPrompt() {
		t.Errorf("RunOnce block text != StandardPrompt() — bridge should send the shared prompt")
	}

	// Verify the injected content is the formatted review result,
	// not the raw prompt — main sees findings, not "go review".
	if len(gotInjected) != 1 {
		t.Fatalf("Review injected %d blocks, want 1", len(gotInjected))
	}
	if gotInjected[0].Type != ContentText {
		t.Errorf("injected block type %q, want %q", gotInjected[0].Type, ContentText)
	}
	want := "## Code review of /ws\n\n(current branch vs default branch; run via /review)\n\n" + reviewText
	if gotInjected[0].Text != want {
		t.Errorf("injected text mismatch:\n got  %q\n want %q", gotInjected[0].Text, want)
	}
}

// TestReview_PropagatesRunOnceError verifies that if the
// review one-shot subprocess fails (e.g. binary missing, model
// error), Review surfaces the error to the dispatcher. The
// dispatcher converts it to a "❌ /review 失败: ..." reply.
func TestReview_PropagatesRunOnceError(t *testing.T) {
	runOnceErr := errors.New("fake: binary not on PATH")
	fs := &fakeStarter{
		name: "fake",
		mode: ModeJSONIO,
		runOnce: func(_ context.Context, _ StartConfig, _ []ContentBlock) (RunResult, error) {
			return RunResult{}, runOnceErr
		},
	}
	var injected int
	rc := ReviewContext{
		Workspace: "/ws",
		Inject: func(_ context.Context, _ []ContentBlock) error {
			injected++
			return nil
		},
	}
	err := Review(context.Background(), fs, rc)
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
