package agent

// The merged review document: one section per group, the group's own
// output under its header.
//
// The format lives in one place — the fan-out builds a run's document
// with it, and the command layer rebuilds the same document from the
// findings it has cached.

import (
	"errors"
	"strings"
	"testing"
)

// TestMergeReviewDocument_CarriesEveryGroup: the document opens with
// the number of groups it holds, and every group gets a section: the
// ones that finished carry their own output, the one that failed
// carries the error that ended it.
func TestMergeReviewDocument_CarriesEveryGroup(t *testing.T) {
	got := MergeReviewDocument([]ReviewGroupSection{
		{Pattern: "**/*.go", Files: []string{"a.go", "b.go"}, Texts: []string{"the go findings"}},
		{Pattern: "**/*.ts", Files: []string{"c.ts"}, Err: errors.New("ts job crashed")},
	})

	want := "Reviewed 2 groups.\n\n" +
		"### Group: pattern **/*.go — files: a.go, b.go\n\n" +
		"the go findings\n\n" +
		"### Group: pattern **/*.ts — failed: ts job crashed"
	if got != want {
		t.Errorf("the document is:\n%s\nwant:\n%s", got, want)
	}
}

// TestMergeReviewDocument_CarriesEveryOutput: a group that ran more
// than once carries its outputs one after the other, under one header.
func TestMergeReviewDocument_CarriesEveryOutput(t *testing.T) {
	got := MergeReviewDocument([]ReviewGroupSection{
		{Pattern: "**/*.go", Files: []string{"a.go"}, Texts: []string{"the first output", "the second output"}},
	})

	want := "Reviewed 1 groups.\n\n" +
		"### Group: pattern **/*.go — files: a.go\n\n" +
		"the first output\n\n" +
		"the second output"
	if got != want {
		t.Errorf("the document is:\n%s\nwant:\n%s", got, want)
	}
}

// TestMergeRunResults_UsesTheSharedDocument: the fan-out's merged text
// is the shared document — one format serves both the run and the
// command layer that rebuilds it.
func TestMergeRunResults_UsesTheSharedDocument(t *testing.T) {
	groups := []reviewGroup{
		{Pattern: "**/*.go", Files: []string{"a.go"}},
		{Pattern: "**/*.ts", Files: []string{"c.ts"}},
	}
	results := []RunResult{{Text: "the go findings"}, {Text: ""}}
	errs := []error{nil, errors.New("boom")}

	merged, err := mergeRunResults("agent-x", groups, results, errs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := MergeReviewDocument([]ReviewGroupSection{
		{Pattern: "**/*.go", Files: []string{"a.go"}, Texts: []string{"the go findings"}},
		{Pattern: "**/*.ts", Files: []string{"c.ts"}, Err: errors.New("boom")},
	})
	if merged.Text != want {
		t.Errorf("the merged text is:\n%s\nwant:\n%s", merged.Text, want)
	}
	if !strings.Contains(merged.Text, "### Group: pattern **/*.go — files: a.go") {
		t.Errorf("the merged text does not carry the group header:\n%s", merged.Text)
	}
}
