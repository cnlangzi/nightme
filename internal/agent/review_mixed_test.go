// review_mixed_test.go — tests for ReviewWithMixed and its supporting
// helpers (nativeReviewGroup, patternNativeReview case in
// assembleGroupPrompt).
//
// ReviewWithMixed is the cursor bridge's review path: native slash
// command goroutine(s) + nightme-owned simplify lens goroutine,
// merged via the existing delegateReviewMultiJob fan-out machinery.
// These tests pin that contract at the agent-package level so the
// cursor bridge's Starter.Review can stay a one-liner.
//
// What we lock here:
//
//   - nativeReviewGroup(slash) returns {Pattern: patternNativeReview,
//     Rule: slash} — the Rule is the slash command itself (with
//     leading slash), so the bridge's RunOnce spawns the binary with
//     the slash as the prompt and the binary dispatches it.
//   - assembleGroupPrompt(pre, nativeReviewGroup) returns g.Rule
//     verbatim — no workspace / diff / file wrapping — even when
//     rc.workspace is empty (the native group doesn't depend on
//     precomputed context).
//   - ReviewWithMixed(empty workspace) returns ErrNoDiff without
//     spawning anything — same contract as ReviewWithPrompt.
//   - ReviewWithMixed(non-empty) spawns len(slashCommands)+1
//     RunOnce goroutines (one per native + one for simplify) and
//     the merged result carries per-group findings.
package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/proc"
)

// TestNativeReviewGroup_Shape — the Rule field MUST carry the slash
// command verbatim (no transformation, no trimming). The bridge's
// RunOnce passes blocks[0].Text to the binary's -p slot; if the
// slash got mangled (e.g. lowercased, leading-slash stripped), the
// binary would receive a literal prompt instead of dispatching the
// slash command — silent regression.
func TestNativeReviewGroup_Shape(t *testing.T) {
	g := nativeReviewGroup("/review-bugbot")
	if g.Pattern != patternNativeReview {
		t.Errorf("Pattern = %q, want %q", g.Pattern, patternNativeReview)
	}
	if g.Rule != "/review-bugbot" {
		t.Errorf("Rule = %q, want %q (slash command MUST round-trip verbatim)",
			g.Rule, "/review-bugbot")
	}
	if len(g.Files) != 0 {
		t.Errorf("Files = %v, want nil (native group has no precomputed file list)", g.Files)
	}
}

// TestAssembleGroupPrompt_NativeReview_Verbatim — the native group
// is a special case in assembleGroupPrompt: no diff / no files /
// no rule wrapping — just the slash command. Without this
// short-circuit, the binary would receive a full review prompt
// with "/review-bugbot" embedded as the rule text, and Bugbot
// would NOT run.
//
// We also assert the short-circuit fires when rc.workspace == ""
// (the native group's spawn target is the binary's slash system,
// not our precomputed reviewContext — so workspace emptiness
// doesn't matter for native groups).
func TestAssembleGroupPrompt_NativeReview_Verbatim(t *testing.T) {
	t.Run("with workspace", func(t *testing.T) {
		g := nativeReviewGroup("/review-bugbot")
		rc := reviewContext{
			workspace:     "/some/repo",
			defaultBranch: "origin/main",
			mergeBase:     "deadbeef",
			reviewable:    []string{"foo.go", "bar.go"},
		}
		got := assembleGroupPrompt(context.Background(), rc, &g)
		if got != "/review-bugbot" {
			t.Errorf("got = %q, want %q (native group MUST return slash verbatim, no wrapping)",
				got, "/review-bugbot")
		}
	})
	t.Run("with empty workspace", func(t *testing.T) {
		g := nativeReviewGroup("/review-bugbot")
		rc := reviewContext{workspace: ""}
		got := assembleGroupPrompt(context.Background(), rc, &g)
		if got != "/review-bugbot" {
			t.Errorf("got = %q, want %q (native group MUST bypass workspace emptiness)",
				got, "/review-bugbot")
		}
	})
}

// TestReviewWithMixed_EmptyDiff_ReturnsErrNoDiff — same contract as
// ReviewWithPrompt: when precompute produces no reviewable files and
// no untracked, return ErrNoDiff WITHOUT spawning any RunOnce.
// Burning a cursor-agent process on an empty review is wasted work.
func TestReviewWithMixed_EmptyDiff_ReturnsErrNoDiff(t *testing.T) {
	var spawnCount int32
	fs := &fakeStarter{
		name: "empty-diff-test",
		runOnce: func(ctx context.Context, cfg StartConfig, blocks []ContentBlock, opts ...RunOnceOption) (RunResult, error) {
			atomic.AddInt32(&spawnCount, 1)
			return okResult("should not see this"), nil
		},
	}
	// Empty workspace → empty reviewContext → isEmptyDiff() true.
	_, err := ReviewWithMixed(context.Background(), fs,
		StartConfig{Workspace: ""},
		[]string{"/review-bugbot"})
	if !errors.Is(err, ErrNoDiff) {
		t.Fatalf("err = %v, want ErrNoDiff", err)
	}
	if spawnCount != 0 {
		t.Errorf("RunOnce called %d times, want 0 (empty diff MUST short-circuit)",
			spawnCount)
	}
}

// TestReviewWithMixed_FanOut_MergesResults — the full happy path:
//
//   - 1 slash command → 1 native goroutine + 1 simplify goroutine
//     = 2 total RunOnce calls
//   - Each goroutine receives its own prompt (native = verbatim slash,
//     simplify = full simplifyPrompt with file list wrapping)
//   - mergeRunResults concatenates per-group findings with the
//     standard "Reviewed N groups.\n\n" header
//
// Uses a fakeStarter whose runOnce extracts the prompt and returns
// per-group findings — verifies both goroutines spawned and the
// merged output carries both findings. No event sink → no aggregator;
// the orchestration + merge path is isolated from the aggregator's
// own event semantics (which have their own 10-test suite).
//
// precomputeReviewWithBuiltin needs a real git repo (runs `git diff`
// etc. to populate reviewable). We set up a minimal one-shot repo
// in a t.TempDir(): one committed file + one unstaged edit gives
// the precompute something to enumerate so isEmptyDiff() is false.
func TestReviewWithMixed_FanOut_MergesResults(t *testing.T) {
	ws := setupGitRepoForReview(t)

	const slashCmd = "/review-bugbot"
	var callCount int32
	var mu sync.Mutex
	fs := &fakeStarter{
		name: "test-cursor",
		runOnce: func(ctx context.Context, cfg StartConfig, blocks []ContentBlock, opts ...RunOnceOption) (RunResult, error) {
			atomic.AddInt32(&callCount, 1)
			mu.Lock()
			defer mu.Unlock()
			prompt := ""
			if len(blocks) > 0 {
				prompt = blocks[0].Text
			}
			// Native goroutine: prompt == slash verbatim.
			if prompt == slashCmd {
				return okResult("bugbot findings: no blockers"), nil
			}
			// Simplify goroutine: prompt is the full assembleGroupPrompt
			// output (file list + simplifyPrompt). It carries the
			// "Simplify review lens" header from assembleGroupPrompt.
			if strings.Contains(prompt, "# Simplify review lens") {
				return okResult("simplify findings: dead code in foo.go"), nil
			}
			return okResult("findings: unknown group"), nil
		},
	}

	merged, err := ReviewWithMixed(context.Background(), fs,
		StartConfig{Workspace: ws},
		[]string{slashCmd})
	if err != nil {
		t.Fatalf("ReviewWithMixed: %v", err)
	}

	// 2 goroutines total (1 native + 1 simplify).
	if got := atomic.LoadInt32(&callCount); got != 2 {
		t.Errorf("RunOnce called %d times, want 2 (1 native + 1 simplify)", got)
	}

	// Merged text carries the standard header + both groups' findings.
	for _, want := range []string{
		"Reviewed 2 groups",
		"bugbot findings: no blockers",
		"simplify findings: dead code in foo.go",
		patternNativeReview, // group header from mergeRunResults
		patternSimplify,
	} {
		if !strings.Contains(merged.Text, want) {
			t.Errorf("merged.Text missing %q; got:\n%s", want, merged.Text)
		}
	}
}

// setupGitRepoForReview creates a minimal git repo in t.TempDir() with:
//
//   - 1 committed file (`committed.go`)        → enters reviewable via
//     git diff <base>...HEAD
//   - 1 unstaged edit on the same file         → keeps it in reviewable
//     via git diff (unstaged)
//
// precomputeReviewWithBuiltin needs `git diff` to return non-empty
// content for isEmptyDiff() to be false (otherwise ReviewWithMixed
// short-circuits with ErrNoDiff before spawning anything).
//
// Reuses gitInit / gitCommitEmpty from review_test.go (same package).
func setupGitRepoForReview(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitInit(t, dir)

	// Committed file.
	if err := os.WriteFile(filepath.Join(dir, "committed.go"),
		[]byte("package x\n\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatalf("write committed.go: %v", err)
	}
	gitCommitEmpty(t, dir) // needed so subsequent HEAD exists; we'll add real commit next

	// Actually commit the file so `git diff <base>...HEAD` sees it.
	c := proc.New(t.Context(), "git",
		"-C", dir, "add", "committed.go")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	c = proc.New(t.Context(), "git",
		"-C", dir, "commit", "-q", "-m", "add committed.go")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	// Unstaged edit so `git diff` (no flag) returns content.
	if err := os.WriteFile(filepath.Join(dir, "committed.go"),
		[]byte("package x\n\n// added comment\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatalf("write committed.go (unstaged): %v", err)
	}
	return dir
}