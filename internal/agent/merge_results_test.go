package agent

import (
	"errors"
	"strings"
	"testing"
)

// TestMergeRunResults_SingleSuccess: trivial case — only one group
// succeeded; merged Text is the single job's Text wrapped in a group
// header.
func TestMergeRunResults_SingleSuccess(t *testing.T) {
	groups := []reviewGroup{{Pattern: "**/*.go", Files: []string{"a.go"}}}
	results := []RunResult{{Text: "go findings"}}
	errs := []error{nil}

	merged, err := mergeRunResults("agent-x", groups, results, errs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(merged.Text, "### Group: pattern **/*.go") {
		t.Errorf("merged text missing group header; got:\n%s", merged.Text)
	}
	if !strings.Contains(merged.Text, "go findings") {
		t.Errorf("merged text missing job's findings; got:\n%s", merged.Text)
	}
}

// TestMergeRunResults_PartialFailure: 1 of 2 jobs failed; the surviving
// job's findings still flow through; failed job shows a failure marker
// (no error returned from mergeRunResults).
func TestMergeRunResults_PartialFailure(t *testing.T) {
	groups := []reviewGroup{
		{Pattern: "**/*.go", Files: []string{"a.go"}},
		{Pattern: "**/*.ts", Files: []string{"b.ts"}},
	}
	results := []RunResult{
		{Text: "go findings"},
		{Text: ""}, // failed job
	}
	errs := []error{nil, errors.New("ts job crashed")}

	merged, err := mergeRunResults("agent-x", groups, results, errs)
	if err != nil {
		t.Errorf("partial failure should NOT surface as merge error; got %v", err)
	}
	if !strings.Contains(merged.Text, "go findings") {
		t.Errorf("surviving group's findings missing from merge; got:\n%s", merged.Text)
	}
	if !strings.Contains(merged.Text, "### Group: pattern **/*.ts — failed") {
		t.Errorf("failed group should have failure marker; got:\n%s", merged.Text)
	}
	if !strings.Contains(merged.Text, "ts job crashed") {
		t.Errorf("failure reason not surfaced in merge marker; got:\n%s", merged.Text)
	}
}

// TestMergeRunResults_AllFailed: every job errored → mergeRunResults
// returns the first error wrapped with the agent name.
func TestMergeRunResults_AllFailed(t *testing.T) {
	groups := []reviewGroup{
		{Pattern: "p1", Files: []string{"a"}},
		{Pattern: "p2", Files: []string{"b"}},
	}
	results := []RunResult{{Text: ""}, {Text: ""}}
	errs := []error{errors.New("first"), errors.New("second")}

	_, err := mergeRunResults("agent-x", groups, results, errs)
	if err == nil {
		t.Fatal("expected error when all jobs failed")
	}
	if !strings.Contains(err.Error(), "agent-x") {
		t.Errorf("returned error missing agent name; got %v", err)
	}
	if !strings.Contains(err.Error(), "first") {
		t.Errorf("returned error should surface first job's error; got %v", err)
	}
}

// TestMergeRunResults_UsageSummed: per-job Usage is summed across
// successful groups (failed groups contribute nothing).
func TestMergeRunResults_UsageSummed(t *testing.T) {
	groups := []reviewGroup{
		{Pattern: "p1", Files: []string{"a"}},
		{Pattern: "p2", Files: []string{"b"}},
		{Pattern: "p3", Files: []string{"c"}},
	}
	results := []RunResult{
		{Text: "x", Usage: &UsageInfo{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 10}},
		{Text: "y", Usage: &UsageInfo{InputTokens: 200, OutputTokens: 100, CacheReadInputTokens: 20}},
		{Text: ""}, // failed
	}
	errs := []error{nil, nil, errors.New("crash")}

	merged, err := mergeRunResults("agent-x", groups, results, errs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.Usage == nil {
		t.Fatal("expected non-nil Usage after merge")
	}
	if got, want := merged.Usage.InputTokens, 300; got != want {
		t.Errorf("InputTokens = %d, want %d", got, want)
	}
	if got, want := merged.Usage.OutputTokens, 150; got != want {
		t.Errorf("OutputTokens = %d, want %d", got, want)
	}
	if got, want := merged.Usage.CacheReadInputTokens, 30; got != want {
		t.Errorf("CacheReadInputTokens = %d, want %d", got, want)
	}
}

// TestMergeRunResults_ShapeMismatch: caller passed mismatched-length
// slices — defensive guard returns an error (not a panic).
func TestMergeRunResults_ShapeMismatch(t *testing.T) {
	groups := []reviewGroup{{Pattern: "p1"}}
	results := []RunResult{{Text: "x"}, {Text: "y"}} // 2 results, 1 group
	errs := []error{nil}

	_, err := mergeRunResults("agent-x", groups, results, errs)
	if err == nil {
		t.Fatal("expected shape-mismatch error")
	}
	if !strings.Contains(err.Error(), "shape mismatch") {
		t.Errorf("error message should mention shape mismatch; got %v", err)
	}
}
