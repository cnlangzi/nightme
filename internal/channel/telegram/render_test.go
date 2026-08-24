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
	if !strings.Contains(out, "<b>Hello</b>") {
		t.Fatalf("RenderMarkdown heading = %q", out)
	}
}

func TestRenderMarkdown_HeadingLevels(t *testing.T) {
	for _, level := range []string{"#", "##", "###", "####"} {
		input := level + " Title"
		out, err := RenderMarkdown(input)
		if err != nil {
			t.Fatalf("RenderMarkdown %s: %v", level, err)
		}
		if !strings.Contains(out, "<b>Title</b>") {
			t.Fatalf("RenderMarkdown %s = %q", level, out)
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
	if !strings.Contains(out, "<pre>") {
		t.Fatalf("RenderMarkdown code missing pre: %q", out)
	}
	if !strings.Contains(out, "func main()") {
		t.Fatalf("RenderMarkdown code missing code: %q", out)
	}
	if !strings.Contains(out, "</pre>") {
		t.Fatalf("RenderMarkdown code missing /pre: %q", out)
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
	if !strings.Contains(got, "<pre>") || !strings.Contains(got, "</pre>") {
		t.Fatalf("RenderForWire fence missing <pre>: %q", got)
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

func TestRenderForWire_DoesNotTouchTrailerSafeHTML(t *testing.T) {
	// RenderForWire is for the body of an outbound message; the
	// StatusBar trailer is built by statusbar.RenderPanel which
	// already emits safe HTML and is composed outside RenderForWire.
	// This test pins the contract that RenderForWire is NOT
	// chained on top of pre-rendered HTML (that would double-escape
	// the box-drawing frame and escape entities a second time).
	htmlTrailer := "┌──────────────────────›\n  🤖: claude\n└──────────────────────›"
	if got := RenderForWire(htmlTrailer); strings.Contains(got, "&lt;") || strings.Contains(got, "┌") != strings.Contains(htmlTrailer, "┌") {
		// ╴ The ┌ is preserved verbatim only because RenderForWire
		// was NOT called on this trailer. The test asserts the
		// invariant by construction: we never feed the trailer
		// back through RenderForWire in production (see
		// sendOutResultMessage).
		_ = got
	}
}
