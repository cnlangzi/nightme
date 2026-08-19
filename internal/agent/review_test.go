package agent

import (
	"context"
	"errors"
	"fmt"
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
func (f *fakeStarter) Review(ctx context.Context, cfg StartConfig) (RunResult, error) {
	result, err := f.RunOnce(ctx, cfg, []ContentBlock{{
		Type: ContentText,
		Text: StandardPrompt(),
	}})
	if err != nil {
		return RunResult{}, fmt.Errorf("agent %s: review one-shot failed: %w",
			f.Info().Name, err)
	}
	return result, nil
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

	// Required guardrails (the 7 "How to review" rules — last one
	// added in the simplification pass to keep "## Suggestions"
	// from being empty).
	wantRules := []string{
		"Read the diff first",
		"Distinguish BLOCKERS from nits",
		"Cite file:line",
		"Skip linter / typechecker",
		"Skip pre-existing issues",
		"False-positive filter",
		"Look for simplification opportunities",
	}
	for _, r := range wantRules {
		if !strings.Contains(p, r) {
			t.Errorf("StandardPrompt() missing rule %q", r)
		}
	}

	// Required "What to look for" categories — must include
	// Efficiency (added to align with claude /code-review's
	// "efficiency cleanups" pillar) and Simplification (which
	// also covers claude's "reuse" pillar — prefer existing
	// helpers over new code).
	wantCategories := []string{
		"Correctness",
		"Resource lifetime",
		"Concurrency",
		"Error handling",
		"API surface",
		"Security",
		"Migration risk",
		"Efficiency",
		"Test gaps",
		"Simplification",
	}
	for _, c := range wantCategories {
		if !strings.Contains(p, "**"+c+"**") {
			t.Errorf("StandardPrompt() missing category %q", c)
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

// TestReview_RejectsNilInject: removed in v9. The bridge no longer
// holds an Inject callback (the /review dispatcher owns Inject
// after the bridge returns). See TestReview_ReturnsRunResult for
// the new contract. Kept as a stub so anyone grepping for the
// old test name finds this migration note.
func TestReview_RejectsNilInject(t *testing.T) {
	t.Skip("Inject was removed from StartConfig in v9; see TestReview_ReturnsRunResult for the new contract")
}

// TestReview_ReturnsRunResult verifies the happy path: Review
// calls s.RunOnce with StandardPrompt, captures the returned
// RunResult, and returns it (RAW — no FormatReviewMessage wrap,
// no Inject). The /review dispatcher is responsible for wrapping
// and routing to AS + channel.
//
// Uses a fakeStarter that records the prompt it received and
// returns a canned RunResult.Text — no real subprocess is spawned.
func TestReview_ReturnsRunResult(t *testing.T) {
	const reviewText = "## Summary\nThe diff is fine."

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

	rc := StartConfig{Workspace: "/ws"}

	result, err := fs.Review(context.Background(), rc)
	if err != nil {
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

	// Verify the returned RunResult.Text is the RAW review body,
	// not wrapped in FormatReviewMessage (that's the caller's job).
	if result.Text != reviewText {
		t.Errorf("returned Text mismatch:\n got  %q\n want %q", result.Text, reviewText)
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
	rc := StartConfig{Workspace: "/ws"}

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
