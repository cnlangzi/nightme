package review

// What /review shows the chat: the plan line before the run, the
// coalesced stream of the groups' findings, and the verdict line at
// the end. The merged body is the main agent's input — it never
// reaches the channel.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestRenderPlanLine: the plan names the runner, the rule set, the
// subagent count and every group with its file count — and nothing
// else.
func TestRenderPlanLine(t *testing.T) {
	got := renderPlanLine("codex", []agent.ReviewPlanGroup{
		{Pattern: "**/*.go", Label: "system / **/*.go · Go", Files: []string{"a.go", "b.go"}},
		{Pattern: agent.PatternSimplify, Label: "Simplify review lens · Go", Files: []string{"a.go"}},
	})

	for _, want := range []string{
		"codex",
		"open-code-review",
		"2 subagents",
		"1. `system / **/*.go · Go` — 2 files",
		"2. Simplify review lens · Go — 1 file",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the plan line does not carry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "findings") {
		t.Errorf("the plan line carries review content:\n%s", got)
	}
}

// TestRenderPlanLine_NamesTheRuleSet: the first line names the rule
// set the review ran against — nightme's own rubric when the plan
// holds the built-in group, ocr's rules otherwise.
func TestRenderPlanLine_NamesTheRuleSet(t *testing.T) {
	builtin := renderPlanLine("codex", []agent.ReviewPlanGroup{
		{Pattern: agent.PatternBuiltin, Label: "nightme code reviewer · Go", Files: []string{"a.go"}},
	})
	if !strings.Contains(builtin, "nightme code reviewer") {
		t.Errorf("the plan line does not name nightme's own rule set:\n%s", builtin)
	}

	ocr := renderPlanLine("codex", []agent.ReviewPlanGroup{
		{Pattern: "**/*.go", Label: "system / **/*.go · Go", Files: []string{"a.go"}},
	})
	if !strings.Contains(ocr, "open-code-review") {
		t.Errorf("the plan line does not name ocr's rule set:\n%s", ocr)
	}
}

// TestRenderPlanLine_OneSubagent: a review of one group says "1
// subagent", not "1 subagents".
func TestRenderPlanLine_OneSubagent(t *testing.T) {
	got := renderPlanLine("codex", []agent.ReviewPlanGroup{
		{Pattern: agent.PatternBuiltin, Label: "nightme code reviewer · Go", Files: []string{"a.go"}},
	})
	if !strings.Contains(got, "1 subagent") {
		t.Errorf("the plan line does not count the single subagent:\n%s", got)
	}
	if strings.Contains(got, "1 subagents") {
		t.Errorf("the plan line pluralized the single subagent:\n%s", got)
	}
}

// TestVerdictLine: the verdict counts the subagents, the findings
// and their severities, names the groups that failed and points at
// the command that resumes them.
func TestVerdictLine(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{
			{pattern: "**/*.go"},
			{pattern: "**/*.ts"},
			{pattern: "**/*.rs"},
		},
		[]reviewContribution{
			testContribution("**/*.go",
				agent.ReviewFinding{Path: "a.go", Severity: agent.SeverityCritical},
				agent.ReviewFinding{Path: "b.go", Severity: agent.SeverityHigh},
				agent.ReviewFinding{Path: "c.go", Severity: agent.SeverityHigh}),
			testContribution("**/*.ts",
				agent.ReviewFinding{Path: "d.ts", Severity: agent.SeverityLow}),
			testContribution("**/*.rs"),
		},
		nil,
	)

	got := newVerdict(rec, false).line()
	for _, want := range []string{
		"3 subagents",
		"4 findings",
		"2 high",
		"1 critical",
		"1 low",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the verdict line does not carry %q: %s", want, got)
		}
	}
	if strings.Contains(got, "group") {
		t.Errorf("the verdict line counts groups instead of subagents: %s", got)
	}
	// A bucket with no findings is not shown.
	if strings.Contains(got, "medium") {
		t.Errorf("the verdict line shows an empty severity bucket: %s", got)
	}

	failed := testRecord(
		[]reviewOutcome{
			{pattern: "**/*.go"},
			{pattern: "**/*.ts", err: errors.New("boom"), sessionID: "sess-ts"},
		},
		[]reviewContribution{testContribution("**/*.go")},
		nil,
	)
	line := newVerdict(failed, false).line()
	for _, want := range []string{"`**/*.ts`", "💡 hint: run `/review --continue` to continue the failed groups"} {
		if !strings.Contains(line, want) {
			t.Errorf("the verdict line does not carry %q: %s", want, line)
		}
	}
}

// TestVerdictLine_FailedGroupsAreTheirOwnBlock: the groups that did
// not finish leave the counts line and stand on their own under it —
// the counts line carries counts and nothing else.
func TestVerdictLine_FailedGroupsAreTheirOwnBlock(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{
			{pattern: "**/*.go", source: "system"},
			{pattern: "**/*.ts", source: "system", err: errors.New("boom"), sessionID: "sess-ts"},
		},
		[]reviewContribution{
			testContribution("**/*.go", agent.ReviewFinding{Path: "a.go", Severity: agent.SeverityHigh}),
		},
		[]agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Source: "system", Label: "system / **/*.go · Go"},
			{Pattern: "**/*.ts", Source: "system", Label: "system / **/*.ts · Go"},
		},
	)

	got := newVerdict(rec, false).line()

	want := "⚠️ review · 2 subagents · 1 finding · 1 high\n\n" +
		"❌ **1 of 2 subagents failed**\n" +
		"- `system / **/*.ts · Go`\n\n" +
		"💡 hint: run `/review --continue` to continue the failed groups"
	if got != want {
		t.Errorf("the verdict line is:\n%s\nwant:\n%s", got, want)
	}
}

// TestVerdictLine_HintStandsOnItsOwn: the hint is a paragraph of its
// own — a line that follows a list item without a blank line is that
// item's continuation, so the hint would end up inside the last
// group's bullet.
func TestVerdictLine_HintStandsOnItsOwn(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{
			{pattern: "a", err: errors.New("boom"), sessionID: "sess-a"},
			{pattern: "b", err: errors.New("boom"), prompt: "the prompt"},
		},
		nil,
		nil,
	)

	got := newVerdict(rec, true).line()

	if !strings.Contains(got, "\n\n💡 hint:") {
		t.Errorf("the hint is not a paragraph of its own: %s", got)
	}
}

// TestVerdictLine_FailureBlockCountsTheGroups: the block's header
// names how many groups failed out of the review's subagents, and the
// noun follows the count.
func TestVerdictLine_FailureBlockCountsTheGroups(t *testing.T) {
	two := testRecord(
		[]reviewOutcome{
			{pattern: "a", err: errors.New("boom"), sessionID: "sess-a"},
			{pattern: "b", err: errors.New("boom"), prompt: "the prompt"},
		},
		nil,
		nil,
	)
	got := newVerdict(two, true).line()
	if !strings.Contains(got, "❌ **2 of 2 subagents failed**") {
		t.Errorf("the block does not count the failed groups: %s", got)
	}

	one := testRecord(
		[]reviewOutcome{{pattern: "a", err: errors.New("boom"), sessionID: "sess-a"}},
		nil,
		nil,
	)
	line := newVerdict(one, true).line()
	if !strings.Contains(line, "❌ **1 of 1 subagent failed**") {
		t.Errorf("the block pluralized the single subagent: %s", line)
	}
}

// TestVerdictLine_ListsEveryFailedGroup: the block names every group
// that failed — the list is not capped.
func TestVerdictLine_ListsEveryFailedGroup(t *testing.T) {
	var outcomes []reviewOutcome
	for i := 0; i < 5; i++ {
		outcomes = append(outcomes, reviewOutcome{
			pattern:   fmt.Sprintf("group-%d", i),
			sessionID: fmt.Sprintf("sess-%d", i),
			err:       errors.New("boom"),
		})
	}

	got := newVerdict(testRecord(outcomes, nil, nil), true).line()
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf("- group-%d\n", i)
		if !strings.Contains(got, want) {
			t.Errorf("the block does not list %q: %s", want, got)
		}
	}
}

// TestVerdictLine_CodeSpansTheGroupName: a name that carries a
// markdown metacharacter is code-spanned — a glob's `**` would
// otherwise pair with another name's `**` into a bold span. A plain
// name is left alone, and a name carrying a backtick is left alone
// too: a code span would break on it.
func TestVerdictLine_CodeSpansTheGroupName(t *testing.T) {
	cases := []struct {
		name  string
		label string
		want  string
	}{
		{"a glob", "system / **/*.go", "- `system / **/*.go`"},
		{"an underscored name", "internal/me/my_file.go", "- `internal/me/my_file.go`"},
		{"a plain name", "nightme code reviewer", "- nightme code reviewer\n"},
		{"a name with a backtick", "a `b` c", "- a `b` c\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := testRecord(
				[]reviewOutcome{{
					pattern:   tc.label,
					source:    "system",
					sessionID: "sess-1",
					err:       errors.New("boom"),
				}},
				nil,
				[]agent.ReviewPlanGroup{{Pattern: tc.label, Source: "system", Label: tc.label}},
			)
			got := newVerdict(rec, true).line()
			if !strings.Contains(got, tc.want) {
				t.Errorf("the verdict does not carry %q: %s", tc.want, got)
			}
		})
	}
}

// TestVerdictLine_KeepsTheUnparsedCountOnTheCountsLine: the unparsed
// groups are a count, so they stay on the counts line — and the
// failure block is unaffected by them.
func TestVerdictLine_KeepsTheUnparsedCountOnTheCountsLine(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{
			{pattern: "a"},
			{pattern: "b"},
			{pattern: "c", err: errors.New("boom"), sessionID: "sess-c"},
		},
		[]reviewContribution{
			{pattern: "a", parsed: false},
			{pattern: "b", parsed: false},
		},
		nil,
	)

	got := newVerdict(rec, true).line()

	counts, block, ok := strings.Cut(got, "\n\n")
	if !ok {
		t.Fatalf("the verdict line has no failure block:\n%s", got)
	}
	if !strings.Contains(counts, "2 unparsed subagents") {
		t.Errorf("the counts line does not report the unparsed subagents: %s", counts)
	}
	if strings.Contains(counts, "failed") {
		t.Errorf("the counts line carries the failure: %s", counts)
	}
	if !strings.Contains(block, "❌ **1 of 3 subagents failed**") {
		t.Errorf("the failure block does not count the failed group: %s", block)
	}
}

// TestVerdictHint_CarriesTheSameBody: the thread's end hint carries
// the same counts and the same failure block as the card's verdict —
// the two differ only in their lead.
func TestVerdictHint_CarriesTheSameBody(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{
			{pattern: "**/*.go", source: "system"},
			{pattern: "**/*.ts", source: "system", err: errors.New("boom"), sessionID: "sess-ts"},
		},
		[]reviewContribution{
			testContribution("**/*.go", agent.ReviewFinding{Path: "a.go", Severity: agent.SeverityHigh}),
		},
		[]agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Source: "system", Label: "system / **/*.go · Go"},
			{Pattern: "**/*.ts", Source: "system", Label: "system / **/*.ts · Go"},
		},
	)
	v := newVerdict(rec, false)

	hint := v.hint()
	want := "⚠️ **review done** · 2 subagents · 1 finding · 1 high\n\n" +
		"❌ **1 of 2 subagents failed**\n" +
		"- `system / **/*.ts · Go`\n\n" +
		"💡 hint: run `/review --continue` to continue the failed groups"
	if hint != want {
		t.Errorf("the end hint is:\n%s\nwant:\n%s", hint, want)
	}

	body := v.body()
	if !strings.HasSuffix(v.line(), body) || !strings.HasSuffix(hint, body) {
		t.Errorf("the card and the hint do not share the body:\n%s\n%s", v.line(), hint)
	}
}

// TestVerdictHint_SaysTheReviewIsPending: a review that is not over
// yet does not claim it is done — the thread and the card must say
// the same thing.
func TestVerdictHint_SaysTheReviewIsPending(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{
			{pattern: "**/*.go"},
			{pattern: "**/*.ts", err: errors.New("boom"), sessionID: "sess-ts"},
		},
		[]reviewContribution{testContribution("**/*.go")},
		nil,
	)

	hint := newVerdict(rec, true).hint()
	if !strings.Contains(hint, "**review pending**") {
		t.Errorf("the end hint does not say the review is pending: %s", hint)
	}
	if strings.Contains(hint, "review done") {
		t.Errorf("the end hint claims the review is done: %s", hint)
	}

	done := newVerdict(rec, false).hint()
	if !strings.Contains(done, "**review done**") {
		t.Errorf("the end hint of a delivered review does not say it is done: %s", done)
	}
}

// TestVerdictLine_UnparsedGroups: a group whose output matched no
// known shape is reported as unparsed.
func TestVerdictLine_UnparsedGroups(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "a"}, {pattern: "b"}},
		[]reviewContribution{
			testContribution("a"),
			{pattern: "b", parsed: false},
		},
		nil,
	)

	got := newVerdict(rec, false).line()
	if !strings.Contains(got, "1 unparsed subagent") {
		t.Errorf("the verdict line does not report the unparsed subagent: %s", got)
	}
}

// TestReviewVerdict_FailedGroupsAreNotUnparsed: a group that failed
// is counted as failed — never as unparsed.
func TestReviewVerdict_FailedGroupsAreNotUnparsed(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{
			{pattern: "**/*.go"},
			{pattern: "**/*.ts", err: errors.New("boom"), prompt: "the group's prompt"},
		},
		[]reviewContribution{testContribution("**/*.go")},
		[]agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go"},
			{Pattern: "**/*.ts", Label: "**/*.ts"},
		},
	)

	got := newVerdict(rec, true).line()
	if !strings.Contains(got, "2 subagents") {
		t.Errorf("the verdict does not count both subagents: %s", got)
	}
	if !strings.Contains(got, "**/*.ts") || !strings.Contains(got, "/review --continue") {
		t.Errorf("the verdict does not name the failed group: %s", got)
	}
	if strings.Contains(got, "unparsed") {
		t.Errorf("the failed group was counted as unparsed: %s", got)
	}
}

// TestReviewVerdict_NamesTheFailedGroupByItsIdentity: two ocr rules
// over the same glob are two groups — the verdict names the one that
// failed, not the other.
func TestReviewVerdict_NamesTheFailedGroupByItsIdentity(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{
			{pattern: "**/*.go", source: "security", err: errors.New("boom")},
			{pattern: "**/*.go", source: "performance"},
		},
		[]reviewContribution{testContribution("**/*.go")},
		[]agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Source: "security", Label: "security / **/*.go · Go"},
			{Pattern: "**/*.go", Source: "performance", Label: "performance / **/*.go · Go"},
		},
	)

	got := newVerdict(rec, true).line()
	if !strings.Contains(got, "security / **/*.go") {
		t.Errorf("the verdict does not name the failed group: %s", got)
	}
	if strings.Contains(got, "performance") {
		t.Errorf("the verdict names the group that did not fail: %s", got)
	}
}

// TestReviewVerdict_FallsBackToTheGroupsName: a group the plan did
// not name is still named by its identity — the source travels with
// the pattern.
func TestReviewVerdict_FallsBackToTheGroupsName(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go", source: "system", err: errors.New("boom")}},
		nil,
		nil,
	)

	got := newVerdict(rec, true).line()
	if !strings.Contains(got, "system / **/*.go") {
		t.Errorf("the verdict does not name the failed group: %s", got)
	}
}

// TestReviewVerdictHint: the thread's end hint carries the same
// numbers as the card's verdict, in Markdown.
func TestReviewVerdictHint(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{
			testContribution("**/*.go", agent.ReviewFinding{Severity: agent.SeverityHigh}),
		},
		[]agent.ReviewPlanGroup{{Pattern: "**/*.go", Label: "**/*.go"}},
	)

	got := reviewVerdictHint(rec, false)
	for _, want := range []string{"**review done**", "1 subagent", "1 finding", "1 high"} {
		if !strings.Contains(got, want) {
			t.Errorf("the end hint does not carry %q: %s", want, got)
		}
	}
}

// TestReviewDigest_GroupsBySeverity: the digest lists one table per
// severity bucket, from the most severe to the least, and a bucket
// with no findings produces no table at all.
func TestReviewDigest_GroupsBySeverity(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}, {pattern: "**/*.ts"}},
		[]reviewContribution{
			testContribution("**/*.go",
				agent.ReviewFinding{Path: "a.go", Line: "42", Severity: agent.SeverityHigh, Content: "first"},
				agent.ReviewFinding{Path: "b.go", Line: "7", Severity: agent.SeverityHigh, Content: "second"}),
			testContribution("**/*.ts",
				agent.ReviewFinding{Path: "c.ts", Line: "3", Severity: agent.SeverityLow, Content: "third"}),
		},
		[]agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go"},
			{Pattern: "**/*.ts", Label: "**/*.ts"},
		},
	)

	got := reviewDigest(rec)
	want := "**high** · 2\n" +
		"| path |\n" +
		"| --- |\n" +
		"| a.go:42 |\n" +
		"| b.go:7 |\n" +
		"\n" +
		"**low** · 1\n" +
		"| path |\n" +
		"| --- |\n" +
		"| c.ts:3 |"
	if got != want {
		t.Errorf("the digest is:\n%s\nwant:\n%s", got, want)
	}
}

// TestReviewDigest_OrdersRowsByThePlan: the rows of one bucket follow
// the plan — the group that comes first in the plan comes first in the
// digest, whatever order the groups finished in.
func TestReviewDigest_OrdersRowsByThePlan(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}, {pattern: "**/*.ts"}},
		[]reviewContribution{
			testContribution("**/*.ts",
				agent.ReviewFinding{Path: "c.ts", Line: "3", Severity: agent.SeverityHigh, Content: "the ts finding"}),
			testContribution("**/*.go",
				agent.ReviewFinding{Path: "a.go", Line: "42", Severity: agent.SeverityHigh, Content: "the go finding"}),
		},
		[]agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go"},
			{Pattern: "**/*.ts", Label: "**/*.ts"},
		},
	)

	got := reviewDigest(rec)
	goRow := strings.Index(got, "a.go:42")
	tsRow := strings.Index(got, "c.ts:3")
	if goRow < 0 || tsRow < 0 {
		t.Fatalf("the digest does not carry both rows:\n%s", got)
	}
	if goRow > tsRow {
		t.Errorf("the rows do not follow the plan:\n%s", got)
	}
}

// TestReviewDigest_CategoryColumn: the category column exists only
// when some finding of the run named a category — and a finding
// without one shows the em dash.
func TestReviewDigest_CategoryColumn(t *testing.T) {
	plain := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{
			testContribution("**/*.go",
				agent.ReviewFinding{Path: "a.go", Line: "42", Severity: agent.SeverityHigh, Content: "first"}),
		},
		nil,
	)

	got := reviewDigest(plain)
	if !strings.Contains(got, "| path |\n| --- |\n") {
		t.Errorf("the table has no path column:\n%s", got)
	}
	if strings.Contains(got, "category") {
		t.Errorf("the table has a category column although no finding named one:\n%s", got)
	}

	mixed := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{
			testContribution("**/*.go",
				agent.ReviewFinding{Path: "a.go", Line: "42", Severity: agent.SeverityHigh, Category: "error-handling", Content: "first"},
				agent.ReviewFinding{Path: "b.go", Line: "7", Severity: agent.SeverityHigh, Content: "second"}),
		},
		nil,
	)

	got = reviewDigest(mixed)
	if !strings.Contains(got, "| category | path |\n") {
		t.Errorf("the table has no category column:\n%s", got)
	}
	if !strings.Contains(got, "| --- | --- |\n") {
		t.Errorf("the separator row does not have one dash per column:\n%s", got)
	}
	if !strings.Contains(got, "| error-handling | a.go:42 |") {
		t.Errorf("the row does not lead with the category:\n%s", got)
	}
	if !strings.Contains(got, "| — | b.go:7 |") {
		t.Errorf("the finding without a category does not show the em dash:\n%s", got)
	}
}

// TestReviewDigest_Sanitizes: a cell stays one line — a pipe becomes
// a slash, a line break or a tab a space — and the cell is trimmed.
func TestReviewDigest_Sanitizes(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{
			testContribution("**/*.go",
				agent.ReviewFinding{Path: "a.go|", Line: "42", Severity: agent.SeverityHigh, Category: "error | handling\nx\ty"},
				agent.ReviewFinding{Path: " b.go ", Severity: agent.SeverityHigh}),
		},
		nil,
	)

	got := reviewDigest(rec)
	if !strings.Contains(got, "| error / handling x y | a.go/:42 |") {
		t.Errorf("the cell was not sanitized:\n%s", got)
	}
	if !strings.Contains(got, "| — | b.go |") {
		t.Errorf("the cell was not trimmed:\n%s", got)
	}
}

// TestReviewDigest_NoFindings: a review without findings produces no
// digest — the verdict message is the verdict line alone.
func TestReviewDigest_NoFindings(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{testContribution("**/*.go")},
		nil,
	)

	if got := reviewDigest(rec); got != "" {
		t.Errorf("a review without findings produced a digest:\n%s", got)
	}
}

// TestReviewDigest_SkipsFailedGroups: a group that failed produced no
// contribution, so it contributes no rows.
func TestReviewDigest_SkipsFailedGroups(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{
			{pattern: "**/*.go", err: errors.New("boom")},
			{pattern: "**/*.ts"},
		},
		[]reviewContribution{
			testContribution("**/*.ts",
				agent.ReviewFinding{Path: "c.ts", Line: "3", Severity: agent.SeverityHigh, Content: "the ts finding"}),
		},
		nil,
	)

	got := reviewDigest(rec)
	if strings.Contains(got, "**/*.go") {
		t.Errorf("the failed group reached the digest:\n%s", got)
	}
	if !strings.Contains(got, "| c.ts:3 |") {
		t.Errorf("the digest does not carry the finished group's finding:\n%s", got)
	}
}

// TestReviewDigest_KeepsEveryFinding: the digest carries every
// finding of the review — no row is dropped, and no tail names the
// findings that were left out.
func TestReviewDigest_KeepsEveryFinding(t *testing.T) {
	var findings []agent.ReviewFinding
	for i := 0; i < 12; i++ {
		findings = append(findings, agent.ReviewFinding{
			Path:     fmt.Sprintf("internal/agent/review.go:%d", 100+i),
			Severity: agent.SeverityHigh,
			Content:  strings.Repeat("错", 80),
		})
	}
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{testContribution("**/*.go", findings...)},
		nil,
	)

	got := reviewDigest(rec)
	rows := strings.Count(got, "| internal/agent/review.go:")
	if rows != len(findings) {
		t.Errorf("the digest holds %d of %d rows:\n%s", rows, len(findings), got)
	}
	if strings.Contains(got, "more findings") {
		t.Errorf("the digest names findings it left out:\n%s", got)
	}
}

// TestSendVerdict_CarriesTheWholeDigest: the verdict message carries
// the whole digest — one message, every finding of the review.
func TestSendVerdict_CarriesTheWholeDigest(t *testing.T) {
	var findings []agent.ReviewFinding
	for i := 0; i < 60; i++ {
		findings = append(findings, agent.ReviewFinding{
			Path:     fmt.Sprintf("internal/gateway/outbound/policy.go:%d", 40+i),
			Severity: agent.SeverityHigh,
			Category: "error-handling",
			Content:  strings.Repeat("错", 80),
		})
	}
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{testContribution("**/*.go", findings...)},
		nil,
	)

	em := newRecordingEmitter()
	sendVerdict(context.Background(), em, "chat", "msg", "codex", agent.RunResult{},
		newVerdict(rec, false), reviewDigest(rec))

	msgs := em.messages()
	if len(msgs) != 1 {
		t.Fatalf("the review sent %d messages, want the one verdict message: %+v", len(msgs), msgs)
	}
	text := msgs[0].Text
	if !strings.Contains(text, "60 findings") {
		t.Errorf("the verdict line does not report every finding:\n%s", text)
	}
	if rows := strings.Count(text, "internal/gateway/outbound/policy.go:"); rows != len(findings) {
		t.Errorf("the verdict message holds %d of %d rows:\n%s", rows, len(findings), text)
	}
	if strings.Contains(text, "more findings") {
		t.Errorf("the verdict message names findings it left out:\n%s", text)
	}
	if prose := strings.Repeat("错", 80); strings.Contains(text, prose) {
		t.Errorf("the verdict message carries the findings' prose:\n%s", text)
	}
}

// TestSendVerdict_CarriesEveryLocationAndNoProse: the verdict
// message holds every finding's location and none of their prose —
// the card is an index of the findings, their text stays in the
// thread and in the main agent's input.
func TestSendVerdict_CarriesEveryLocationAndNoProse(t *testing.T) {
	var findings []agent.ReviewFinding
	for i := 0; i < 40; i++ {
		findings = append(findings, agent.ReviewFinding{
			Path:     fmt.Sprintf("internal/agent/review.go:%d", 100+i),
			Severity: agent.SeverityHigh,
			Content:  fmt.Sprintf("finding %d: %s", i, strings.Repeat("细节", 30)),
		})
	}
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{testContribution("**/*.go", findings...)},
		nil,
	)

	em := newRecordingEmitter()
	sendVerdict(context.Background(), em, "chat", "msg", "codex", agent.RunResult{},
		newVerdict(rec, false), reviewDigest(rec))

	msgs := em.messages()
	if len(msgs) != 1 {
		t.Fatalf("the review sent %d messages, want the one verdict message: %+v", len(msgs), msgs)
	}
	text := msgs[0].Text
	for i := range findings {
		if want := fmt.Sprintf("internal/agent/review.go:%d", 100+i); !strings.Contains(text, want) {
			t.Errorf("the verdict message does not carry %q:\n%s", want, text)
		}
		if prose := fmt.Sprintf("finding %d: ", i); strings.Contains(text, prose) {
			t.Errorf("the verdict message carries the finding's prose %q:\n%s", prose, text)
		}
	}
}

// failingEmitter rejects every message — the channel refused the
// body.
type failingEmitter struct {
	err error
}

func (e *failingEmitter) Send(_ context.Context, _ messages.OutboundMessage) (string, error) {
	return "", e.err
}

// TestSendVerdict_LogsAFailedSend: a channel that refuses the
// verdict is logged — the verdict is the user's only record of what
// the review found, so a lost one must not vanish silently.
func TestSendVerdict_LogsAFailedSend(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{
			testContribution("**/*.go",
				agent.ReviewFinding{Path: "a.go", Line: "42", Severity: agent.SeverityHigh, Content: "the finding"}),
		},
		nil,
	)

	sendVerdict(context.Background(), &failingEmitter{err: errors.New("channel said no")},
		"chat", "msg", "codex", agent.RunResult{}, newVerdict(rec, false), reviewDigest(rec))

	if !strings.Contains(logs.String(), "channel said no") {
		t.Errorf("the failed verdict send was not logged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("the failed verdict send was not logged at warn:\n%s", logs.String())
	}
}

// TestSendReviewLine_LogsAFailedSend: the review's other lines are
// logged the same way — a lost plan line is as invisible as a lost
// verdict otherwise.
func TestSendReviewLine_LogsAFailedSend(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	sendReviewLine(context.Background(), &failingEmitter{err: errors.New("channel said no")},
		"chat", "msg", "codex", "🔍 review · codex")

	if !strings.Contains(logs.String(), "channel said no") {
		t.Errorf("the failed line send was not logged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("the failed line send was not logged at warn:\n%s", logs.String())
	}
}

// TestSendVerdict_CarriesTheDigest: the digest rides inside the
// verdict message — one message, one card entry — and a review
// without findings leaves the message as the verdict line alone.
func TestSendVerdict_CarriesTheDigest(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{
			testContribution("**/*.go",
				agent.ReviewFinding{Path: "a.go", Line: "42", Severity: agent.SeverityHigh, Category: "error-handling", Content: "the finding"}),
		},
		nil,
	)

	em := newRecordingEmitter()
	sendVerdict(context.Background(), em, "chat", "msg", "codex", agent.RunResult{},
		newVerdict(rec, false), reviewDigest(rec))

	msgs := em.messages()
	if len(msgs) != 1 {
		t.Fatalf("the review sent %d messages, want the one verdict message: %+v", len(msgs), msgs)
	}
	text := msgs[0].Text
	for _, want := range []string{
		"1 subagent",
		"**high** · 1",
		"| category | path |",
		"| error-handling | a.go:42 |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the verdict message does not carry %q:\n%s", want, text)
		}
	}

	empty := newRecordingEmitter()
	clean := testRecord(
		[]reviewOutcome{{pattern: "**/*.go"}},
		[]reviewContribution{testContribution("**/*.go")},
		nil,
	)
	sendVerdict(context.Background(), empty, "chat", "msg", "codex", agent.RunResult{},
		newVerdict(clean, false), reviewDigest(clean))

	msgs = empty.messages()
	if len(msgs) != 1 {
		t.Fatalf("the review sent %d messages, want the one verdict message: %+v", len(msgs), msgs)
	}
	if text := msgs[0].Text; strings.Contains(text, "|") || strings.HasSuffix(text, "\n") {
		t.Errorf("the verdict message of a review without findings changed:\n%q", text)
	}
}

// TestSendVerdict_PutsTheFailureBlockBetweenTheCountsAndTheDigest:
// the card message reads as the counts, the failure and the digest —
// the failure block sits under the counts line and above the digest.
func TestSendVerdict_PutsTheFailureBlockBetweenTheCountsAndTheDigest(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{
			{pattern: "**/*.go"},
			{pattern: "**/*.ts", source: "system", err: errors.New("boom"), sessionID: "sess-ts"},
		},
		[]reviewContribution{
			testContribution("**/*.go",
				agent.ReviewFinding{Path: "a.go", Line: "42", Severity: agent.SeverityHigh, Content: "the finding"}),
		},
		[]agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go"},
			{Pattern: "**/*.ts", Source: "system", Label: "system / **/*.ts"},
		},
	)

	em := newRecordingEmitter()
	sendVerdict(context.Background(), em, "chat", "msg", "codex", agent.RunResult{},
		newVerdict(rec, false), reviewDigest(rec))

	msgs := em.messages()
	if len(msgs) != 1 {
		t.Fatalf("the review sent %d messages, want the one verdict message: %+v", len(msgs), msgs)
	}

	want := "⚠️ review · 2 subagents · 1 finding · 1 high\n\n" +
		"❌ **1 of 2 subagents failed**\n" +
		"- `system / **/*.ts`\n\n" +
		"💡 hint: run `/review --continue` to continue the failed groups\n\n" +
		"**high** · 1\n" +
		"| path |\n" +
		"| --- |\n" +
		"| a.go:42 |"
	if msgs[0].Text != want {
		t.Errorf("the verdict message is:\n%s\nwant:\n%s", msgs[0].Text, want)
	}
}

// TestReviewPromptFrame_FollowsTheThinkMode: the frame asks the
// agent to state the findings' substance when the thread carries
// none, and to leave them alone when the thread already has them.
//
// The card's digest lists only the locations, so with /think off the
// agent's answer is the reader's only view of what the findings say.
func TestReviewPromptFrame_FollowsTheThinkMode(t *testing.T) {
	shown := reviewPromptFrame(true)
	if !strings.Contains(shown, "Do not restate the findings") {
		t.Errorf("the frame lets the agent copy the findings the thread already carries:\n%s", shown)
	}

	hidden := reviewPromptFrame(false)
	if strings.Contains(hidden, "Do not restate the findings") {
		t.Errorf("the frame tells the agent to withhold the findings the chat no longer shows:\n%s", hidden)
	}
	if !strings.Contains(hidden, "what each of them says") {
		t.Errorf("the frame does not ask for the findings' substance:\n%s", hidden)
	}
}

// planReviewStarter is a fake starter that reports a plan and one
// outcome per group — the way the real fan-out does — and returns a
// merged text.
//
// reply, when set, is the text every group produces: the fake emits
// it through the review's sink, so the findings reach the chat the way
// a real group's do.
type planReviewStarter struct {
	name   string
	groups []agent.ReviewPlanGroup
	failed int
	// texts are the groups' own outputs — what the merged document
	// carries under each group's header.
	texts []string
	reply string
	// prompt is the prompt every group ran — the record keeps it, so
	// a group that never reached the agent can be re-run with it.
	prompt string
	// timedOut is how many of the groups report a silence timeout —
	// the first ones, so the count lines up with `failed`.
	timedOut int
}

func (s *planReviewStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, "", nil, nil)
}
func (s *planReviewStarter) Detect() error { return nil }
func (s *planReviewStarter) Init(string)   {}
func (s *planReviewStarter) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return nil, errors.New("planReviewStarter: Start not implemented")
}
func (s *planReviewStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("planReviewStarter: RunOnce not implemented")
}

func (s *planReviewStarter) Review(_ context.Context, _ agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	cfg := agent.ParseRunOnceOptions(opts)
	if cfg.OnReviewPlan != nil {
		cfg.OnReviewPlan(s.groups)
	}
	if cfg.OnEvent != nil && s.reply != "" {
		for i := range s.groups {
			cfg.OnEvent(agent.AgentEvent{
				Kind:      agent.EventAgentReady,
				SessionID: fmt.Sprintf("sess-%d", i+1),
				Model:     "model-x",
			})
			// The aggregator hands a group's findings over as one
			// thinking event carrying the group's number.
			cfg.OnEvent(agent.AgentEvent{
				Kind: agent.EventAgentThinking,
				Text: fmt.Sprintf("[%d] %s of group %d", i+1, s.reply, i+1),
			})
			cfg.OnEvent(agent.AgentEvent{
				Kind:   agent.EventAgentResult,
				Result: &agent.AgentResultEvent{},
			})
		}
	}
	if cfg.OnReviewJob != nil {
		for i, g := range s.groups {
			var err error
			switch {
			case i < s.timedOut:
				err = &agent.GroupTimeoutError{
					Group:   agent.ReviewGroupName(g.Source, g.Pattern),
					Timeout: 15 * time.Minute,
				}
			case i < s.failed:
				err = fmt.Errorf("group %d failed", i+1)
			}
			report := agent.ReviewReport{Parsed: true}
			if i >= s.failed && i >= s.timedOut {
				report.Findings = []agent.ReviewFinding{{
					Path:     fmt.Sprintf("file%d.go", i+1),
					Line:     fmt.Sprintf("%d", i+1),
					Severity: agent.SeverityHigh,
					Category: "correctness",
					Content:  fmt.Sprintf("issue %d in group %d", i+1, i+1),
				}}
			}
			text := fmt.Sprintf("the findings of group %d", i+1)
			if i < len(s.texts) {
				text = s.texts[i]
			}
			cfg.OnReviewJob(agent.ReviewJob{
				Pattern:   g.Pattern,
				Source:    g.Source,
				Files:     g.Files,
				SessionID: fmt.Sprintf("sess-%d", i+1),
				Prompt:    s.prompt,
				Report:    report,
				Text:      text,
			}, err)
		}
	}
	return agent.RunResult{Text: "the merged review"}, nil
}

// outReplies returns the OutReply messages the emitter received — the
// review's own lines, in order.
func outReplies(em *recordingEmitter) []messages.OutboundMessage {
	var out []messages.OutboundMessage
	for _, msg := range em.messages() {
		if msg.Kind == messages.OutReply {
			out = append(out, msg)
		}
	}
	return out
}

// waitForOutReplies waits until the emitter has recorded n OutReply
// messages — the review's own lines.
func waitForOutReplies(t *testing.T, em *recordingEmitter, n int) []messages.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		replies := outReplies(em)
		if len(replies) >= n {
			return replies
		}
		if time.Now().After(deadline) {
			t.Fatalf("the emitter received %d lines, want %d: %+v", len(replies), n, replies)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForMessages waits until the emitter has recorded n messages of
// any kind — the review's card lines and the thread's messages.
func waitForMessages(t *testing.T, em *recordingEmitter, n int) []messages.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs := em.messages()
		if len(msgs) >= n {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("the emitter received %d messages, want %d: %+v", len(msgs), n, msgs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForReviewMessages waits until the emitter has recorded n of the
// review's own messages — its card lines and the thread's messages,
// without the session's init and heartbeat traffic.
func waitForReviewMessages(t *testing.T, em *recordingEmitter, n int) []messages.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var msgs []messages.OutboundMessage
		for _, msg := range em.messages() {
			if msg.Kind != messages.OutReply && msg.Kind != messages.OutThinking {
				continue
			}
			msgs = append(msgs, msg)
		}
		if len(msgs) >= n {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("the emitter received %d review messages, want %d: %+v", len(msgs), n, msgs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForBlocks waits until the driver received n blocks.
func waitForBlocks(t *testing.T, d *recordingDriver, n int) []agent.ContentBlock {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		blocks := d.blocks()
		if len(blocks) >= n {
			return blocks
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent session received %d blocks, want %d", len(blocks), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHandle_ReviewDeliversThePlanAndTheVerdict: the channel sees
// the plan first and the verdict last, and never the merged body —
// the merged text goes to the main agent's session instead, with the
// instruction to answer with a verdict and the next step.
func TestHandle_ReviewDeliversThePlanAndTheVerdict(t *testing.T) {
	agents := newTestAgents(t)
	starter := &planReviewStarter{
		name: "review-delivery-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go", "b.go"}},
			{Pattern: "_nightme_simplify", Label: "simplify lens", Files: []string{"a.go"}},
		},
		texts: []string{"the go findings", "the simplify findings"},
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-delivery-chat"
	cs, asDriver := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")

	// The verdict line is the last thing the review sends.
	replies := waitForOutReplies(t, em, 2)
	if len(replies) < 2 {
		t.Fatalf("the review sent %d lines to the channel, want the plan and the verdict: %+v", len(replies), replies)
	}

	plan := replies[0]
	if !strings.Contains(plan.Text, starter.name) {
		t.Errorf("the first line does not name the runner: %q", plan.Text)
	}
	if !strings.Contains(plan.Text, "2 subagents") {
		t.Errorf("the first line does not carry the subagent count: %q", plan.Text)
	}
	if !strings.Contains(plan.Text, "simplify lens") {
		t.Errorf("the first line does not list the groups: %q", plan.Text)
	}

	verdict := replies[len(replies)-1]
	if !strings.Contains(verdict.Text, "2 subagents") {
		t.Errorf("the verdict does not report the subagent count: %q", verdict.Text)
	}
	if !strings.Contains(verdict.Text, "2 high") {
		t.Errorf("the verdict does not count the findings by severity: %q", verdict.Text)
	}

	// The merged body never reaches the channel.
	for _, msg := range replies {
		if strings.Contains(msg.Text, "### Group: pattern ") {
			t.Errorf("the merged body reached the channel: %q", msg.Text)
		}
	}

	// The main agent's session receives the merged text — in full —
	// plus the instruction to answer with a verdict.
	blocks := waitForBlocks(t, asDriver, 1)
	injected := blocks[0].Text
	if !strings.Contains(injected, "the go findings") || !strings.Contains(injected, "the simplify findings") {
		t.Errorf("the injected turn does not carry the merged text:\n%s", injected)
	}
	if !strings.Contains(injected, "verdict") {
		t.Errorf("the injected turn does not ask for a verdict:\n%s", injected)
	}
}

// TestHandle_ReviewFrameFollowsTheChat: the injected turn asks the
// agent to state the findings when the chat's /think keeps the
// thread empty, and to leave them alone when the thread has them.
//
// The card's digest lists only the locations — with /think off the
// agent's answer is the reader's only view of what the findings say.
func TestHandle_ReviewFrameFollowsTheChat(t *testing.T) {
	agents := newTestAgents(t)
	starter := &planReviewStarter{
		name:   "review-frame-agent",
		groups: []agent.ReviewPlanGroup{{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}}},
		texts:  []string{"the go findings"},
	}
	agents.registry.Register(starter)

	hiddenEmitter := newRecordingEmitter()
	const hiddenChat = "review-frame-hidden-chat"
	hiddenSession, hiddenDriver := newContinueChatSession(t, hiddenChat, starter.name, t.TempDir(), hiddenEmitter)

	f := NewFactory()
	dispatchContinue(t, f, hiddenSession, hiddenChat, "review")

	hidden := waitForBlocks(t, hiddenDriver, 1)[0].Text
	if !strings.Contains(hidden, "what each of them says") {
		t.Errorf("the injected turn does not ask for the findings the chat does not show:\n%s", hidden)
	}

	shownEmitter := newRecordingEmitter()
	const shownChat = "review-frame-shown-chat"
	shownSession, shownDriver := newContinueChatSession(t, shownChat, starter.name, t.TempDir(), shownEmitter)
	enableThink(t, shownSession)

	shown := NewFactory()
	dispatchContinue(t, shown, shownSession, shownChat, "review")

	injected := waitForBlocks(t, shownDriver, 1)[0].Text
	if !strings.Contains(injected, "Do not restate the findings") {
		t.Errorf("the injected turn lets the agent copy the findings the thread already carries:\n%s", injected)
	}
}

// TestHandle_ReviewDeliversTheFindingsToTheThread: the card carries
// the plan and the verdict — and nothing else. The findings reach the
// chat in the thread, one reply per group, each starting with the
// group's number, and the review closes with its verdict.
//
// The verdict's digest carries the parsed findings as rows; the
// group's own prose stays in the thread.
//
// The fake starter emits the events the aggregator produces; the
// aggregator's own contract is covered by the agent package's tests.
func TestHandle_ReviewDeliversTheFindingsToTheThread(t *testing.T) {
	agents := newTestAgents(t)
	starter := &planReviewStarter{
		name: "review-thread-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "system / **/*.go · Go", Files: []string{"a.go", "b.go"}},
			{Pattern: agent.PatternSimplify, Label: "Simplify review lens · Go", Files: []string{"a.go"}},
		},
		reply: "the findings",
		texts: []string{"the go findings", "the simplify findings"},
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-thread-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)
	enableThink(t, cs)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")

	// Five messages carry the run: the plan, one thread message per
	// group, the end hint, and the verdict. Waiting for fewer lets
	// the poll catch the run mid-flight — the verdict lands last,
	// so a snapshot taken before it arrives misses a reply.
	msgs := waitForReviewMessages(t, em, 5)

	// The card carries the plan and the verdict — and no group text.
	var replies []string
	var thinking []string
	for _, msg := range msgs {
		switch msg.Kind {
		case messages.OutReply:
			replies = append(replies, msg.Text)
		case messages.OutThinking:
			thinking = append(thinking, msg.Text)
		}
	}
	if len(replies) != 2 {
		t.Fatalf("the card carries %d lines, want the plan and the verdict: %+v", len(replies), replies)
	}
	if !strings.Contains(replies[0], "🔍 review") || !strings.Contains(replies[0], "2 subagents") {
		t.Errorf("the first line is not the plan: %q", replies[0])
	}
	if !strings.Contains(replies[0], "1. `system / **/*.go · Go` — 2 files") {
		t.Errorf("the first line does not list the groups: %q", replies[0])
	}
	if !strings.Contains(replies[1], "2 subagents") || !strings.Contains(replies[1], "2 high") {
		t.Errorf("the second line is not the verdict: %q", replies[1])
	}
	for _, reply := range replies {
		if strings.Contains(reply, "the findings") {
			t.Errorf("a group's findings reached the card: %q", reply)
		}
	}

	// The thread carries the findings — one reply per group, each
	// starting with the group's number — and the review's end hint.
	if len(thinking) != 3 {
		t.Fatalf("the thread carries %d messages, want 2 findings and the end hint: %+v", len(thinking), thinking)
	}
	if !strings.Contains(thinking[0], "[1] the findings of group 1") {
		t.Errorf("the first group's findings do not carry the group's number: %q", thinking[0])
	}
	if !strings.Contains(thinking[1], "[2] the findings of group 2") {
		t.Errorf("the second group's findings do not carry the group's number: %q", thinking[1])
	}
	if !strings.Contains(thinking[2], "review done") {
		t.Errorf("the thread does not close on the verdict: %q", thinking[2])
	}
}

// TestHandle_ReviewVerdictCarriesTheDigest: the verdict message
// carries the digest — one table per severity, one row per finding —
// and it reaches the card whatever the chat's /think setting says.
//
// The thread carries the groups' findings; the card carries their
// digest. A user with /think off still reads what the review found.
func TestHandle_ReviewVerdictCarriesTheDigest(t *testing.T) {
	agents := newTestAgents(t)
	starter := &planReviewStarter{
		name: "review-digest-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}},
			{Pattern: agent.PatternSimplify, Label: "simplify lens", Files: []string{"a.go"}},
		},
		texts: []string{"the go findings"},
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-digest-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")

	// /think is off: the card carries the plan and the verdict, and
	// the thread carries nothing.
	msgs := waitForReviewMessages(t, em, 2)
	if len(msgs) != 2 {
		t.Fatalf("the review sent %d messages, want the plan and the verdict: %+v", len(msgs), msgs)
	}
	for _, msg := range msgs {
		if msg.Kind != messages.OutReply {
			t.Errorf("the thread carries a message while /think is off: %+v", msg)
		}
	}

	verdict := msgs[1]
	for _, want := range []string{
		"2 subagents",
		"2 high",
		"**high** · 2",
		"| category | path |",
		"| --- | --- |",
		"| correctness | file1.go:1 |",
		"| correctness | file2.go:2 |",
	} {
		if !strings.Contains(verdict.Text, want) {
			t.Errorf("the verdict does not carry %q:\n%s", want, verdict.Text)
		}
	}
}

// TestHandle_ReviewNamesTheFailedGroup: a group that failed is named
// in the verdict, with the command that resumes it.
func TestHandle_ReviewNamesTheFailedGroup(t *testing.T) {
	agents := newTestAgents(t)
	starter := &planReviewStarter{
		name: "review-failed-group-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}},
			{Pattern: "**/*.ts", Label: "**/*.ts", Files: []string{"c.ts"}},
		},
		failed: 1,
		texts:  []string{"the go findings", "the ts findings"},
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-failed-group-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")

	em.waitForOutReply(t, "/review --continue")

	replies := waitForOutReplies(t, em, 2)
	verdict := replies[len(replies)-1]
	if !strings.Contains(verdict.Text, "**/*.go") {
		t.Errorf("the verdict does not name the failed group: %q", verdict.Text)
	}
	if !strings.Contains(verdict.Text, "2 subagents") {
		t.Errorf("the verdict does not report the subagent count: %q", verdict.Text)
	}
}

// TestVerdictLine_UnparsedGroupsAreNotGreen: a verdict whose
// count is incomplete (a group's output could not be parsed) must not
// read as a clean "nothing found".
func TestVerdictLine_UnparsedGroupsAreNotGreen(t *testing.T) {
	rec := testRecord(
		[]reviewOutcome{{pattern: "a"}, {pattern: "b"}},
		[]reviewContribution{
			testContribution("a"),
			{pattern: "b", parsed: false},
		},
		nil,
	)

	got := newVerdict(rec, false).line()
	if !strings.Contains(got, "⚠️") {
		t.Errorf("the verdict of an unparsed review is not marked: %s", got)
	}
	if !strings.Contains(got, "1 unparsed subagent") {
		t.Errorf("the verdict does not report the unparsed subagent: %s", got)
	}
}
