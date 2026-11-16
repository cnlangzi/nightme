package agent

// The parser that turns one review group's raw output into findings.
//
// The parser has to tolerate the shapes the review prompts produce —
// the ocr delegate rules' per-finding block, the builtin rubric's
// one-line form, the simplify lens' severity vocabulary — and has to
// report an output it does not recognize as unparsed rather than
// dropping it silently.

import (
	"strings"
	"testing"
)

// TestParseReviewFindings_BlockForm covers the per-finding block the
// ocr delegate rules ask for.
func TestParseReviewFindings_BlockForm(t *testing.T) {
	text := `## Coverage
- total: 2
- reviewed: 2
- skipped: 0
- coverage_rate: 2/2

## Findings
- path: internal/foo.go
- start_line: 42
- end_line: 50
- category: bug
- severity: high
- content: the retry swallows the error
- path: internal/bar.go
- start_line: 7
- end_line: 7
- category: security
- severity: critical
- content: the token is logged
`

	report := ParseReviewFindings(text)
	if !report.Parsed {
		t.Fatalf("the block form was not recognized: %+v", report)
	}
	if len(report.Findings) != 2 {
		t.Fatalf("parsed %d findings, want 2: %+v", len(report.Findings), report.Findings)
	}

	first := report.Findings[0]
	if first.Path != "internal/foo.go" {
		t.Errorf("path = %q, want %q", first.Path, "internal/foo.go")
	}
	if first.Line != "42" {
		t.Errorf("line = %q, want %q", first.Line, "42")
	}
	if first.Severity != SeverityHigh {
		t.Errorf("severity = %q, want %q", first.Severity, SeverityHigh)
	}
	if first.Category != "bug" {
		t.Errorf("category = %q, want %q", first.Category, "bug")
	}
	if first.Content != "the retry swallows the error" {
		t.Errorf("content = %q, want the finding's text", first.Content)
	}

	second := report.Findings[1]
	if second.Path != "internal/bar.go" || second.Severity != SeverityCritical {
		t.Errorf("second finding = %+v, want internal/bar.go at critical", second)
	}
}

// TestParseReviewFindings_OneLineForm covers the one-line form the
// builtin rubric asks for.
func TestParseReviewFindings_OneLineForm(t *testing.T) {
	text := `## Summary
The change adds a retry loop.

## Findings
- **critical**: internal/foo.go:42 — the retry swallows the error
- **medium**: internal/bar.go:7 — the helper is duplicated

## Suggestions
- internal/baz.go:3 — extract the helper
`

	report := ParseReviewFindings(text)
	if !report.Parsed {
		t.Fatalf("the one-line form was not recognized: %+v", report)
	}
	if len(report.Findings) != 2 {
		t.Fatalf("parsed %d findings, want 2 (the Suggestions bullet is not a finding): %+v",
			len(report.Findings), report.Findings)
	}
	first := report.Findings[0]
	if first.Path != "internal/foo.go" {
		t.Errorf("path = %q, want %q", first.Path, "internal/foo.go")
	}
	if first.Line != "42" {
		t.Errorf("line = %q, want %q", first.Line, "42")
	}
	if first.Severity != SeverityCritical {
		t.Errorf("severity = %q, want %q", first.Severity, SeverityCritical)
	}
	if first.Content != "the retry swallows the error" {
		t.Errorf("content = %q, want the issue text", first.Content)
	}
	if report.Findings[1].Severity != SeverityMedium {
		t.Errorf("second severity = %q, want %q", report.Findings[1].Severity, SeverityMedium)
	}
}

// TestParseReviewFindings_SimplifyVocabulary covers the simplify
// lens' severity words: they land on the same buckets.
func TestParseReviewFindings_SimplifyVocabulary(t *testing.T) {
	text := `## Findings
- path: internal/foo.go
- start_line: 10
- end_line: 20
- category: reuse
- severity: blocking
- content: the helper exists already
- path: internal/bar.go
- start_line: 3
- end_line: 3
- category: simplification
- severity: warning
- content: the branch is redundant
`

	report := ParseReviewFindings(text)
	if !report.Parsed {
		t.Fatalf("the block form was not recognized: %+v", report)
	}
	if len(report.Findings) != 2 {
		t.Fatalf("parsed %d findings, want 2: %+v", len(report.Findings), report.Findings)
	}
	if report.Findings[0].Severity != SeverityCritical {
		t.Errorf("blocking → %q, want %q", report.Findings[0].Severity, SeverityCritical)
	}
	if report.Findings[1].Severity != SeverityHigh {
		t.Errorf("warning → %q, want %q", report.Findings[1].Severity, SeverityHigh)
	}
}

// TestNormalizeReviewSeverity pins the severity mapping: the two
// vocabularies fold onto the same buckets, and a word the parser
// does not know becomes unspecified.
func TestNormalizeReviewSeverity(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"critical", SeverityCritical},
		{"high", SeverityHigh},
		{"medium", SeverityMedium},
		{"low", SeverityLow},
		{"blocking", SeverityCritical},
		{"warning", SeverityHigh},
		{"style", SeverityLow},
		{"suggestion", SeverityLow},
		{"Critical", SeverityCritical},
		{"  warning  ", SeverityHigh},
		{"blocker", SeverityUnspecified},
		{"nit", SeverityUnspecified},
		{"", SeverityUnspecified},
	}
	for _, tc := range cases {
		if got := NormalizeReviewSeverity(tc.in); got != tc.want {
			t.Errorf("NormalizeReviewSeverity(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseReviewFindings_UnknownSeverityIsUnspecified: a finding
// whose severity the parser does not know is still a finding — it
// lands in the unspecified bucket, it is not dropped.
func TestParseReviewFindings_UnknownSeverityIsUnspecified(t *testing.T) {
	report := ParseReviewFindings("- **weird**: internal/foo.go:3 — odd")
	if !report.Parsed {
		t.Fatalf("the one-line form was not recognized: %+v", report)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
	}
	if report.Findings[0].Severity != SeverityUnspecified {
		t.Errorf("severity = %q, want %q", report.Findings[0].Severity, SeverityUnspecified)
	}
}

// TestParseReviewFindings_UnparsedOutput: an output that matches no
// known shape is reported as unparsed — and yields no finding.
func TestParseReviewFindings_UnparsedOutput(t *testing.T) {
	report := ParseReviewFindings("I looked at the diff and it seems fine to me.")
	if report.Parsed {
		t.Errorf("an unrecognized output was reported as parsed: %+v", report)
	}
	if len(report.Findings) != 0 {
		t.Errorf("an unrecognized output produced findings: %+v", report.Findings)
	}
}

// TestParseReviewFindings_NoFindings: the shape the prompts ask for
// when there is nothing to flag — parsed, zero findings.
func TestParseReviewFindings_NoFindings(t *testing.T) {
	report := ParseReviewFindings("## Coverage\n- total: 3\n\nNo blockers; nothing material to flag.")
	if !report.Parsed {
		t.Errorf("the no-findings shape was not recognized: %+v", report)
	}
	if len(report.Findings) != 0 {
		t.Errorf("the no-findings shape produced findings: %+v", report.Findings)
	}
}

// TestParseReviewFindings_EmptyOutput: a group that produced nothing
// at all is unparsed — there is no shape to recognize.
func TestParseReviewFindings_EmptyOutput(t *testing.T) {
	report := ParseReviewFindings("   \n\n")
	if report.Parsed {
		t.Errorf("an empty output was reported as parsed: %+v", report)
	}
	if len(report.Findings) != 0 {
		t.Errorf("an empty output produced findings: %+v", report.Findings)
	}
}

// TestParseReviewFindings_MixedShapes: one output may carry both
// shapes — a group that reports a block and a one-line finding.
func TestParseReviewFindings_MixedShapes(t *testing.T) {
	text := `## Findings
- path: internal/foo.go
- start_line: 1
- end_line: 1
- category: bug
- severity: low
- content: the nil check is missing
- **high**: internal/bar.go:9 — the lock is never released
`
	report := ParseReviewFindings(text)
	if !report.Parsed {
		t.Fatalf("the output was not recognized: %+v", report)
	}
	if len(report.Findings) != 2 {
		t.Fatalf("parsed %d findings, want 2: %+v", len(report.Findings), report.Findings)
	}
	if report.Findings[0].Severity != SeverityLow {
		t.Errorf("first severity = %q, want %q", report.Findings[0].Severity, SeverityLow)
	}
	if report.Findings[1].Severity != SeverityHigh {
		t.Errorf("second severity = %q, want %q", report.Findings[1].Severity, SeverityHigh)
	}
}

// TestParseReviewFindings_BlockWithoutSeverity: the prompts allow a
// block to omit any field it cannot determine. Such a finding is
// still a finding — its severity is unspecified.
func TestParseReviewFindings_BlockWithoutSeverity(t *testing.T) {
	text := `- path: internal/foo.go
- start_line: 4
- content: something is off
`
	report := ParseReviewFindings(text)
	if !report.Parsed {
		t.Fatalf("the block form was not recognized: %+v", report)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
	}
	if report.Findings[0].Severity != SeverityUnspecified {
		t.Errorf("severity = %q, want %q", report.Findings[0].Severity, SeverityUnspecified)
	}
	if report.Findings[0].Path != "internal/foo.go" {
		t.Errorf("path = %q, want %q", report.Findings[0].Path, "internal/foo.go")
	}
}

// TestParseReviewFindings_LineRange: the simplify lens cites a range
// — the finding keeps the first line of it.
func TestParseReviewFindings_LineRange(t *testing.T) {
	report := ParseReviewFindings("- **low**: internal/foo.go:42-50 — the block is long")
	if len(report.Findings) != 1 {
		t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
	}
	if report.Findings[0].Path != "internal/foo.go" {
		t.Errorf("path = %q, want %q", report.Findings[0].Path, "internal/foo.go")
	}
	if report.Findings[0].Line != "42" {
		t.Errorf("line = %q, want %q", report.Findings[0].Line, "42")
	}
}

// TestParseReviewFindings_KeepsEveryFinding: no finding is dropped
// silently — every recognized finding reaches the report.
func TestParseReviewFindings_KeepsEveryFinding(t *testing.T) {
	var b strings.Builder
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		b.WriteString("- **" + sev + "**: internal/foo.go:1 — a " + sev + " finding\n")
	}
	report := ParseReviewFindings(b.String())
	if len(report.Findings) != 4 {
		t.Fatalf("parsed %d findings, want 4: %+v", len(report.Findings), report.Findings)
	}
	for i, sev := range []string{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow} {
		if report.Findings[i].Severity != sev {
			t.Errorf("finding %d severity = %q, want %q", i, report.Findings[i].Severity, sev)
		}
	}
}

// TestParseReviewFindings_ProseIsNotAFinding: a bullet that only
// looks like a finding is prose. The builtin prompt spells out its
// severity rubric in exactly this shape, and an agent that echoes it
// must not add findings to the verdict's count.
func TestParseReviewFindings_ProseIsNotAFinding(t *testing.T) {
	cases := []string{
		"- **critical**: would break in production or lose data. Must fix before merge.",
		"- warning: this may deadlock under load",
		"- **Note**: watch the nil map",
	}
	for _, text := range cases {
		report := ParseReviewFindings(text)
		if len(report.Findings) != 0 {
			t.Errorf("%q produced %d findings, want none: %+v", text, len(report.Findings), report.Findings)
		}
	}
}
