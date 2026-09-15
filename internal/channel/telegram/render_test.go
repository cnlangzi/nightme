package telegram

import (
	"strings"
	"testing"
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

// TestRenderMarkdown_BoldWithInlineCode is the regression test for
// the PUA-placeholder slice being local to each renderInline frame.
//
// Before this fix, renderInline declared `placeholders := make([]string, 0)`
// inside its own body, and every recursive call (bold/italic/strike/
// link/spoiler nested via `renderInline(text)`) got its own FRESH
// slice. The outer scope's codeSpan pass had already replaced any
// backticks inside `text` with PUA runes (e.g. \xE000 representing
// `<code>foo</code>`); the recursive call's own placeholder table was
// empty, so its final PUA-char walk fell through and emitted the
// raw PUA rune as a Unicode character. Net effect: every inline-
// code span that lived inside any other markdown construct was
// silently dropped — Telegram rendered a stray Private Use Area
// glyph (or a blank space depending on font) instead of `<code>…</code>`.
//
// The 2026-08-26 Telegram bug report was triggered by a long reply
// that mixed `**bold**` and ``` `code` ``` heavily (e.g. `**`Dir`
// 现在 = `filepath.Dir`**`); the inline-code spans inside `**…**`
// vanished and the user saw spaces / tofu where bold text should
// have been monospace. This test pins the contract that inline-code
// inside bold (and the other recursive constructs) survives intact.
//
// The fix: renderInline now threads a shared `*[]string` placeholder
// table down through recursion. Children resolve outer-scope PUA
// runes via the same slice the parent used.
//
// See splitInlineWithPH for the recursive core.
func TestRenderMarkdown_BoldWithInlineCode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"single_code_in_bold",
			"**`Dir`**",
			"<b><code>Dir</code></b>",
		},
		{
			"code_between_text_in_bold",
			"**a `b` c**",
			"<b>a <code>b</code> c</b>",
		},
		{
			"two_codes_in_bold",
			"**a `b` c `d` e**",
			"<b>a <code>b</code> c <code>d</code> e</b>",
		},
		{
			"code_at_outer_then_bold",
			"`a` and `b` inside **bold**",
			"<code>a</code> and <code>b</code> inside <b>bold</b>",
		},
		{
			"code_in_italic",
			"*`x`*",
			"<i><code>x</code></i>",
		},
		{
			"code_in_strike",
			"~~`x`~~",
			"<s><code>x</code></s>",
		},
		{
			"code_in_link_label",
			"[`code`](https://example.com)",
			`<a href="https://example.com"><code>code</code></a>`,
		},
		{
			"bold_inside_italic_with_code",
			"*em `code` more*",
			"<i>em <code>code</code> more</i>",
		},
	}
	for _, tc := range cases {
		got, err := RenderMarkdown(tc.in)
		if err != nil {
			t.Errorf("%s: RenderMarkdown error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: input=%q\n  got:  %s\n  want: %s", tc.name, tc.in, got, tc.want)
		}
		// Belt-and-suspenders: no PUA chars / no PROTECTED leak.
		for _, r := range got {
			if r >= 0xE000 && r < 0xF000 {
				t.Errorf("%s: stray PUA rune %U in output %q", tc.name, r, got)
				break
			}
		}
		if strings.Contains(got, "PROTECTED") {
			t.Errorf("%s: leaked PROTECTED sentinel in output %q", tc.name, got)
		}
	}
}

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

// --- Commit D (legacy): footer markdown links → HTML for parse_mode=HTML ---
//
// appendTrailerToBody was the parse_mode=HTML StatusBar trailer renderer
// (body + "\n\n" + RenderPanel(footer-with-HTML-links)). Retired
// when OutResult migrated from rich_message[markdown] (where the
// HTML trailer was the only way to render a clickable PR anchor)
// to rich_message[blocks] with a dedicated footer block
// (renderRichTurnBlocksLocked + buildResultBlocks + footerLinesToRichText).
// See docs/channel/telegram.md §11.12.4.1 + §11.12.13.

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
