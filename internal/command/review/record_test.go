package review

// The record of a review: what the groups said, the findings that
// have not been delivered yet, and the state of every group.
//
// The record is what a review leaves behind between its rounds: the
// findings of the groups that finished wait there until every group
// is in.

import (
	"errors"
	"strings"
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

// TestRecord_ContributingOutputsKeepTheLatestWord: the group's
// latest output is its current word, and an earlier output that only
// repeats it does not go into the document again.
func TestRecord_ContributingOutputsKeepTheLatestWord(t *testing.T) {
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

	outputs := rec.contributingOutputs("", "**/*.go")
	if len(outputs) != 1 || outputs[0].text != "the same output again" {
		t.Errorf("the contributing outputs are %v, want the latest one alone", outputs)
	}
}

// TestRecord_ContributingOutputsKeepTheUniqueFinding: an earlier
// output that carries a finding the later one does not have stays in
// the document — the agent needs to know what that finding says.
func TestRecord_ContributingOutputsKeepTheUniqueFinding(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{
			{pattern: "**/*.go", text: "the first output", findings: []agent.ReviewFinding{
				{Path: "a.go", Content: "the problem"},
			}},
		},
		nil,
	)
	rec.merge(reviewRun{
		outcomes: []reviewOutcome{{pattern: "**/*.go"}},
		contributions: []reviewContribution{
			{pattern: "**/*.go", text: "the later output", findings: []agent.ReviewFinding{
				{Path: "b.go", Content: "another problem"},
			}},
		},
	})

	outputs := rec.contributingOutputs("", "**/*.go")
	if len(outputs) != 2 {
		t.Fatalf("the contributing outputs are %v, want both", outputs)
	}
	if outputs[0].text != "the first output" || outputs[1].text != "the later output" {
		t.Errorf("the contributing outputs are %v, want them in the order they arrived", outputs)
	}
}

// TestRecord_DeliveredOutputDoesNotComeBack: an output the agent
// already has does not go into the merged document again.
func TestRecord_DeliveredOutputDoesNotComeBack(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{
			{pattern: "**/*.go", text: "the first output", findings: []agent.ReviewFinding{
				{Path: "a.go", Content: "the problem"},
			}},
		},
		nil,
	)
	rec.merge(reviewRun{
		outcomes: []reviewOutcome{{pattern: "**/*.go"}},
		contributions: []reviewContribution{
			{pattern: "**/*.go", text: "the later output", findings: []agent.ReviewFinding{
				{Path: "b.go", Content: "another problem"},
			}},
		},
	})
	// The delivery hands the first output to the agent.
	rec.contributions = markContributions(rec.contributions, []reviewContribution{rec.contributions[0]}, true)

	outputs := rec.contributingOutputs("", "**/*.go")
	if len(outputs) != 1 || outputs[0].text != "the later output" {
		t.Errorf("the contributing outputs are %v, want the undelivered one alone", outputs)
	}
	doc := rec.mergedDocument()
	if strings.Contains(doc, "the first output") {
		t.Errorf("the delivered output is in the document again:\n%s", doc)
	}
}

// TestRecord_AnOlderRoundDoesNotOverwriteTheNewerState: a run that
// started before the round that wrote the states does not overwrite
// them — it only adds what it found.
func TestRecord_AnOlderRoundDoesNotOverwriteTheNewerState(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go", err: errors.New("boom"), sessionID: "sess-go"}},
		nil,
		nil,
	)
	rec.generation = 2
	rec.merge(reviewRun{
		id:       1,
		outcomes: []reviewOutcome{{pattern: "**/*.go", err: errors.New("boom"), sessionID: "sess-go"}},
	})

	if len(rec.outcomes) != 1 {
		t.Fatalf("the record holds %d outcomes, want 1", len(rec.outcomes))
	}
	if rec.outcomes[0].err == nil {
		t.Error("the older round's state replaced the newer one")
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
