package review

// The record of a review: what the groups said, the findings that
// have not been delivered yet, and the state of every group.
//
// The record is what a review leaves behind between its rounds: the
// findings of the groups that finished wait there until every group
// is in.

import (
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// testRecord builds a review record the way a finished run leaves it:
// the state of every group, what the groups that finished said, and
// the plan that named them.
func testRecord(outcomes []reviewOutcome, contributions []reviewContribution,
	plan []agent.ReviewPlanGroup,
) reviewRecord {
	var rec reviewRecord
	rec.merge(reviewRun{outcomes: outcomes, contributions: contributions, plan: plan})
	return rec
}

// testContribution is what a group that finished says: its output and
// the findings it found.
func testContribution(pattern string, findings ...agent.ReviewFinding) reviewContribution {
	return reviewContribution{
		pattern:  pattern,
		text:     "the findings of " + pattern,
		findings: findings,
		parsed:   true,
	}
}

// TestRecord_MergeKeepsTheFindingsOfEveryRound: the record
// accumulates — the findings of the second round join the ones of the
// first, and both rounds' contributions stay.
func TestRecord_MergeKeepsTheFindingsOfEveryRound(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{
			testContribution("**/*.go", agent.ReviewFinding{Path: "a.go", Content: "the first"}),
		},
		nil,
	)
	rec.merge(reviewRun{
		outcomes: []reviewOutcome{{pattern: "**/*.ts"}},
		contributions: []reviewContribution{
			testContribution("**/*.ts", agent.ReviewFinding{Path: "b.ts", Content: "the second"}),
		},
	})

	if len(rec.findings) != 2 {
		t.Fatalf("the record holds %d findings, want 2", len(rec.findings))
	}
	if len(rec.contributions) != 2 {
		t.Errorf("the record holds %d contributions, want 2", len(rec.contributions))
	}
	if rec.delivered {
		t.Error("the record says its findings were delivered")
	}
}

// TestRecord_MergeDropsTheRepeatedFinding: the same group reporting
// the same finding twice keeps one — the finding's identity is the
// group, the path and the content.
//
// Two groups reporting the same path and content are two findings:
// each group's word is its own.
func TestRecord_MergeDropsTheRepeatedFinding(t *testing.T) {
	finding := agent.ReviewFinding{Path: "a.go", Line: "42", Severity: agent.SeverityHigh, Content: "the problem"}
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{testContribution("**/*.go", finding)},
		nil,
	)
	rec.merge(reviewRun{
		outcomes: []reviewOutcome{{pattern: "**/*.go"}},
		contributions: []reviewContribution{
			testContribution("**/*.go", agent.ReviewFinding{
				Path: "a.go", Line: "99", Severity: agent.SeverityLow, Content: "the problem",
			}),
		},
	})
	if len(rec.findings) != 1 {
		t.Fatalf("the record holds %d findings, want 1", len(rec.findings))
	}
	if rec.findings[0].finding.Line != "42" {
		t.Errorf("the finding's line = %q, want the first one (%q)", rec.findings[0].finding.Line, "42")
	}
	if rec.findings[0].finding.Severity != agent.SeverityHigh {
		t.Errorf("the finding's severity = %q, want the first one (%q)",
			rec.findings[0].finding.Severity, agent.SeverityHigh)
	}

	other := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{testContribution("**/*.go", finding)},
		nil,
	)
	other.merge(reviewRun{
		outcomes: []reviewOutcome{{pattern: "**/*.ts"}},
		contributions: []reviewContribution{
			testContribution("**/*.ts", agent.ReviewFinding{Path: "a.go", Content: "the problem"}),
		},
	})
	if len(other.findings) != 2 {
		t.Errorf("the record holds %d findings, want 2 — two groups, two findings", len(other.findings))
	}
}

// TestRecord_PendingUntilNoGroupCanBePickedUp: the review is pending
// while a failed group can still be picked up by a continuation.
func TestRecord_PendingUntilNoGroupCanBePickedUp(t *testing.T) {
	cases := []struct {
		name    string
		outcome reviewOutcome
		pending bool
	}{
		{"the group reached the agent", reviewOutcome{pattern: "**/*.go", sessionID: "sess-go", err: errors.New("boom")}, true},
		{"the group kept its prompt", reviewOutcome{pattern: "**/*.go", prompt: "the prompt", err: errors.New("boom")}, true},
		{"the group's recovery attempt is spent", reviewOutcome{pattern: "**/*.go", sessionID: "sess-go", spent: true, err: errors.New("boom")}, false},
		{"the group reached neither the agent nor a prompt", reviewOutcome{pattern: "**/*.go", err: errors.New("boom")}, false},
		{"the group finished", reviewOutcome{pattern: "**/*.go"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := testRecord([]reviewOutcome{tc.outcome}, nil, nil)
			if got := rec.pending(); got != tc.pending {
				t.Errorf("pending() = %v, want %v", got, tc.pending)
			}
		})
	}
}

// TestRecord_MergedDocumentRebuildsTheReview: the merged document is
// rebuilt from the record — the groups that finished carry their own
// output, the group that failed carries the error that ended it.
func TestRecord_MergedDocumentRebuildsTheReview(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{
			{pattern: "**/*.go", files: []string{"a.go", "b.go"}},
			{pattern: "**/*.ts", files: []string{"c.ts"}, err: errors.New("boom")},
		},
		[]reviewContribution{
			{pattern: "**/*.go", files: []string{"a.go", "b.go"}, text: "the go findings"},
		},
		[]agent.ReviewPlanGroup{
			{Pattern: "**/*.go"},
			{Pattern: "**/*.ts"},
		},
	)

	want := "Reviewed 2 groups.\n\n" +
		"### Group: pattern **/*.go — files: a.go, b.go\n\n" +
		"the go findings\n\n" +
		"### Group: pattern **/*.ts — failed: boom"
	if got := rec.mergedDocument(); got != want {
		t.Errorf("the merged document is:\\n%s\\nwant:\\n%s", got, want)
	}
}

// TestRecord_ContributingTextsSkipsTheRepeatedOutput: an output that
// only repeats what the agent already heard does not go into the
// document again.
func TestRecord_ContributingTextsSkipsTheRepeatedOutput(t *testing.T) {
	finding := agent.ReviewFinding{Path: "a.go", Content: "the problem"}
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{
			{pattern: "**/*.go", text: "the first output", findings: []agent.ReviewFinding{finding}},
		},
		nil,
	)
	rec.merge(reviewRun{
		outcomes: []reviewOutcome{{pattern: "**/*.go"}},
		contributions: []reviewContribution{
			{pattern: "**/*.go", text: "the same output again", findings: []agent.ReviewFinding{finding}},
		},
	})

	texts := rec.contributingTexts("", "**/*.go")
	if len(texts) != 1 || texts[0] != "the first output" {
		t.Errorf("the contributing outputs are %v, want the first one alone", texts)
	}
}

// TestRecord_MergeReplacesTheGroupState: the newer word about a group
// wins — a group that ran again and succeeded is a group that
// succeeded.
func TestRecord_MergeReplacesTheGroupState(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go", sessionID: "sess-go", err: errors.New("boom")}},
		nil,
		nil,
	)
	rec.merge(reviewRun{
		outcomes: []reviewOutcome{{pattern: "**/*.go", sessionID: "sess-go"}},
		plan:     []agent.ReviewPlanGroup{{Pattern: "**/*.go"}},
	})

	if len(rec.outcomes) != 1 {
		t.Fatalf("the record holds %d outcomes, want 1", len(rec.outcomes))
	}
	if rec.outcomes[0].err != nil {
		t.Errorf("the group still carries the failure of the round before: %+v", rec.outcomes[0])
	}
	if rec.pending() {
		t.Error("the review is pending although every group finished")
	}
}
