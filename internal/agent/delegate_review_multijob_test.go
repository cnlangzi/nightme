package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// This file covers the v12 multi-job path that aggregate_sink_test
// (the event aggregator) and merge_results_test (the findings merge)
// do NOT cover: the per-group PROMPT assembly + the concurrent
// ORCHESTRATION in delegateReviewMultiJob (one RunOnce per ocr rule
// group, sem-capped, merged).
//
// mergeRunResults already has 6 tests in merge_results_test.go — we
// do not duplicate them here. The eventAggregator has 10 tests in
// aggregate_sink_test.go. The gap this file closes is:
//   - assembleGroupPrompt (per-group prompt: this group's files +
//     rule, NOT other groups' files — smart-bundling isolation)
//   - delegateReviewMultiJob (concurrent fan-out + merge wiring,
//     using a fakeStarter; no event sink → isolates orchestration
//     from the aggregator's own tested event semantics)

func twoGroups() []reviewGroup {
	return []reviewGroup{
		{Pattern: "**/*.go", Files: []string{"a.go", "b.go"}, Rule: "Go rule text"},
		{Pattern: "**/*.ts", Files: []string{"c.ts"}, Rule: "TS rule text"},
	}
}

func okResult(text string) RunResult { return RunResult{Text: text} }

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }

// TestAssembleGroupPrompt_Structure: a per-group prompt lists THIS
// group's files + rule, NOT other groups' files, and carries the
// coverage_rate + how-to + ocr-rules sections. Uses a non-existent
// workspace so groupFilteredDiff's runGit fails → diff sections
// omitted, but the structural sections (Context / Files / Rules /
// How-to / Output format) are still present and testable.
func TestAssembleGroupPrompt_Structure(t *testing.T) {
	rc := reviewContext{
		workspace:     "/nonexistent-repo",
		defaultBranch: "origin/main",
		mergeBase:     "abc123",
	}
	goGroup := &reviewGroup{Pattern: "**/*.go", Files: []string{"a.go", "b.go"}, Rule: "Go rule text"}
	tsFile := "c.ts" // belongs to the OTHER group — must NOT appear

	prompt := assembleGroupPrompt(context.Background(), rc, goGroup)
	if prompt == "" {
		t.Fatal("assembleGroupPrompt returned empty for valid group")
	}
	// This group's files + rule present.
	if !strings.Contains(prompt, "a.go") || !strings.Contains(prompt, "b.go") {
		t.Errorf("group prompt missing this group's files; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Go rule text") {
		t.Errorf("group prompt missing this group's rule; got:\n%s", prompt)
	}
	// Other group's file MUST NOT leak in (smart-bundling isolation).
	if strings.Contains(prompt, tsFile) {
		t.Errorf("group prompt leaked another group's file %q; got:\n%s", tsFile, prompt)
	}
	// Structural sections.
	for _, want := range []string{
		"# Context", "# Files to review", "Coverage is MANDATORY",
		"# Review rules", "# How to review", "# Output format",
		"coverage_rate", "rule group: pattern **/*.go",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("group prompt missing %q; got:\n%s", want, prompt)
		}
	}
}

// TestAssembleGroupPrompt_EmptyWorkspace: "" workspace or nil group
// → "" (DelegateReview's signal to fall back to StandardPrompt).
func TestAssembleGroupPrompt_EmptyWorkspace(t *testing.T) {
	g := &reviewGroup{Pattern: "**/*.go", Files: []string{"a.go"}, Rule: "r"}
	if got := assembleGroupPrompt(context.Background(), reviewContext{}, g); got != "" {
		t.Errorf("empty workspace should return \"\"; got %q", got)
	}
	if got := assembleGroupPrompt(context.Background(), reviewContext{workspace: "/x"}, nil); got != "" {
		t.Errorf("nil group should return \"\"; got %q", got)
	}
}

// TestDelegateReviewMultiJob_ConcurrentMerge: with 2 ocr rule groups,
// delegateReviewMultiJob runs one RunOnce per group (concurrent,
// sem-capped) and merges. Uses fakeStarter whose runOnce extracts the
// group pattern from the assembled prompt and returns per-group
// findings — verifying (a) each group got its own prompt, (b) the
// merge concatenated per-group results, (c) no group was dropped.
//
// No event sink is passed (opts empty) → aggregator is nil; this
// isolates the ORCHESTRATION + MERGE path. The aggregator's own
// event semantics have their 10-test suite (aggregate_sink_test).
func TestDelegateReviewMultiJob_ConcurrentMerge(t *testing.T) {
	groups := twoGroups()
	pre := reviewContext{
		workspace:     "/nonexistent-repo",
		defaultBranch: "origin/main",
		mergeBase:     "abc123",
		ocrGroups:     groups,
	}

	var callCount int32
	var mu sync.Mutex
	fs := &fakeStarter{
		name: "test-agent",
		runOnce: func(ctx context.Context, cfg StartConfig, blocks []ContentBlock, opts ...RunOnceOption) (RunResult, error) {
			atomic.AddInt32(&callCount, 1)
			mu.Lock()
			defer mu.Unlock()
			prompt := ""
			if len(blocks) > 0 {
				prompt = blocks[0].Text
			}
			// Each group's prompt carries its own pattern; echo it
			// back so the merge is distinguishable per group.
			for _, g := range groups {
				if strings.Contains(prompt, g.Pattern) {
					return okResult("findings[" + g.Pattern + "]"), nil
				}
			}
			return okResult("findings[unknown]"), nil
		},
	}

	merged, err := delegateReviewMultiJob(context.Background(), fs, StartConfig{Workspace: pre.workspace}, pre)
	if err != nil {
		t.Fatalf("delegateReviewMultiJob: %v", err)
	}
	if callCount != int32(len(groups)) {
		t.Errorf("RunOnce called %d times, want %d (one per group)", callCount, len(groups))
	}
	if !strings.Contains(merged.Text, "Reviewed 2 groups") {
		t.Errorf("merged missing header; got:\n%s", merged.Text)
	}
	for _, g := range groups {
		want := "findings[" + g.Pattern + "]"
		if !strings.Contains(merged.Text, want) {
			t.Errorf("merged missing %q for group %s; got:\n%s", want, g.Pattern, merged.Text)
		}
	}
}

// TestDelegateReviewMultiJob_PartialFailure: one group's RunOnce
// errors → that group is marked failed in the merge, the survivor's
// findings still flow. Verifies per-job error isolation (one bad job
// doesn't poison the whole review).
func TestDelegateReviewMultiJob_PartialFailure(t *testing.T) {
	groups := twoGroups()
	pre := reviewContext{
		workspace: "/nonexistent-repo",
		ocrGroups: groups,
	}
	fs := &fakeStarter{
		name: "test-agent",
		runOnce: func(ctx context.Context, cfg StartConfig, blocks []ContentBlock, opts ...RunOnceOption) (RunResult, error) {
			prompt := ""
			if len(blocks) > 0 {
				prompt = blocks[0].Text
			}
			// The TS group fails; the Go group succeeds.
			if strings.Contains(prompt, "**/*.ts") {
				return RunResult{}, errBoom
			}
			return okResult("go findings"), nil
		},
	}

	merged, err := delegateReviewMultiJob(context.Background(), fs, StartConfig{Workspace: pre.workspace}, pre)
	if err != nil {
		t.Fatalf("partial failure should NOT error (survivors); got: %v", err)
	}
	if !strings.Contains(merged.Text, "go findings") {
		t.Errorf("surviving group lost; got:\n%s", merged.Text)
	}
	if !strings.Contains(merged.Text, "failed") || !strings.Contains(merged.Text, "**/*.ts") {
		t.Errorf("failed group not marked; got:\n%s", merged.Text)
	}
}
