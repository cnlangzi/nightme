// Package feishu — F-39 tests for the result-reply dispatch + rendering helpers.
//
// Mirrors cc-connect's tests for buildReplyContent / containsMarkdown /
// countMarkdownTables / buildPostMdJSON / buildCardJSON.
package feishu

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// containsMarkdown
// ---------------------------------------------------------------------------

func TestContainsMarkdown_Fence(t *testing.T) {
	if !containsMarkdown("hello ``` world") {
		t.Error("triple-backtick should be detected as markdown")
	}
}

func TestContainsMarkdown_Bold(t *testing.T) {
	if !containsMarkdown("this **is** bold") {
		t.Error("** should be detected")
	}
}

func TestContainsMarkdown_Strike(t *testing.T) {
	if !containsMarkdown("~~strike~~") {
		t.Error("~~ should be detected")
	}
}

func TestContainsMarkdown_InlineCode(t *testing.T) {
	if !containsMarkdown("`inline`") {
		t.Error("` should be detected")
	}
}

func TestContainsMarkdown_UnorderedList(t *testing.T) {
	if !containsMarkdown("intro\n- item one\n- item two") {
		t.Error("`- ` (newline-prefixed) should be detected")
	}
}

func TestContainsMarkdown_OrderedList(t *testing.T) {
	if !containsMarkdown("intro\n1. first\n2. second") {
		t.Error("`1. ` (newline-prefixed) should be detected")
	}
}

func TestContainsMarkdown_Heading(t *testing.T) {
	if !containsMarkdown("intro\n# Title") {
		t.Error("`# ` (newline-prefixed) should be detected")
	}
}

func TestContainsMarkdown_HorizontalRule(t *testing.T) {
	if !containsMarkdown("above\n---\nbelow") {
		t.Error("`---` should be detected")
	}
}

func TestContainsMarkdown_PlainTextFalse(t *testing.T) {
	if containsMarkdown("just plain text without markers") {
		t.Error("plain text should not be detected as markdown")
	}
}

// ---------------------------------------------------------------------------
// countMarkdownTables
// ---------------------------------------------------------------------------

func TestCountMarkdownTables_None(t *testing.T) {
	if n := countMarkdownTables("plain text\nno tables here"); n != 0 {
		t.Errorf("expected 0 tables, got %d", n)
	}
}

func TestCountMarkdownTables_One(t *testing.T) {
	in := "intro\n| A | B |\n|---|---|\n| 1 | 2 |\noutro"
	if n := countMarkdownTables(in); n != 1 {
		t.Errorf("expected 1 table, got %d", n)
	}
}

func TestCountMarkdownTables_Two(t *testing.T) {
	in := "| A | B |\n|---|---|\n| 1 | 2 |\n\n| X | Y |\n|---|---|\n| a | b |"
	if n := countMarkdownTables(in); n != 2 {
		t.Errorf("expected 2 tables, got %d", n)
	}
}

func TestCountMarkdownTables_Six_Overflow(t *testing.T) {
	// Build 6 tables; expect result > limit so dispatch falls back to Post.
	var b strings.Builder
	for i := 0; i < 6; i++ {
		b.WriteString("| A | B |\n|---|---|\n| 1 | 2 |\n\n")
	}
	in := strings.TrimRight(b.String(), "\n")
	if n := countMarkdownTables(in); n != 6 {
		t.Errorf("expected 6 tables, got %d", n)
	}
}

func TestCountMarkdownTables_NoTrailingPipe(t *testing.T) {
	if n := countMarkdownTables("| only open pipe"); n != 0 {
		t.Errorf("single open-pipe line should not count as table, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// buildPostMdJSON
// ---------------------------------------------------------------------------

func TestBuildPostMdJSON_Shape(t *testing.T) {
	out, err := buildPostMdJSON("hello")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var envelope struct {
		ZHCn struct {
			Content [][]map[string]any `json:"content"`
		} `json:"zh_cn"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, out)
	}
	if len(envelope.ZHCn.Content) != 1 {
		t.Fatalf("expected 1 content group, got %d", len(envelope.ZHCn.Content))
	}
	if len(envelope.ZHCn.Content[0]) != 1 {
		t.Fatalf("expected 1 line, got %d", len(envelope.ZHCn.Content[0]))
	}
	got := envelope.ZHCn.Content[0][0]
	if got["tag"] != "md" {
		t.Errorf("tag should be 'md', got %v", got["tag"])
	}
	if got["text"] != "hello" {
		t.Errorf("text should be 'hello', got %v", got["text"])
	}
}

// TestBuildReceiptCard_FooterUsesDivTextNotElements locks the
// Feishu Card 2.0 schema for the F-46 footer. The pre-F-46 bug
// emitted `{"tag":"div", "elements":[<plain_text>, ...]}` and
// Feishu rejected the whole card with code 200621 ("unknown
// property, property: elements, path: ... -> [2](tag: div)").
// That broke every receipt SendCard / PATCH that carried a
// footer, so the receipt was never created or PATCHed and the
// user's response never appeared in chat. The corrected
// structure is one <div> per footer line, each holding a single
// nested <plain_text> in its `text` field.
//
// This test parses the JSON and walks to assert: (a) every <div>
// in the body has a `text` field, (b) no <div> has an `elements`
// array. A future refactor that re-introduces the `elements`
// array will fail this test before reaching production.
func TestBuildReceiptCard_FooterUsesDivTextNotElements(t *testing.T) {
	footer := []string{"🤖 claude · opus-4-5", "💰 ↓ 1.0k · ↻ 0 · ↑ 0 · 1.0k · $0.001"}
	body, err := buildReceiptCard(
		[]LogEntry{{Icon: "💬", Text: "hello"}},
		nil,
		footer,
	)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}

	var envelope struct {
		Body struct {
			Elements []map[string]any `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("parse card JSON: %v\nbody: %s", err, body)
	}

	// Find every <div> in the body and verify schema.
	divCount := 0
	for i, e := range envelope.Body.Elements {
		tag, _ := e["tag"].(string)
		if tag != "div" {
			continue
		}
		divCount++
		if _, hasText := e["text"]; !hasText {
			t.Errorf("body element %d: <div> missing required `text` field\nelement: %#v\nbody: %s", i, e, body)
		}
		if _, hasElements := e["elements"]; hasElements {
			t.Errorf("body element %d: <div> has invalid `elements` property (Feishu rejects with 200621)\nelement: %#v\nbody: %s", i, e, body)
		}
	}
	if divCount != len(footer) {
		t.Errorf("div count = %d, want %d (one <div> per footer line)", divCount, len(footer))
	}

	// Footer <hr> must be present.
	var foundHr bool
	for _, e := range envelope.Body.Elements {
		if tag, _ := e["tag"].(string); tag == "hr" {
			foundHr = true
			break
		}
	}
	if !foundHr {
		t.Errorf("body missing <hr> divider\nbody: %s", body)
	}
}

// ---------------------------------------------------------------------------
// buildResultCardJSON
// ---------------------------------------------------------------------------

func TestBuildResultCardJSON_SingleDiv(t *testing.T) {
	body, err := buildResultCardJSON("hello world", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var envelope struct {
		Schema string `json:"schema"`
		Body   struct {
			Elements []map[string]any `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if envelope.Schema != "2.0" {
		t.Errorf("expected schema 2.0, got %q", envelope.Schema)
	}
	if len(envelope.Body.Elements) != 1 {
		t.Errorf("expected 1 element for short content, got %d", len(envelope.Body.Elements))
	}
	if e := envelope.Body.Elements[0]; e["tag"] != "markdown" {
		t.Errorf("tag should be markdown, got %v", e["tag"])
	}
}

func TestBuildResultCardJSON_MultiDiv(t *testing.T) {
	// 2500 chars × 'a' (ASCII) forces ≥3 divs at divTextCharLimit=1000.
	in := strings.Repeat("a", 2500)
	body, err := buildResultCardJSON(in, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var envelope struct {
		Body struct {
			Elements []map[string]any `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(envelope.Body.Elements) < 3 {
		t.Errorf("expected ≥ 3 elements for 2500-char input, got %d", len(envelope.Body.Elements))
	}
	for i, e := range envelope.Body.Elements {
		if e["tag"] != "markdown" {
			t.Errorf("element %d: tag should be markdown, got %v", i, e["tag"])
		}
	}
}

func TestBuildResultCardJSON_PreservesCodeBlock(t *testing.T) {
	// Code block must stay intact across splitMarkdownForDivs.
	in := "intro\n\n```go\nfunc x() { return 1 }\n```\n\noutro"
	body, err := buildResultCardJSON(in, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var envelope struct {
		Body struct {
			Elements []map[string]any `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	allContent := ""
	for _, e := range envelope.Body.Elements {
		if c, ok := e["content"].(string); ok {
			allContent += c + "\n"
		}
	}
	if !strings.Contains(allContent, "```go\nfunc x() { return 1 }\n```") {
		t.Errorf("code block must stay intact in any element, got:\n%s", allContent)
	}
}

// ---------------------------------------------------------------------------
// buildResultPayload dispatch
// ---------------------------------------------------------------------------

func TestBuildResultPayload_NoMarkdown_UsesText(t *testing.T) {
	msgType, body, err := buildResultPayload("plain text without markers", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if msgType != "text" {
		t.Errorf("expected MsgTypeText, got %q", msgType)
	}
	if !strings.Contains(body, `"text":"plain text without markers"`) {
		t.Errorf("expected text body, got %q", body)
	}
}

func TestBuildResultPayload_LotsOfTables_UsesPost(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 6; i++ {
		b.WriteString("| A | B |\n|---|---|\n| 1 | 2 |\n\n")
	}
	sanitized := SanitizeCardMarkdown(strings.TrimRight(b.String(), "\n"))
	msgType, body, err := buildResultPayload(sanitized, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if msgType != "post" {
		t.Errorf("expected MsgTypePost for >5 tables, got %q", msgType)
	}
	if !strings.Contains(body, `"tag":"md"`) {
		t.Errorf("post body should use md tag, got %q", body)
	}
}

func TestBuildResultPayload_Default_UsesInteractiveCard(t *testing.T) {
	// Has markdown (```) but few tables.
	in := "intro\n\n```go\nfunc x() {}\n```\n\noutro"
	sanitized := SanitizeCardMarkdown(in)
	msgType, body, err := buildResultPayload(sanitized, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if msgType != "interactive" {
		t.Errorf("expected MsgTypeInteractive, got %q", msgType)
	}
	if !strings.Contains(body, `"schema":"2.0"`) {
		t.Errorf("expected Card 2.0 envelope, got %q", body)
	}
}

// ---------------------------------------------------------------------------
// truncateRunes
// ---------------------------------------------------------------------------

func TestTruncateForLog_ASCIIShortUnchanged(t *testing.T) {
	if got := truncateForLog("hello", 100); got != "hello" {
		t.Errorf("ASCII short should pass through, got %q", got)
	}
}

func TestTruncateForLog_ASCIIOverTruncatedWithEllipsis(t *testing.T) {
	got := truncateForLog("hello world", 6)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected trailing ellipsis, got %q", got)
	}
	// 5 runes + ellipsis.
	if r := []rune(got); len(r) != 6 {
		t.Errorf("expected 6 runes (5 + …), got %d: %q", len(r), got)
	}
}

func TestTruncateForLog_CJKBoundary(t *testing.T) {
	// 5 CJK runes; limit 4 → 3 runes + ellipsis (4 runes total).
	in := "你好世界你好"
	got := truncateForLog(in, 4)
	if r := []rune(got); len(r) != 4 {
		t.Errorf("expected 4 runes, got %d: %q", len(r), got)
	}
}

func TestTruncateForLog_EdgeCaseOne(t *testing.T) {
	if got := truncateForLog("hello", 1); got != "…" {
		t.Errorf("maxRunes=1 should return single ellipsis, got %q", got)
	}
}

func TestTruncateForLog_EdgeCaseZero(t *testing.T) {
	if got := truncateForLog("hello", 0); got != "" {
		t.Errorf("maxRunes=0 should return empty, got %q", got)
	}
}

func TestTruncateForLog_UTF8Valid(t *testing.T) {
	// Verify result is valid UTF-8 (no mid-rune slicing).
	got := truncateForLog("中文mixed ascii 你好呀", 8)
	for _, r := range got {
		if r == 0xFFFD {
			t.Errorf("invalid UTF-8 rune in result: %q", got)
		}
	}
}
