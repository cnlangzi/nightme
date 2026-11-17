package agent

// The shapes a review group answers in when it drifts from the shape
// the prompt asked for: the block form without its bullet markers,
// the field names the agent used instead of the prompt's, and a JSON
// payload.
//
// A group whose output the parser cannot read contributes nothing to
// the verdict's counts — so a shape it does not read is a shape the
// review loses findings on.

import (
	"strings"
	"testing"
)

// TestParseReviewFindings_RelaxedBlockForm: the per-finding block
// without the bullet markers, with a multi-line content field.
func TestParseReviewFindings_RelaxedBlockForm(t *testing.T) {
	text := `path: src/tunnel/ws.rs
start_line: 658
end_line: 700
category: bug
severity: high
content: read_ws_accept_response reads up to 512 bytes per syscall and discards
  everything past CRLFCRLF, so any bytes the gateway sends together with the
  101 are silently destroyed.
`

	report := ParseReviewFindings(text)
	if !report.Parsed {
		t.Fatalf("the relaxed block form was not recognized: %+v", report)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
	}
	finding := report.Findings[0]
	if finding.Path != "src/tunnel/ws.rs" {
		t.Errorf("path = %q, want %q", finding.Path, "src/tunnel/ws.rs")
	}
	if finding.Line != "658" {
		t.Errorf("line = %q, want %q", finding.Line, "658")
	}
	if finding.Severity != SeverityHigh {
		t.Errorf("severity = %q, want %q", finding.Severity, SeverityHigh)
	}
	if finding.Category != "bug" {
		t.Errorf("category = %q, want %q", finding.Category, "bug")
	}
	want := "read_ws_accept_response reads up to 512 bytes per syscall and discards " +
		"everything past CRLFCRLF, so any bytes the gateway sends together with the " +
		"101 are silently destroyed."
	if finding.Content != want {
		t.Errorf("content = %q, want %q", finding.Content, want)
	}
}

// TestParseReviewFindings_RelaxedBlockKeepsEveryBlock: a relaxed
// block form carries several findings — the parser reads them all.
func TestParseReviewFindings_RelaxedBlockKeepsEveryBlock(t *testing.T) {
	text := `path: a.go
start_line: 1
severity: high
content: the first problem
path: b.go
start_line: 2
severity: low
content: the second problem
`

	report := ParseReviewFindings(text)
	if len(report.Findings) != 2 {
		t.Fatalf("parsed %d findings, want 2: %+v", len(report.Findings), report.Findings)
	}
	if report.Findings[0].Path != "a.go" || report.Findings[1].Path != "b.go" {
		t.Errorf("the blocks were not read one by one: %+v", report.Findings)
	}
	if report.Findings[1].Severity != SeverityLow {
		t.Errorf("second severity = %q, want %q", report.Findings[1].Severity, SeverityLow)
	}
}

// TestParseReviewFindings_FieldAliases: the field names an agent
// reaches for instead of the prompt's are read as the fields they
// stand for.
func TestParseReviewFindings_FieldAliases(t *testing.T) {
	text := `- file: a.go
- line: 12
- level: blocker
- message: the lock is never released
`

	report := ParseReviewFindings(text)
	if !report.Parsed {
		t.Fatalf("the aliased block was not recognized: %+v", report)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
	}
	finding := report.Findings[0]
	if finding.Path != "a.go" {
		t.Errorf("path = %q, want %q", finding.Path, "a.go")
	}
	if finding.Line != "12" {
		t.Errorf("line = %q, want %q", finding.Line, "12")
	}
	if finding.Severity != SeverityCritical {
		t.Errorf("severity = %q, want %q", finding.Severity, SeverityCritical)
	}
	if finding.Content != "the lock is never released" {
		t.Errorf("content = %q, want the message", finding.Content)
	}
}

// TestParseReviewFindings_JSONPayloads: a group that answers with a
// JSON payload still has findings — in every shape the payload takes.
func TestParseReviewFindings_JSONPayloads(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{
			name: "a fenced json block",
			text: "```json\n" +
				`[{"path": "a.go", "start_line": 12, "severity": "high", "content": "boom"}]` +
				"\n```",
		},
		{
			name: "a bare json array",
			text: `[{"path": "a.go", "start_line": 12, "severity": "high", "content": "boom"}]`,
		},
		{
			name: "an object carrying a findings array",
			text: `{"findings": [{"path": "a.go", "start_line": 12, "severity": "high", "content": "boom"}]}`,
		},
		{
			name: "json with trailing commas",
			text: `[
				{"path": "a.go", "start_line": 12, "severity": "high", "content": "boom",},
			]`,
		},
		{
			name: "json with single-quoted strings",
			text: `[{'path': 'a.go', 'start_line': 12, 'severity': 'high', 'content': 'boom'}]`,
		},
		{
			name: "json with unquoted keys",
			text: `[{path: "a.go", start_line: 12, severity: "high", content: "boom"}]`,
		},
		{
			name: "json with unquoted keys across lines",
			text: `[
				{
					path: "a.go",
					start_line: 12,
					severity: "high",
					content: "boom"
				}
			]`,
		},
		{
			name: "json embedded in prose",
			text: "Here are the findings:\n" +
				`[{"path": "a.go", "start_line": 12, "severity": "high", "content": "boom"}]` +
				"\nThat is all.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := ParseReviewFindings(tc.text)
			if !report.Parsed {
				t.Fatalf("the json payload was not recognized: %+v", report)
			}
			if len(report.Findings) != 1 {
				t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
			}
			finding := report.Findings[0]
			if finding.Path != "a.go" {
				t.Errorf("path = %q, want %q", finding.Path, "a.go")
			}
			if finding.Line != "12" {
				t.Errorf("line = %q, want %q", finding.Line, "12")
			}
			if finding.Severity != SeverityHigh {
				t.Errorf("severity = %q, want %q", finding.Severity, SeverityHigh)
			}
			if finding.Content != "boom" {
				t.Errorf("content = %q, want %q", finding.Content, "boom")
			}
		})
	}
}

// TestParseReviewFindings_JSONCarriesSeveralFindings: a payload with
// more than one finding contributes all of them.
func TestParseReviewFindings_JSONCarriesSeveralFindings(t *testing.T) {
	text := `[{"path": "a.go", "line": 1, "severity": "critical", "content": "first"},
	         {"path": "b.go", "line": 2, "severity": "low", "content": "second"}]`

	report := ParseReviewFindings(text)
	if len(report.Findings) != 2 {
		t.Fatalf("parsed %d findings, want 2: %+v", len(report.Findings), report.Findings)
	}
	if report.Findings[0].Severity != SeverityCritical {
		t.Errorf("first severity = %q, want %q", report.Findings[0].Severity, SeverityCritical)
	}
	if report.Findings[1].Path != "b.go" {
		t.Errorf("second path = %q, want %q", report.Findings[1].Path, "b.go")
	}
}

// TestParseReviewFindings_NoFindingsPhrasing: a group that says it
// found nothing is parsed — it is not an unreadable group.
func TestParseReviewFindings_NoFindingsPhrasing(t *testing.T) {
	cases := []string{
		"No findings.",
		"No findings",
		"No blockers; nothing material to flag.",
		"I found no problems.",
		"No issues found.",
	}
	for _, text := range cases {
		report := ParseReviewFindings(text)
		if !report.Parsed {
			t.Errorf("%q was reported as unparsed: %+v", text, report)
		}
		if len(report.Findings) != 0 {
			t.Errorf("%q produced findings: %+v", text, report.Findings)
		}
	}
}

// TestParseReviewFindings_BulletlessPathIsProse: a "path:" line that
// names no field after it is a sentence, not a block.
func TestParseReviewFindings_BulletlessPathIsProse(t *testing.T) {
	text := "path: the file layout is unclear\nthe reader has to guess\n"

	report := ParseReviewFindings(text)
	if len(report.Findings) != 0 {
		t.Errorf("prose produced findings: %+v", report.Findings)
	}
}

// TestParseReviewFindings_ProseWithABraceIsNotAFinding: the JSON
// salvage reads a payload, not a sentence that mentions one.
func TestParseReviewFindings_ProseWithABraceIsNotAFinding(t *testing.T) {
	text := "the map literal {\"a\": 1} is wrong"

	report := ParseReviewFindings(text)
	if len(report.Findings) != 0 {
		t.Errorf("prose produced findings: %+v", report.Findings)
	}
}

// TestParseReviewFindings_KeepsTheStrictForm: the shape the prompts
// ask for is still read — the relaxed scanner did not replace it.
func TestParseReviewFindings_KeepsTheStrictForm(t *testing.T) {
	text := `## Findings
- path: internal/foo.go
- start_line: 42
- end_line: 50
- category: bug
- severity: high
- content: the retry swallows the error
`

	report := ParseReviewFindings(text)
	if len(report.Findings) != 1 {
		t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
	}
	finding := report.Findings[0]
	if finding.Path != "internal/foo.go" || finding.Line != "42" {
		t.Errorf("the strict block was not read: %+v", finding)
	}
	if finding.Content != "the retry swallows the error" {
		t.Errorf("content = %q, want the finding's text", finding.Content)
	}
}

// TestParseReviewFindings_ContentKeepsItsOwnLines: a content field
// whose value spans lines is joined with a single space — the digest
// renders it in one cell.
func TestParseReviewFindings_ContentKeepsItsOwnLines(t *testing.T) {
	text := "- path: a.go\n- content: first line\n  second line\n"

	report := ParseReviewFindings(text)
	if len(report.Findings) != 1 {
		t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
	}
	if report.Findings[0].Content != "first line second line" {
		t.Errorf("content = %q, want the two lines joined", report.Findings[0].Content)
	}
	if strings.Contains(report.Findings[0].Content, "\n") {
		t.Errorf("the content still carries a line break: %q", report.Findings[0].Content)
	}
}

// TestParseReviewFindings_BlankLineAfterTheOpener: the opener and
// its fields are one unit — a blank line between them does not end
// the block.
func TestParseReviewFindings_BlankLineAfterTheOpener(t *testing.T) {
	text := `path: a.go

start_line: 12
severity: high
content: the problem
`

	report := ParseReviewFindings(text)
	if len(report.Findings) != 1 {
		t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
	}
	finding := report.Findings[0]
	if finding.Path != "a.go" || finding.Line != "12" {
		t.Errorf("the block was not read: %+v", finding)
	}
	if finding.Severity != SeverityHigh {
		t.Errorf("severity = %q, want %q", finding.Severity, SeverityHigh)
	}
}

// TestParseReviewFindings_NothingFoundMustBeTheWholeLine: a group
// that mentions "no findings" in passing and reports real findings
// elsewhere must not be read as a group that found nothing — that
// would drop its findings from the counts and hide the group from
// the re-emit.
func TestParseReviewFindings_NothingFoundMustBeTheWholeLine(t *testing.T) {
	cases := []string{
		"No findings in the diff.\n\nHowever, the retry loop drops the error.",
		"There were no findings in the retry loop.",
		"No findings — the retry loop drops the error.",
	}
	for _, text := range cases {
		report := ParseReviewFindings(text)
		if report.Parsed {
			t.Errorf("%q was read as a group that found nothing: %+v", text, report)
		}
	}
}

// TestParseReviewFindings_QuotedPathIsAPath: the path an agent quoted
// is still a path — the quotes come off and the block is read.
func TestParseReviewFindings_QuotedPathIsAPath(t *testing.T) {
	text := "- path: \"internal/foo.go\"\n- start_line: 12\n- severity: high\n- content: bad\n"

	report := ParseReviewFindings(text)
	if len(report.Findings) != 1 {
		t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
	}
	if report.Findings[0].Path != "internal/foo.go" {
		t.Errorf("path = %q, want %q", report.Findings[0].Path, "internal/foo.go")
	}
	if report.Findings[0].Line != "12" {
		t.Errorf("line = %q, want %q", report.Findings[0].Line, "12")
	}
}

// TestParseReviewFindings_QuotedJSONIsNotABlock: the keys of a JSON
// payload are never bulleted, so a bulleted path is a block and an
// unbulleted one belongs to the JSON salvage.
func TestParseReviewFindings_QuotedJSONIsNotABlock(t *testing.T) {
	text := "{\n  path: \"a.go\",\n  line: 12,\n  severity: \"high\"\n}\n"

	report := ParseReviewFindings(text)
	if len(report.Findings) != 1 {
		t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
	}
	if report.Findings[0].Path != "a.go" || report.Findings[0].Severity != SeverityHigh {
		t.Errorf("the json payload was not read: %+v", report.Findings[0])
	}
}

// TestParseReviewFindings_JSONQuotedInProseIsNotAFinding: a JSON
// object the agent quotes in a sentence is prose — a config file the
// agent talks about is not a finding.
func TestParseReviewFindings_JSONQuotedInProseIsNotAFinding(t *testing.T) {
	text := "The config file {\"path\": \"a.go\", \"content\": \"x\"} is unrelated."

	report := ParseReviewFindings(text)
	if len(report.Findings) != 0 {
		t.Errorf("a quoted config produced findings: %+v", report.Findings)
	}
}

// TestParseReviewFindings_AStrayBraceDoesNotSwallowThePayload: a
// brace in the prose before the payload must not hide the payload
// that follows it.
func TestParseReviewFindings_AStrayBraceDoesNotSwallowThePayload(t *testing.T) {
	text := "the { placeholder\n{\"path\":\"a.go\",\"line\":1,\"severity\":\"high\",\"content\":\"boom\"}"

	report := ParseReviewFindings(text)
	if len(report.Findings) != 1 {
		t.Fatalf("parsed %d findings, want 1: %+v", len(report.Findings), report.Findings)
	}
	if report.Findings[0].Path != "a.go" || report.Findings[0].Severity != SeverityHigh {
		t.Errorf("the payload was not read: %+v", report.Findings[0])
	}
}
