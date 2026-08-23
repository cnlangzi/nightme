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

// TestBuildReceiptCard_FooterUsesMarkdownFontGrey locks the
// Feishu CardKit lark_md footer rendering pattern (matches
// openclaw-lark src/card/builder.ts::buildFooter). The footer is
// rendered as:
//   1. <hr> divider element
//   2. one <markdown> element whose content uses inline
//      <font color='grey'>...</font> tags per line
//
// Previous attempts (gone through three iterations):
//   - <div> wrapping <plain_text> with text_color="#999999" —
//     Feishu rejected with "unknown property, property: elements"
//     (code 200621) AND "invalid color: #999999" (code 230099).
//   - <div> wrapping <plain_text> with text_color="grey-500" —
//     Feishu plain_text text_color only accepts the grey-XXX
//     numbered family in some contexts; still flaky.
//   - this final approach: single <markdown> element with inline
//     <font color='grey'> tags per line. Matches openclaw-lark's
//     reference; verified against Feishu CardKit lark_md spec
//     (named colors only — no hex).
func TestBuildReceiptCard_FooterUsesMarkdownFontGrey(t *testing.T) {
	footer := []string{"🤖 claude · opus-4-5", "💰 ↓ 1.0k · ↻ 0 · ↑ 0 · 1.0k · $0.001"}
	body, _, err := buildReceiptCard(
		[]LogEntry{{Icon: "💬", Text: "hello"}},
		nil,
		footer,
		nil,
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

	// Expect 3 elements: [entry markdown, hr, footer markdown].
	// The entry "hello" is the first markdown (the body content);
	// then the footer section: <hr> + <markdown> with <font> wrappers.
	if len(envelope.Body.Elements) != 3 {
		t.Fatalf("body element count = %d, want 3 ([entry-markdown, hr, footer-markdown]); body: %s", len(envelope.Body.Elements), body)
	}

	// First element: <markdown> with the entry text.
	entry := envelope.Body.Elements[0]
	if tag, _ := entry["tag"].(string); tag != "markdown" {
		t.Errorf("body[0].tag = %q, want %q\nbody: %s", tag, "markdown", body)
	}
	if content, _ := entry["content"].(string); content != "💬 hello" {
		t.Errorf("body[0].content = %q, want %q\nbody: %s", content, "💬 hello", body)
	}

	// Second element: <hr>.
	if tag, _ := envelope.Body.Elements[1]["tag"].(string); tag != "hr" {
		t.Errorf("body[1].tag = %q, want %q\nbody: %s", tag, "hr", body)
	}

	// Third element: <markdown> with <font color='grey'> wrappers.
	footerMd := envelope.Body.Elements[2]
	if tag, _ := footerMd["tag"].(string); tag != "markdown" {
		t.Errorf("body[2].tag = %q, want %q\nbody: %s", tag, "markdown", body)
	}
	content, _ := footerMd["content"].(string)
	for _, line := range footer {
		want := "<font color='grey'>" + line + "</font>"
		if !strings.Contains(content, want) {
			t.Errorf("markdown content missing %q\ncontent: %q\nbody: %s", want, content, body)
		}
	}
	// No <div> at all — plain_text is gone.
	for i, e := range envelope.Body.Elements {
		if tag, _ := e["tag"].(string); tag == "div" {
			t.Errorf("body element %d: should NOT be <div> (footer uses <markdown>)\nelement: %#v\nbody: %s", i, e, body)
		}
		if tag, _ := e["tag"].(string); tag == "plain_text" {
			t.Errorf("body element %d: should NOT be <plain_text> (footer uses <markdown>)\nelement: %#v\nbody: %s", i, e, body)
		}
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
// TestBuildResultCardJSON_BlankLineBeforeFooter: 2026-08-24 user
// feedback "OutResult 的内容和 footer 中间应该增加一个空行, 方便
// 阅读." — verify the rendered card inserts an empty <markdown>
// element between the body content and the footer block so the
// StatusBar box doesn't sit flush against the body text.
func TestBuildResultCardJSON_BlankLineBeforeFooter(t *testing.T) {
	body, err := buildResultCardJSON("hello world", []string{
		"🤖: claude · opus-4-5",
		"📁: code/nightme",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var envelope struct {
		Body struct {
			Elements []map[string]any `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	// Expect 4 elements: body markdown, blank markdown, hr + footer markdown.
	if len(envelope.Body.Elements) != 4 {
		t.Fatalf("expected 4 elements (body + blank + hr + footer), got %d: %+v",
			len(envelope.Body.Elements), envelope.Body.Elements)
	}
	if e := envelope.Body.Elements[0]; e["tag"] != "markdown" || e["content"] != "hello world" {
		t.Errorf("elements[0] should be body markdown, got %+v", e)
	}
	if e := envelope.Body.Elements[1]; e["tag"] != "markdown" {
		t.Errorf("elements[1] should be the blank-line markdown spacer, got tag=%v", e["tag"])
	}
	if e := envelope.Body.Elements[2]; e["tag"] != "hr" {
		t.Errorf("elements[2] should be the hr divider, got tag=%v", e["tag"])
	}
	if e := envelope.Body.Elements[3]; e["tag"] != "markdown" {
		t.Errorf("elements[3] should be the footer markdown, got tag=%v", e["tag"])
	}
}

// TestBuildResultCardJSON_NoBlankLineWhenFooterEmpty: when there's
// no footer (footerLines is nil/empty), no blank-line spacer is
// inserted either — the blank line is a body/footer separator, not
// unconditional.
func TestBuildResultCardJSON_NoBlankLineWhenFooterEmpty(t *testing.T) {
	body, err := buildResultCardJSON("hello world", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var envelope struct {
		Body struct {
			Elements []map[string]any `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if len(envelope.Body.Elements) != 1 {
		t.Errorf("expected 1 element when footer is empty, got %d: %+v",
			len(envelope.Body.Elements), envelope.Body.Elements)
	}
}

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

// TestCardFooterElements_AllLinesWrapped pins the invariant:
// every footer line — including the workspace line that
// carries the markdown-link PR tail `[#N](url)` as its last
// segment — is wrapped in <font color='grey'>…</font>.
// Empirically verified on current Feishu (2026-08, see
// pr_render_compare_test.go): lark_md correctly renders
// `[#N](url)` inside <font> — `#N` surfaces as a clickable
// blue anchor while the rest of the workspace row stays
// grey. The earlier `](` bypass heuristic was solving a
// problem that doesn't exist in current lark_md and was
// removed.
//
// Regression guard: if a future change re-adds the `](`
// heuristic, the workspace row will lose its grey colour
// to "rescue" whatever was inside the `<font>` — the
// failure mode this test catches by asserting the grey
// wrap survives intact even when the footer line contains
// `](` (from the markdown link syntax). The heuristic was
// never necessary; if a future lark_md regresses, prefer a
// dedicated card element for the link rather than blanket-
// unwrapping footer rows.
func TestCardFooterElements_AllLinesWrapped(t *testing.T) {
	elems := cardFooterElements([]string{
		"🤖: claude · opus-4-5",
		"💰:「 12.3k / 8.2k / 1.5k · $0.087 」",
		"📁: code/nightme · ⎇ fix-x · [#42](https://example/pr/42)",
	})
	if len(elems) != 2 {
		t.Fatalf("got %d elements, want 2 (hr + markdown)", len(elems))
	}
	if elems[0]["tag"] != "hr" {
		t.Errorf("elems[0].tag = %v, want hr", elems[0]["tag"])
	}
	if elems[1]["tag"] != "markdown" {
		t.Errorf("elems[1].tag = %v, want markdown", elems[1]["tag"])
	}
	content, _ := elems[1]["content"].(string)
	for _, want := range []string{
		"<font color='grey'>🤖: claude · opus-4-5</font>",
		"<font color='grey'>💰:「 12.3k / 8.2k / 1.5k · $0.087 」</font>",
		"<font color='grey'>📁: code/nightme · ⎇ fix-x · [#42](https://example/pr/42)</font>",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing wrapped %q in:\n%s", want, content)
		}
	}
	// The markdown link syntax MUST survive into the markdown
	// content — guards against a future change accidentally
	// HTML-escaping `[` / `]` / `(` / `)` and breaking the link.
	if !strings.Contains(content, "[#42](https://example/pr/42)") {
		t.Errorf("PR link syntax mangled in markdown content:\n%s", content)
	}
	// And no raw (un-wrapped) line should appear in the content.
	for _, line := range []string{
		"🤖: claude · opus-4-5",
		"💰:「 12.3k / 8.2k / 1.5k · $0.087 」",
		"📁: code/nightme · ⎇ fix-x · [#42](https://example/pr/42)",
	} {
		// Each line should NOT appear outside of a <font> wrap.
		// We check by stripping all <font>…</font> spans and
		// asserting the bare line doesn't survive.
		stripped := strings.ReplaceAll(content, "<font color='grey'>"+line+"</font>", "")
		if strings.Contains(stripped, line) {
			t.Errorf("line %q appears un-wrapped in:\n%s", line, content)
		}
	}
}

// TestCardFooterElements_NoLinkAllWrapped is the minimal
// sanity check: footer lines without any markdown link syntax
// are all wrapped in <font color='grey'> (no heuristic to
// trip; this is the post-refactor baseline).
func TestCardFooterElements_NoLinkAllWrapped(t *testing.T) {
	elems := cardFooterElements([]string{
		"🤖: claude",
		"💰:「 1k / 2k 」",
		"📁: code/nightme · ⎇ main",
	})
	content, _ := elems[1]["content"].(string)
	for _, want := range []string{
		"<font color='grey'>🤖: claude</font>",
		"<font color='grey'>💰:「 1k / 2k 」</font>",
		"<font color='grey'>📁: code/nightme · ⎇ main</font>",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing wrapped %q in:\n%s", want, content)
		}
	}
}
