package agent

import (
	"errors"
	"strings"
	"testing"
)

// TestMergeRunResults_SingleSuccess: trivial case — only one group
// succeeded; merged Text is the single job's Text wrapped in a group
// header + a top-line "Reviewed N groups." summary.
func TestMergeRunResults_SingleSuccess(t *testing.T) {
	groups := []reviewGroup{{Pattern: "**/*.go", Files: []string{"a.go"}}}
	results := []RunResult{{Text: "go findings"}}
	errs := []error{nil}

	merged, err := mergeRunResults("agent-x", groups, results, errs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Top-line summary
	if !strings.HasPrefix(merged.Text, "Reviewed 1 groups.") {
		t.Errorf("merged text missing 'Reviewed 1 groups.' summary; got:\n%s", merged.Text)
	}
	// Per-group header
	if !strings.Contains(merged.Text, "### Group: pattern **/*.go") {
		t.Errorf("merged text missing group header; got:\n%s", merged.Text)
	}
	// Original text preserved verbatim
	if !strings.Contains(merged.Text, "go findings") {
		t.Errorf("merged text missing job's findings; got:\n%s", merged.Text)
	}
}

// TestMergeRunResults_PartialFailure: 1 of 2 jobs failed; the
// surviving job's findings still flow through; failed job shows a
// failure marker (no error returned from mergeRunResults).
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

// TestMergeRunResults_UsageNotAggregated: per v12 simplification,
// mergeRunResults does NOT aggregate Usage — each job's usage stays
// in its own RunResult, and the merged RunResult.Usage is left nil.
// Aggregation was removed because the consumer (main agent on the
// /review dispatcher path) doesn't read the merged Usage — only the
// outer synthetic Result is consumed, and it carries no Text.
func TestMergeRunResults_UsageNotAggregated(t *testing.T) {
	groups := []reviewGroup{
		{Pattern: "p1", Files: []string{"a"}},
		{Pattern: "p2", Files: []string{"b"}},
	}
	results := []RunResult{
		{Text: "x", Usage: &UsageInfo{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 10}},
		{Text: "y", Usage: &UsageInfo{InputTokens: 200, OutputTokens: 100, CacheReadInputTokens: 20}},
	}
	errs := []error{nil, nil}

	merged, err := mergeRunResults("agent-x", groups, results, errs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.Usage != nil {
		t.Errorf("v12: mergeRunResults should NOT aggregate Usage; got %+v", merged.Usage)
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

// TestMergeRunResults_NoSortNoDedupNoCoverage: explicit assertion
// that v12 merge is purely textual concat — no severity sort, no
// path dedup, no coverage aggregation. Each group's findings are
// preserved verbatim.
func TestMergeRunResults_NoSortNoDedupNoCoverage(t *testing.T) {
	groups := []reviewGroup{
		{Pattern: "p1", Files: []string{"a"}},
		{Pattern: "p2", Files: []string{"b"}},
	}
	// Intentionally give the "wrong" order / content to verify
	// merge doesn't re-order / parse / re-format.
	results := []RunResult{
		{Text: "blocker: low-severity issue from group-1"},
		{Text: "critical: high-severity issue from group-2"},
	}
	errs := []error{nil, nil}

	merged, err := mergeRunResults("agent-x", groups, results, errs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Order must match input order (group-1 BEFORE group-2), NOT
	// severity-sorted.
	g1Idx := strings.Index(merged.Text, "low-severity issue from group-1")
	g2Idx := strings.Index(merged.Text, "high-severity issue from group-2")
	if g1Idx == -1 || g2Idx == -1 {
		t.Fatalf("expected both findings in merged text; got:\n%s", merged.Text)
	}
	if g1Idx >= g2Idx {
		t.Errorf("expected input-order (group-1 BEFORE group-2), NOT severity-sort; merged: %s", merged.Text)
	}
}

// TestMergeRunResults_PropagatesAuditMetadata: the merged RunResult
// carries forward the FIRST successful job's SessionID / Model /
// DurationMs / Subtype so callers (e.g. /review audit logging) can
// tell which sessionId produced the merged review. Usage stays nil
// (v12 — see TestMergeRunResults_UsageNotAggregated). Failures
// before the first successful job are skipped.
func TestMergeRunResults_PropagatesAuditMetadata(t *testing.T) {
	groups := []reviewGroup{
		{Pattern: "p1", Files: []string{"a"}},
		{Pattern: "p2", Files: []string{"b"}},
		{Pattern: "p3", Files: []string{"c"}},
	}
	results := []RunResult{
		{Text: ""}, // failed job — skipped
		{Text: "y", SessionID: "session-2", Model: "minimax", DurationMs: 1234, Subtype: "completed"},
		{Text: "z", SessionID: "session-3", Model: "minimax", DurationMs: 9999, Subtype: "completed"},
	}
	errs := []error{
		errors.New("first job crashed"),
		nil,
	 nil,
	}

	merged, err := mergeRunResults("agent-x", groups, results, errs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.SessionID != "session-2" {
		t.Errorf("merged.SessionID = %q; want first successful %q", merged.SessionID, "session-2")
	}
	if merged.Model != "minimax" {
		t.Errorf("merged.Model = %q; want first successful %q", merged.Model, "minimax")
	}
	if merged.DurationMs != 1234 {
		t.Errorf("merged.DurationMs = %d; want first successful 1234", merged.DurationMs)
	}
	if merged.Subtype != "completed" {
		t.Errorf("merged.Subtype = %q; want first successful %q", merged.Subtype, "completed")
	}
	if merged.Usage != nil {
		t.Errorf("merged.Usage must remain nil (v12); got %+v", merged.Usage)
	}
}

// TestMergeRunResults_AuditMetadataLeftEmptyWhenAllFailed: when all
// jobs failed, mergeRunResults returns an error — no audit metadata
// propagation is exercised on that path.
func TestMergeRunResults_AuditMetadataLeftEmptyWhenAllFailed(t *testing.T) {
	groups := []reviewGroup{
		{Pattern: "p1", Files: []string{"a"}},
		{Pattern: "p2", Files: []string{"b"}},
	}
	results := []RunResult{
		{Text: "", SessionID: "session-1"},
		{Text: "", SessionID: "session-2"},
	}
	errs := []error{errors.New("first"), errors.New("second")}

	_, err := mergeRunResults("agent-x", groups, results, errs)
	if err == nil {
		t.Fatal("expected error when all jobs failed")
	}
}
