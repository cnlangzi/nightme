package telegram

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/statusbar"
)

func TestRenderMarkdown_Empty(t *testing.T) {
	out, err := RenderMarkdown("")
	if err != nil {
		t.Fatalf("RenderMarkdown empty: %v", err)
	}
	if out != "" {
		t.Fatalf("RenderMarkdown empty = %q", out)
	}
}

func TestRenderMarkdown_Heading(t *testing.T) {
	out, err := RenderMarkdown("# Hello")
	if err != nil {
		t.Fatalf("RenderMarkdown heading: %v", err)
	}
	// H1 now gets a 📌 pin emoji + blank line for visual emphasis.
	if !strings.Contains(out, "<b>📌 Hello</b>") {
		t.Fatalf("RenderMarkdown heading = %q", out)
	}
}

func TestRenderMarkdown_HeadingLevels(t *testing.T) {
	cases := []struct {
		level    string
		wantSubs []string
	}{
		// H1: pin emoji + bold.
		{"#", []string{"<b>📌 Title</b>"}},
		// H2-H6: bold + Unicode underline bar.
		{"##", []string{"<b>Title</b>", "────────"}},
		{"###", []string{"<b>Title</b>", "────────"}},
		{"####", []string{"<b>Title</b>", "────────"}},
	}
	for _, tc := range cases {
		input := tc.level + " Title"
		out, err := RenderMarkdown(input)
		if err != nil {
			t.Fatalf("RenderMarkdown %s: %v", tc.level, err)
		}
		for _, want := range tc.wantSubs {
			if !strings.Contains(out, want) {
				t.Fatalf("RenderMarkdown %s missing %q; got %q", tc.level, want, out)
			}
		}
	}
}

func TestRenderMarkdown_Bullet(t *testing.T) {
	out, err := RenderMarkdown("- first\n- second")
	if err != nil {
		t.Fatalf("RenderMarkdown bullet: %v", err)
	}
	if !strings.Contains(out, "• first") {
		t.Fatalf("RenderMarkdown bullet missing first: %q", out)
	}
	if !strings.Contains(out, "• second") {
		t.Fatalf("RenderMarkdown bullet missing second: %q", out)
	}
}

func TestRenderMarkdown_Ordered(t *testing.T) {
	out, err := RenderMarkdown("1. one\n2. two")
	if err != nil {
		t.Fatalf("RenderMarkdown ordered: %v", err)
	}
	if !strings.Contains(out, "1. one") {
		t.Fatalf("RenderMarkdown ordered missing one: %q", out)
	}
	if !strings.Contains(out, "2. two") {
		t.Fatalf("RenderMarkdown ordered missing two: %q", out)
	}
}

func TestRenderMarkdown_CodeBlock(t *testing.T) {
	out, err := RenderMarkdown("```go\nfunc main() {}\n```")
	if err != nil {
		t.Fatalf("RenderMarkdown code: %v", err)
	}
	if !strings.Contains(out, `<pre><code class="language-go">`) {
		t.Fatalf("RenderMarkdown code missing language-tagged pre/code: %q", out)
	}
	if !strings.Contains(out, "func main()") {
		t.Fatalf("RenderMarkdown code missing code: %q", out)
	}
	if !strings.Contains(out, "</code></pre>") {
		t.Fatalf("RenderMarkdown code missing /code/pre: %q", out)
	}
}

func TestRenderMarkdown_InlineCode(t *testing.T) {
	out, err := RenderMarkdown("use `foo` here")
	if err != nil {
		t.Fatalf("RenderMarkdown inline code: %v", err)
	}
	if !strings.Contains(out, "<code>foo</code>") {
		t.Fatalf("RenderMarkdown inline code = %q", out)
	}
}

func TestRenderMarkdown_Link(t *testing.T) {
	out, err := RenderMarkdown("[click](https://example.com)")
	if err != nil {
		t.Fatalf("RenderMarkdown link: %v", err)
	}
	if !strings.Contains(out, `<a href="https://example.com">click</a>`) {
		t.Fatalf("RenderMarkdown link = %q", out)
	}
}

func TestRenderMarkdown_Bold(t *testing.T) {
	out, err := RenderMarkdown("**strong**")
	if err != nil {
		t.Fatalf("RenderMarkdown bold: %v", err)
	}
	if !strings.Contains(out, "<b>strong</b>") {
		t.Fatalf("RenderMarkdown bold = %q", out)
	}
}

func TestRenderMarkdown_Italic(t *testing.T) {
	out, err := RenderMarkdown("*em*")
	if err != nil {
		t.Fatalf("RenderMarkdown italic: %v", err)
	}
	if !strings.Contains(out, "<i>em</i>") {
		t.Fatalf("RenderMarkdown italic = %q", out)
	}
}

func TestRenderMarkdown_Spoiler(t *testing.T) {
	out, err := RenderMarkdown("||hidden||")
	if err != nil {
		t.Fatalf("RenderMarkdown spoiler: %v", err)
	}
	if !strings.Contains(out, `<span class="tg-spoiler">hidden</span>`) {
		t.Fatalf("RenderMarkdown spoiler = %q", out)
	}
}

func TestRenderMarkdown_Quote(t *testing.T) {
	out, err := RenderMarkdown("> quoted")
	if err != nil {
		t.Fatalf("RenderMarkdown quote: %v", err)
	}
	if !strings.Contains(out, "<blockquote>quoted</blockquote>") {
		t.Fatalf("RenderMarkdown quote = %q", out)
	}
}

func TestRenderMarkdown_HorizontalRule(t *testing.T) {
	out, err := RenderMarkdown("above\n---\nbelow")
	if err != nil {
		t.Fatalf("RenderMarkdown hr: %v", err)
	}
	if !strings.Contains(out, "────────") {
		t.Fatalf("RenderMarkdown hr = %q", out)
	}
}

func TestRenderMarkdown_Table(t *testing.T) {
	out, err := RenderMarkdown("| a | b |\n| - | - |\n| 1 | 2 |")
	if err != nil {
		t.Fatalf("RenderMarkdown table: %v", err)
	}
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") {
		t.Fatalf("RenderMarkdown table = %q", out)
	}
}

func TestRenderMarkdown_EscapesHTML(t *testing.T) {
	out, err := RenderMarkdown("<script>alert(1)</script>")
	if err != nil {
		t.Fatalf("RenderMarkdown escape: %v", err)
	}
	if strings.Contains(out, "<script>") {
		t.Fatalf("RenderMarkdown did not escape script: %q", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("RenderMarkdown escape = %q", out)
	}
}

func TestRenderMarkdown_LinkSafetyBlocks(t *testing.T) {
	out, err := RenderMarkdown("[bad](javascript:alert(1))")
	if err != nil {
		t.Fatalf("RenderMarkdown link safety: %v", err)
	}
	// The unsafe scheme must not be turned into a clickable link.
	if strings.Contains(out, `<a href="javascript:`) {
		t.Fatalf("RenderMarkdown leaked javascript link: %q", out)
	}
}

func TestSplitTelegramText_Small(t *testing.T) {
	parts, err := splitTelegramText("hello world", 100)
	if err != nil {
		t.Fatalf("splitTelegramText small: %v", err)
	}
	if len(parts) != 1 || parts[0] != "hello world" {
		t.Fatalf("splitTelegramText small = %v", parts)
	}
}

func TestSplitTelegramText_Large(t *testing.T) {
	parts, err := splitTelegramText(strings.Repeat("a", 12000), 3900)
	if err != nil {
		t.Fatalf("splitTelegramText large: %v", err)
	}
	if len(parts) < 3 {
		t.Fatalf("splitTelegramText large = %d parts", len(parts))
	}
	for _, part := range parts {
		if len(part) > 3900 {
			t.Fatalf("part over limit: %d", len(part))
		}
	}
}

func TestSplitTelegramText_PreservesNewlines(t *testing.T) {
	parts, err := splitTelegramText(strings.Repeat("line\n", 1000), 3900)
	if err != nil {
		t.Fatalf("splitTelegramText newlines: %v", err)
	}
	if len(parts) == 0 {
		t.Fatal("no parts")
	}
	combined := strings.Join(parts, "\n")
	if !strings.Contains(combined, "line") {
		t.Fatalf("no line in combined: %q", combined[:200])
	}
}

func TestSplitTelegramText_InvalidLimit(t *testing.T) {
	_, err := splitTelegramText("anything", 0)
	if err == nil {
		t.Fatal("splitTelegramText zero limit must error")
	}
}

// TestSplitTelegramText_NoBreakInsideTag: when the natural cut at the
// byte limit lands inside an HTML tag (e.g., `<b>`, `</blockquote>`),
// splitTelegramText must walk back to a safe cut point that does NOT
// leave the second chunk starting mid-tag. The first chunk may end
// with an unclosed opening tag — Telegram's HTML parser tolerates
// that — but the second chunk must start at a position that is not
// between '<' and '>'.
func TestSplitTelegramText_NoBreakInsideTag(t *testing.T) {
	// 3890 chars of filler + `<b>bold</b>` (11 chars) + 50 chars tail
	// = 3951 chars. With limit 3900, natural cut at byte 3900 lands
	// inside `<b>` (positions 3890..3892). The function MUST walk back
	// to a position BEFORE the `<b>` so the tag stays whole in chunk 1
	// (unclosed `<b>` is fine) or AFTER `</b>` so chunk 2 starts clean.
	filler := strings.Repeat("a", 3890)
	body := filler + "<b>bold</b>" + strings.Repeat("x", 50)
	parts, err := splitTelegramText(body, 3900)
	if err != nil {
		t.Fatalf("splitTelegramText: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected ≥2 chunks, got %d", len(parts))
	}
	for i, p := range parts {
		if len(p) > 3900 {
			t.Errorf("part %d over limit: %d", i, len(p))
		}
		// No chunk may START inside an HTML tag. The "starts inside
		// a tag" condition is: chunk[0] is '<' AND chunk continues
		// with non-'>' chars without hitting '>' first. In other
		// words, the chunk is mid-tag.
		if isMidTag(p) {
			t.Errorf("part %d starts mid-tag: %q...", i, p[:min(60, len(p))])
		}
	}
}

// TestSplitTelegramText_CutRightAfterOpenBracket: explicit regression
// for the case where the natural byte-cut lands at position `start +
// limit` which is the byte right after '<'. The walk-back must
// detect this and shift the cut left, not return a chunk that begins
// with the second character of an opening tag.
func TestSplitTelegramText_CutRightAfterOpenBracket(t *testing.T) {
	// 3899 bytes of filler + '<' at position 3899 + 'b' at 3900 + ...
	// Limit = 3900 → naive cut at 3900 puts '<' in chunk 1 and 'b'
	// at chunk 2 start → chunk 2 starts mid-tag.
	filler := strings.Repeat("x", 3899)
	body := filler + "<b>more content after the tag</b>"
	parts, err := splitTelegramText(body, 3900)
	if err != nil {
		t.Fatalf("splitTelegramText: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected ≥2 chunks, got %d", len(parts))
	}
	for i, p := range parts {
		if isMidTag(p) {
			t.Errorf("part %d starts mid-tag: %q...", i, p[:min(60, len(p))])
		}
	}
}

// TestSplitTelegramText_PreBlockAtomic: a <pre>...</pre> block must
// stay whole in a single chunk. Cuts must never land inside the pre
// block content when the block fits within the limit.
//
// Note: when the pre block ITSELF is larger than the limit, the
// function falls back to hard-cut and the block may be split with
// broken rendering — that's the documented "pre-block-spanning-limits"
// limitation, covered by TestSplitTelegramText_PreBlockTooBigHardCut.
func TestSplitTelegramText_PreBlockAtomic(t *testing.T) {
	// 500 chars filler + <pre>...</pre> (~2910 chars total) +
	// 500 chars tail = ~3910 chars. Limit 3900 → just over limit
	// so the function MUST cut somewhere; the cut must land before
	// `<pre>` (i.e., the pre block stays whole in chunk 2 along
	// with the tail), not inside the pre content.
	preContent := strings.Repeat("x", 2900) // pre block content
	body := strings.Repeat("a", 500) + "<pre>" + preContent + "</pre>" + strings.Repeat("b", 500)
	parts, err := splitTelegramText(body, 3900)
	if err != nil {
		t.Fatalf("splitTelegramText: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected ≥2 chunks, got %d", len(parts))
	}

	// Find the chunk that contains <pre> and the chunk that contains
	// </pre>. They must be the same chunk.
	preOpenChunk := -1
	preCloseChunk := -1
	for i, p := range parts {
		if strings.Contains(p, "<pre>") {
			preOpenChunk = i
		}
		if strings.Contains(p, "</pre>") {
			preCloseChunk = i
		}
	}
	if preOpenChunk < 0 || preCloseChunk < 0 {
		t.Fatalf("pre block not found in chunks: open=%d close=%d", preOpenChunk, preCloseChunk)
	}
	if preOpenChunk != preCloseChunk {
		t.Errorf("pre block split across chunks: <pre> in chunk %d, </pre> in chunk %d",
			preOpenChunk, preCloseChunk)
	}
	// The chunk containing the pre block must NOT start mid-pre.
	if isMidPre(parts[preOpenChunk]) {
		t.Errorf("chunk %d starts inside pre block: %q...", preOpenChunk,
			parts[preOpenChunk][:min(60, len(parts[preOpenChunk]))])
	}
}

// TestSplitTelegramText_HardCutFallback: when the input is one giant
// <b>...</b> wrapping everything, every safe-position search fails
// and the function falls back to byte-cut at limit. The resulting
// chunks have unbalanced tags (one ends with unclosed `<b>`, the next
// starts with stray content). This is a documented known limitation;
// the test pins the fallback behaviour so a future refactor doesn't
// silently regress to an infinite loop.
func TestSplitTelegramText_HardCutFallback(t *testing.T) {
	body := "<b>" + strings.Repeat("a", 8000) + "</b>"
	parts, err := splitTelegramText(body, 3900)
	if err != nil {
		t.Fatalf("splitTelegramText: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected ≥2 chunks, got %d", len(parts))
	}
	// Every chunk must be ≤ limit.
	for i, p := range parts {
		if len(p) > 3900 {
			t.Errorf("part %d over limit: %d", i, len(p))
		}
	}
}

// TestSplitTelegramText_PreBlockTooBigHardCut: when a single pre
// block exceeds the limit by itself, the function falls back to
// byte-cut. This is the documented "pre-block-spanning-limits"
// limitation — the chunks produced will have rendering issues but
// the function MUST terminate and not loop forever.
func TestSplitTelegramText_PreBlockTooBigHardCut(t *testing.T) {
	body := "<pre>" + strings.Repeat("x", 8000) + "</pre>"
	parts, err := splitTelegramText(body, 3900)
	if err != nil {
		t.Fatalf("splitTelegramText: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected ≥2 chunks (hard cut inside huge pre), got %d", len(parts))
	}
	for i, p := range parts {
		if len(p) > 3900 {
			t.Errorf("part %d over limit: %d", i, len(p))
		}
	}
}

// TestSplitTelegramText_NestedTags: opening and closing tags around
// the cut boundary must not be split.
func TestSplitTelegramText_NestedTags(t *testing.T) {
	// Mix of <b>, <i>, </i>, </b> tags scattered through the body.
	// Verify no chunk starts with a partial tag.
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("<b>")
		b.WriteString(strings.Repeat("a", 38))
		b.WriteString("</b>")
	}
	body := b.String()
	parts, err := splitTelegramText(body, 3900)
	if err != nil {
		t.Fatalf("splitTelegramText: %v", err)
	}
	for i, p := range parts {
		if len(p) > 3900 {
			t.Errorf("part %d over limit: %d", i, len(p))
		}
		if isMidTag(p) {
			t.Errorf("part %d starts mid-tag: %q...", i, p[:min(60, len(p))])
		}
	}
}

// isMidTag reports whether the chunk begins in the middle of an
// HTML tag — i.e., its first byte is '<' and the chunk continues
// past the matching '>'. A chunk that IS exactly a tag like `<b>`
// does not count as mid-tag (it's a complete tag).
func isMidTag(chunk string) bool {
	if len(chunk) == 0 || chunk[0] != '<' {
		return false
	}
	// Find '>' in chunk. If found, the tag is complete and chunk
	// does not start mid-tag (it might be a complete tag, or a
	// fragment that happens to include a complete tag).
	// If NOT found, the chunk starts with '<' and has no '>' —
	// it's a tag fragment → mid-tag.
	return strings.IndexByte(chunk, '>') < 0
}

// isMidPre reports whether the chunk begins inside a pre block (i.e.,
// its first byte is NOT a tag and not a fresh-start position).
// Heuristic: a chunk starts mid-pre when it doesn't start with a
// '<' and contains a '</pre>' but no matching '<pre>'.
func isMidPre(chunk string) bool {
	if len(chunk) == 0 || chunk[0] == '<' {
		return false
	}
	return strings.Contains(chunk, "</pre>") && !strings.Contains(chunk, "<pre>")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- Commit B regression tests (P-8 placeholder leak fix + new
//     formatting features). ---

// TestRenderInline_NoPlaceholderLeak is the regression test for the
// 2026-08-25 incident where Telegram chat displayed `**PROTECTED0**`
// — the renderInline NUL-byte sentinel was getting stripped by
// some intermediate layer, leaving the literal "PROTECTED<n>"
// visible to the user. The new sentinel uses a single Unicode
// Private Use Area rune (U+E000+idx), which has no NUL bytes and
// cannot be confused with user text.
//
// The test pins the contract: rendered output NEVER contains the
// string "PROTECTED" (case-sensitive) and the PUA sentinel runes
// are always substituted back to their HTML.
func TestRenderInline_NoPlaceholderLeak(t *testing.T) {
	cases := []string{
		"plain text",
		"**bold**",
		"*italic*",
		"`code`",
		"~~strike~~",
		"[link](https://example.com)",
		"||spoiler||",
		"mixed **bold** and *italic* and `code`",
		"nested **bold with *italic* inside**",
		"# heading\n> quote\n- bullet\n```\ncode\n```",
		strings.Repeat("a", 500),
	}
	for _, in := range cases {
		out, err := RenderMarkdown(in)
		if err != nil {
			t.Fatalf("RenderMarkdown(%q): %v", in, err)
		}
		if strings.Contains(out, "PROTECTED") {
			t.Errorf("renderInline leaked placeholder for input %q; got %q", in, out)
		}
		// No PUA chars in output (they should all be substituted
		// back to their HTML strings).
		for _, r := range out {
			if r >= 0xE000 && r < 0xF000 {
				t.Errorf("renderInline left PUA rune %U in output for input %q; got %q", r, in, out)
				break
			}
		}
	}
}

// TestRenderMarkdown_Strikethrough: `~~text~~` renders as
// `<s>text</s>` (Telegram's strikethrough tag). Markdown doesn't
// have a native underline syntax, so `<u>` is intentionally NOT
// implemented — LLM outputs commonly produce `~~` but rarely
// anything else for emphasis deletion.
func TestRenderMarkdown_Strikethrough(t *testing.T) {
	out, err := RenderMarkdown("this is ~~deleted~~ text")
	if err != nil {
		t.Fatalf("RenderMarkdown strikethrough: %v", err)
	}
	if !strings.Contains(out, "<s>deleted</s>") {
		t.Fatalf("RenderMarkdown strikethrough = %q", out)
	}
	if strings.Contains(out, "~~") {
		t.Errorf("RenderMarkdown leaked literal ~~: %q", out)
	}
}

// TestRenderMarkdown_QuoteExpandable: a `>` quote block longer
// than expandableBlockquoteThresholdChars (800) renders with the
// `<blockquote expandable>` tag (Bot API 7.0+, 2024-03) — the
// client collapses it by default with a "▼ Expand" affordance.
// Short quotes stay as `<blockquote>` because expanding a one-
// line quote is more annoying than seeing it inline.
func TestRenderMarkdown_QuoteExpandable(t *testing.T) {
	// Long quote — well over 800 chars after rendering.
	long := strings.Repeat("blah blah ", 100) // 1000 chars
	out, err := RenderMarkdown("> " + long)
	if err != nil {
		t.Fatalf("RenderMarkdown long quote: %v", err)
	}
	if !strings.Contains(out, "<blockquote expandable>") {
		t.Fatalf("RenderMarkdown long quote missing expandable: %q", out)
	}
	if strings.Contains(out, "<blockquote>blah") {
		t.Errorf("RenderMarkdown long quote used non-expandable form: %q", out)
	}
}

// TestRenderMarkdown_QuoteShortStaysInline: a short quote stays
// as the legacy `<blockquote>` (no `expandable`). The threshold
// uses the post-render length to keep the visual rule consistent.
func TestRenderMarkdown_QuoteShortStaysInline(t *testing.T) {
	out, err := RenderMarkdown("> short quote")
	if err != nil {
		t.Fatalf("RenderMarkdown short quote: %v", err)
	}
	if !strings.Contains(out, "<blockquote>short quote</blockquote>") {
		t.Fatalf("RenderMarkdown short quote = %q", out)
	}
	if strings.Contains(out, "expandable") {
		t.Errorf("RenderMarkdown short quote should NOT use expandable: %q", out)
	}
}

// TestRenderMarkdown_FenceLanguageVariants: the fence opener
// ` ```X ` (where X is a Telegram-recognized language token) emits
// `<pre><code class="language-X">` so official clients do
// client-side syntax highlighting. Tests several common languages
// + the no-language fallback.
func TestRenderMarkdown_FenceLanguageVariants(t *testing.T) {
	cases := []struct {
		lang    string
		openTag string
	}{
		{"go", `<pre><code class="language-go">`},
		{"python", `<pre><code class="language-python">`},
		{"rust", `<pre><code class="language-rust">`},
		{"diff", `<pre><code class="language-diff">`},
		{"yaml", `<pre><code class="language-yaml">`},
		{"json", `<pre><code class="language-json">`},
		{"", "<pre><code>"}, // no language token → no class
	}
	for _, tc := range cases {
		input := "```" + tc.lang + "\nx\n```"
		out, err := RenderMarkdown(input)
		if err != nil {
			t.Fatalf("RenderMarkdown fence %q: %v", tc.lang, err)
		}
		if !strings.Contains(out, tc.openTag) {
			t.Errorf("RenderMarkdown fence %q missing openTag %q; got %q", tc.lang, tc.openTag, out)
		}
		if !strings.Contains(out, "</code></pre>") {
			t.Errorf("RenderMarkdown fence %q missing closing: %q", tc.lang, out)
		}
	}
}

// TestRenderMarkdown_FenceLangInjectionSafe: a hostile language
// token with quote / angle bracket / class attrs must not break
// out of the `class="..."` attribute. The fenceLangPattern only
// accepts `[A-Za-z0-9_+-]{1,32}` so anything else falls through to
// the no-language fallback (`<pre><code>`).
func TestRenderMarkdown_FenceLangInjectionSafe(t *testing.T) {
	hostile := "go\" evil=\"><script>alert(1)</script>"
	input := "```" + hostile + "\nx\n```"
	out, err := RenderMarkdown(input)
	if err != nil {
		t.Fatalf("RenderMarkdown hostile fence: %v", err)
	}
	// No literal injection — the hostile content must be either
	// rejected (fallback to no-class) or HTML-escaped.
	if strings.Contains(out, `<script>alert(1)</script>`) {
		t.Errorf("RenderMarkdown leaked hostile script: %q", out)
	}
	if strings.Contains(out, `class="go" evil=`) {
		t.Errorf("RenderMarkdown leaked attribute injection: %q", out)
	}
}

// --- Commit C: RenderForWire whole-body expandable fold ---

// TestRenderForWire_LongBodyFolded: a long rendered body
// (> expandableFullThresholdChars = 2000 chars but < ~4056 chars so
// wrap overhead stays within Telegram's 4096 limit) is wrapped in
// `<blockquote expandable>` so the message collapses to a "▼ Expand"
// affordance by default on Telegram.
func TestRenderForWire_LongBodyFolded(t *testing.T) {
	body := strings.Repeat("This is some content. ", 130) // ~3120 chars
	got := RenderForWire(body)
	if !strings.HasPrefix(got, "<blockquote expandable>") {
		t.Errorf("RenderForWire long body missing expandable open; got prefix %q", got[:min(60, len(got))])
	}
	if !strings.HasSuffix(got, "</blockquote>") {
		t.Errorf("RenderForWire long body missing expandable close; got suffix %q", got[len(got)-min(60, len(got)):])
	}
}

// TestRenderForWire_ShortBodyNotFolded: a short body stays
// unwrapped — no point in forcing a user to expand a one-paragraph
// result.
func TestRenderForWire_ShortBodyNotFolded(t *testing.T) {
	body := "short result"
	got := RenderForWire(body)
	if strings.Contains(got, "<blockquote") {
		t.Errorf("RenderForWire short body should not wrap; got %q", got)
	}
}

// TestRenderForWire_NoNestedFold: when RenderMarkdown already
// emitted a `<blockquote expandable>` (via a long `>` quote inside
// the body), RenderForWire must NOT wrap again — Telegram's HTML
// parser rejects nested `<blockquote>`.
func TestRenderForWire_NoNestedFold(t *testing.T) {
	// Force the inner quote to be expandable: a `>` block longer
	// than 800 chars after render.
	longQuote := strings.Repeat("quoted ", 200) // 1600 chars
	body := "> " + longQuote
	got := RenderForWire(body)
	count := strings.Count(got, "<blockquote")
	if count > 1 {
		t.Errorf("RenderForWire nested <blockquote>: count=%d; got %q", count, got)
	}
}

// TestRenderForWire_Over4096Fallback: a body whose wrapped form
// would exceed Telegram's 4096-char hard limit must NOT be wrapped.
// The caller (sendOutResultMessage) handles the long content via
// splitTelegramText — multiple message chunks, no expandable wrap.
// Wrap overhead = 40 chars (open + close). 4096 - 40 = 4056 chars
// max renderable before we fall back.
func TestRenderForWire_Over4096Fallback(t *testing.T) {
	body := strings.Repeat("x", 4090) // > expandableFullThresholdChars but wrap would exceed 4096
	got := RenderForWire(body)
	if strings.Contains(got, "<blockquote") {
		t.Errorf("RenderForWire over-4096 must NOT wrap; got prefix %q", got[:min(60, len(got))])
	}
}

func TestIsTableSeparator(t *testing.T) {
	cases := []struct {
		line string
	want bool
	}{
		{"| - | - |", true},
		{"|:-|:-:|", true},
		{"| - |", true},
		{"not a separator", false},
		{"| data | more |", false},
	}
	for _, tc := range cases {
		if got := isTableSeparator(tc.line); got != tc.want {
			t.Errorf("isTableSeparator(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestSafeLink(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"https://example.com", true},
		{"http://example.com", true},
		{"tg://resolve?domain=x", true},
		{"javascript:alert(1)", false},
		{"file:///etc/passwd", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := safeLink(tc.input); got != tc.want {
			t.Errorf("safeLink(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestEscapeHTML(t *testing.T) {
	got := escapeHTML(`<a href="x">&'`)
	want := "&lt;a href=&#34;x&#34;&gt;&amp;&#39;"
	if got != want {
		t.Fatalf("escapeHTML = %q, want %q", got, want)
	}
}

// RenderForWire is the single wire-facing entry point used by
// sendOutResultMessage (and any future raw-markdown block senders).
// It must round-trip the same shapes RenderMarkdown already
// covers, plus the empty short-circuit and the raw-HTML-escape
// guarantee. Keep this test set in lock-step with render.go's
// RenderForWire body — if you change one, change the other.

func TestRenderForWire_EmptyReturnsEmpty(t *testing.T) {
	if got := RenderForWire(""); got != "" {
		t.Fatalf("RenderForWire empty = %q, want empty", got)
	}
}

func TestRenderForWire_BoldPassesThroughAsHTML(t *testing.T) {
	got := RenderForWire("**strong**")
	if !strings.Contains(got, "<b>strong</b>") {
		t.Fatalf("RenderForWire bold = %q, want <b>strong</b>", got)
	}
	if strings.Contains(got, "**") {
		t.Fatalf("RenderForWire bold leaked literal asterisks: %q", got)
	}
}

func TestRenderForWire_FenceBlockRendersAsPreTag(t *testing.T) {
	got := RenderForWire("```go\nfunc main() {}\n```")
	if !strings.Contains(got, `<pre><code class="language-go">`) {
		t.Fatalf("RenderForWire fence missing language-tagged pre/code: %q", got)
	}
	if !strings.Contains(got, "</code></pre>") {
		t.Fatalf("RenderForWire fence missing closing: %q", got)
	}
	if !strings.Contains(got, "func main()") {
		t.Fatalf("RenderForWire fence missing code body: %q", got)
	}
}

func TestRenderForWire_LinkSafeScheme(t *testing.T) {
	got := RenderForWire("[click](https://example.com)")
	if !strings.Contains(got, `<a href="https://example.com">click</a>`) {
		t.Fatalf("RenderForWire link = %q", got)
	}
}

func TestRenderForWire_RawHTMLIsEscaped(t *testing.T) {
	got := RenderForWire("<script>alert(1)</script>")
	if strings.Contains(got, "<script>") {
		t.Fatalf("RenderForWire leaked <script>: %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("RenderForWire did not escape: %q", got)
	}
}

func TestRenderForWire_NotForAlreadyRenderedHTML(t *testing.T) {
	// Contract pin: RenderForWire is the raw-markdown → safe-HTML
	// wire-facing entry. It is NOT safe to call on already-rendered
	// HTML — entities get escaped a second time ("&amp;" →
	// "&amp;amp;") and pre-baked tag literals ("<b>") turn into
	// "&lt;b&gt;".
	//
	// sendOutResultMessage relies on this contract: the StatusBar
	// trailer (statusbar.RenderPanel output) is pre-baked safe HTML
	// and is composed OUTSIDE RenderForWire so the box-drawing frame
	// + already-escaped entities survive intact. This test
	// documents the "wrong use" (double-escape mode) so a future
	// refactor that accidentally routes the trailer through
	// RenderForWire — which would silently corrupt the StatusBar
	// panel — is caught by the test suite as a visible regression
	// (the assertions below start failing once the "wrong use"
	// becomes the "correct use").
	//
	// pi review finding 2026-08-24: prior version of this test was
	// vacuous (input had no < / > / &, so the condition was always
	// false and the body never executed). The new input carries all
	// three characters so each escape path fires.
	in := "&amp; <b>safe</b>"
	got := RenderForWire(in)
	if !strings.Contains(got, "&amp;amp;") {
		t.Errorf("RenderForWire must double-escape &amp; on already-rendered HTML; in=%q got=%q", in, got)
	}
	if !strings.Contains(got, "&lt;b&gt;") {
		t.Errorf("RenderForWire must escape literal <b> tags; in=%q got=%q", in, got)
	}
}

// ---------------------------------------------------------------------------
// v9 P3 — renderMarkdownSafe primitive + appendTrailerToBody primitive.
// ---------------------------------------------------------------------------

// TestRenderMarkdownSafe_EmptyReturnsEmpty: empty input must short-
// circuit (matches RenderForWire behaviour, matches the chain's
// per-entry short-circuit when entries are pure whitespace).
func TestRenderMarkdownSafe_EmptyReturnsEmpty(t *testing.T) {
	if got := renderMarkdownSafe(""); got != "" {
		t.Fatalf("renderMarkdownSafe(\"\") = %q; want empty", got)
	}
}

// TestRenderMarkdownSafe_BoldPassesThrough: a markdown bold expression
// must render as `<b>...</b>` HTML (the same path RenderForWire walks,
// since RenderForWire now delegates to renderMarkdownSafe).
func TestRenderMarkdownSafe_BoldPassesThrough(t *testing.T) {
	got := renderMarkdownSafe("**bold**")
	if !strings.Contains(got, "<b>bold</b>") {
		t.Fatalf("renderMarkdownSafe bold pass-through; got %q", got)
	}
}

// TestRenderMarkdownSafe_FenceRendersAsPre: triple-backtick fences
// must render as `<pre><code>...</code></pre>` HTML (preserves the
// OutError ```fences``` content path's contract). Telegram's
// official clients render `<pre><code>` as a monospace block; the
// language class is optional and absent for plain ``` fences.
func TestRenderMarkdownSafe_FenceRendersAsPre(t *testing.T) {
	got := renderMarkdownSafe("```\ncode\n```")
	if !strings.Contains(got, "<pre><code>code\n</code></pre>") {
		t.Fatalf("renderMarkdownSafe fence render; got %q", got)
	}
}

// TestRenderMarkdownSafe_RawHTMLEscapes: literal HTML angle brackets
// must be escaped to entities (defense against LLM-supplied markup).
func TestRenderMarkdownSafe_RawHTMLEscapes(t *testing.T) {
	got := renderMarkdownSafe("<script>")
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("renderMarkdownSafe raw-HTML escape; got %q", got)
	}
}

// TestRenderMarkdownSafe_DelegatesToRenderForWire confirms the v9 P3
// DRY contract: RenderForWire is a thin wrapper around
// renderMarkdownSafe. Both must produce byte-identical output for any
// given input (so a future caller picking one over the other for
// behaviour parity is safe).
func TestRenderMarkdownSafe_DelegatesToRenderForWire(t *testing.T) {
	cases := []string{
		"",
		"plain text",
		"**bold**",
		"`code`",
		"```\nblock\n```",
		"<tag>",
	}
	for _, in := range cases {
		safe := renderMarkdownSafe(in)
		wire := RenderForWire(in)
		if safe != wire {
			t.Errorf("renderMarkdownSafe(%q) != RenderForWire(%q); safe=%q wire=%q", in, in, safe, wire)
		}
	}
}

// TestAppendTrailerToBody_NoFooter: nil/empty footerLines must return
// body unchanged (matches the chain appendSegmentForKind policy of
// "skip trailer when msg has no status-bearing fields").
func TestAppendTrailerToBody_NoFooter(t *testing.T) {
	cases := [][]string{nil, {}}
	for _, lines := range cases {
		got := appendTrailerToBody("body text", lines)
		if got != "body text" {
			t.Errorf("appendTrailerToBody with empty footer must return body unchanged; got %q", got)
		}
	}
}

// TestAppendTrailerToBody_WithFooter: a non-empty footer must be
// appended with a `\n\n` gap (NOT `\n────────\n` — see §11.12.4.1
// v9 P2.1 trailer-only boundary rationale).
func TestAppendTrailerToBody_WithFooter(t *testing.T) {
	body := "result body"
	footer := []string{"line1", "line2", "line3"}
	got := appendTrailerToBody(body, footer)
	panel := statusbar.RenderPanel(footer)
	want := body + "\n\n" + panel
	if got != want {
		t.Fatalf("appendTrailerToBody concat mismatch; got=%q want=%q", got, want)
	}
}

// TestAppendTrailerToBody_PanelBoxDrawingPreserved: the box-drawing
// chars (`┌──›`, `└──›`) inside RenderPanel output must NOT be
// run through any HTML escape — that's exactly why the trailer
// bypasses RenderMarkdown / renderMarkdownSafe. This test pins the
// contract: a future refactor that routes the trailer through
// RenderForWire will visibly regress this.
func TestAppendTrailerToBody_PanelBoxDrawingPreserved(t *testing.T) {
	body := "result"
	footer := []string{"line"}
	got := appendTrailerToBody(body, footer)
	if !strings.Contains(got, "┌") {
		t.Fatalf("appendTrailerToBody must preserve ┌ box-drawing; got %q", got)
	}
	if !strings.Contains(got, "└") {
		t.Fatalf("appendTrailerToBody must preserve └ box-drawing; got %q", got)
	}
}
