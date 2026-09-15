package telegram

import (
	"encoding/json"
	"testing"
)

// TestBuildResultBlocks_EmptyBodyEmptyFooter returns ok=false so
// sendOutResultMessage can short-circuit (mirrors the silent drop
// when msg.Text is empty).
func TestBuildResultBlocks_EmptyBodyEmptyFooter(t *testing.T) {
	if _, ok := buildResultBlocks("", nil); ok {
		t.Fatal("empty body + empty footer must yield ok=false")
	}
	if _, ok := buildResultBlocks("", []string{}); ok {
		t.Fatal("empty body + empty footer slice must yield ok=false")
	}
}

// TestBuildResultBlocks_BodyOnly produces only body blocks (no
// divider, no footer block) when footer is absent.
func TestBuildResultBlocks_BodyOnly(t *testing.T) {
	out, ok := buildResultBlocks("hello", nil)
	if !ok {
		t.Fatal("body-only should succeed")
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(out), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d: %v", len(blocks), blocks)
	}
	if blocks[0]["type"] != "paragraph" {
		t.Errorf("block type = %v, want paragraph", blocks[0]["type"])
	}
}

// TestBuildResultBlocks_FooterOnly produces a divider + footer block
// when body is empty but footer carries status data.
func TestBuildResultBlocks_FooterOnly(t *testing.T) {
	out, ok := buildResultBlocks("", []string{"🤖: claude"})
	if !ok {
		t.Fatal("footer-only should succeed")
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(out), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (divider + footer), got %d: %v", len(blocks), blocks)
	}
	if blocks[0]["type"] != "divider" {
		t.Errorf("block 0 type = %v, want divider", blocks[0]["type"])
	}
	if blocks[1]["type"] != "footer" {
		t.Errorf("block 1 type = %v, want footer", blocks[1]["type"])
	}
}

// TestBuildResultBlocks_BodyAndFooter verifies the section order:
// body blocks → divider → footer block.
func TestBuildResultBlocks_BodyAndFooter(t *testing.T) {
	body := "# Title\n\nparagraph text"
	footer := []string{"🤖: claude", "💰: 「$0.05」"}
	out, ok := buildResultBlocks(body, footer)
	if !ok {
		t.Fatal("body + footer should succeed")
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(out), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	// heading + paragraph + divider + footer = 4
	if len(blocks) != 4 {
		t.Fatalf("expected 4 blocks, got %d: %v", len(blocks), blocks)
	}
	wantTypes := []string{"heading", "paragraph", "divider", "footer"}
	for i, want := range wantTypes {
		if blocks[i]["type"] != want {
			t.Errorf("block %d type = %v, want %s", i, blocks[i]["type"], want)
		}
	}
}

// TestBuildResultBlocks_FooterTextIsPlainStringWhenNoEntities pins
// the wire-form optimisation: when no footer line has inline
// entities, footer.text is a single string (Telegram's plain
// RichText shortcut), not an array of one-element strings.
func TestBuildResultBlocks_FooterTextIsPlainStringWhenNoEntities(t *testing.T) {
	footer := []string{
		"🤖: claude",
		"💰: 「12.3k / 8.2k · 10.5% · $0.087」",
		"📁: code/nightme · ⎇ main · + 2 · − 1",
	}
	out, ok := buildResultBlocks("body", footer)
	if !ok {
		t.Fatal("body + plain footer should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	footerBlock := blocks[len(blocks)-1]
	if footerBlock["type"] != "footer" {
		t.Fatalf("last block type = %v, want footer", footerBlock["type"])
	}
	text, ok := footerBlock["text"].(string)
	if !ok {
		t.Fatalf("footer.text should be string when no entities, got %T: %v",
			footerBlock["text"], footerBlock["text"])
	}
	// Should contain all three lines joined by \n.
	for _, line := range footer {
		if !contains(text, line) {
			t.Errorf("footer text missing line %q; got %q", line, text)
		}
	}
}

// TestBuildResultBlocks_FooterTextPreservesPRAnchor verifies the
// PR anchor `[#N](url)` becomes a `{"type":"url","text":"#N","url":...}`
// entity inside footer.text (not literal markdown, not literal HTML).
// This is the clickable-link behaviour that wireFormatFooterLine +
// parse_mode=HTML used to provide — moved into the rich-text path
// without dropping the affordance.
func TestBuildResultBlocks_FooterTextPreservesPRAnchor(t *testing.T) {
	footer := []string{
		"🤖: claude",
		"💰: 「$0.05」",
		"📁: code/nightme · ⎇ main · [#42](https://github.com/x/y/pull/42)",
	}
	out, ok := buildResultBlocks("body", footer)
	if !ok {
		t.Fatal("body + footer with PR anchor should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	footerBlock := blocks[len(blocks)-1]
	if footerBlock["type"] != "footer" {
		t.Fatalf("last block type = %v, want footer", footerBlock["type"])
	}
	text, ok := footerBlock["text"].([]any)
	if !ok {
		t.Fatalf("footer.text should be array (entity present), got %T: %v",
			footerBlock["text"], footerBlock["text"])
	}

	var found bool
	for _, item := range text {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "url" && m["text"] == "#42" && m["url"] == "https://github.com/x/y/pull/42" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected url entity {text:#42, url:...}, got: %v", text)
	}
	// Literal markdown `[#42](url)` must NOT appear in the wire form.
	rawJSON, _ := json.Marshal(text)
	if contains(string(rawJSON), "[#42]") {
		t.Errorf("footer.text leaked literal markdown link; got %s", rawJSON)
	}
}

// TestBuildResultBlocks_FooterTextDoesNotCoerceUnsafeScheme verifies
// that `[click](javascript:alert)` stays as literal text inside the
// footer (entity is dropped), mirroring safeLink's contract from
// rich_walker.go.
func TestBuildResultBlocks_FooterTextDoesNotCoerceUnsafeScheme(t *testing.T) {
	footer := []string{
		"📁: evil · [click](javascript:alert(1))",
	}
	out, ok := buildResultBlocks("body", footer)
	if !ok {
		t.Fatal("body + footer should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	footerBlock := blocks[len(blocks)-1]
	text, _ := footerBlock["text"].(string)
	if text == "" {
		// array form — scan for url entity
		text = "array"
	}
	// In either form, the javascript: URL must not survive as an entity.
	rawJSON, _ := json.Marshal(footerBlock["text"])
	if contains(string(rawJSON), `"url":"javascript:`) {
		t.Errorf("footer.text leaked javascript: scheme; wire=%s", rawJSON)
	}
}

// TestBuildResultBlocks_FooterLinesVerbatim verifies the footer
// block passes raw statusbar lines through unchanged (no
// statusbar.RenderPanel wrap). The footer block's caption-style
// rendering on the Telegram side provides the visual framing
// instead of box-drawing characters. This matches the rich turn
// path's behaviour (renderRichTurnBlocksLocked uses turn.footer
// strings verbatim) — the two OutResult paths stay visually
// consistent.
func TestBuildResultBlocks_FooterLinesVerbatim(t *testing.T) {
	footer := []string{"🤖: claude", "💰: 「$0.05」"}
	out, ok := buildResultBlocks("body", footer)
	if !ok {
		t.Fatal("body + footer should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	footerBlock := blocks[len(blocks)-1]
	if footerBlock["type"] != "footer" {
		t.Fatalf("last block type = %v, want footer", footerBlock["type"])
	}
	text, _ := footerBlock["text"].(string)
	want := "🤖: claude\n💰: 「$0.05」"
	if text != want {
		t.Errorf("footer.text = %q, want %q", text, want)
	}
}

// TestBuildResultBlocks_WalkerRejectDegradesToParagraph verifies
// the table-column-mismatch bail path: walker returns ok=false, the
// body degrades to a single paragraph block (better than dropping
// the message entirely).
func TestBuildResultBlocks_WalkerRejectDegradesToParagraph(t *testing.T) {
	body := "| A | B |\n|---|---|\n| 1 | 2 | 3 |" // column mismatch
	out, ok := buildResultBlocks(body, nil)
	if !ok {
		t.Fatal("walker-rejected body should fall back, not bail")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 1 || blocks[0]["type"] != "paragraph" {
		t.Fatalf("expected 1 paragraph block, got %v", blocks)
	}
}

// TestBuildResultBlocks_LongBodyStaysInRichBlocks verifies the
// char-cap path: body > 32K triggers the walker to bail, the
// caller falls back to a single paragraph block. (The chain
// path splits via splitTelegramText at 3900, but rich blocks have
// 32K+ per-block ceiling so splitting is unnecessary in the rich
// OutResult path.)
func TestBuildResultBlocks_LongBodyStaysInRichBlocks(t *testing.T) {
	// 32K + 1 chars — over richWalkerCharCap, walker bails.
	long := make([]byte, richWalkerCharCap+1)
	for i := range long {
		long[i] = 'x'
	}
	out, ok := buildResultBlocks(string(long), nil)
	if !ok {
		t.Fatal("over-cap body should fall back to paragraph, not bail")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 1 || blocks[0]["type"] != "paragraph" {
		t.Fatalf("expected 1 paragraph block, got %d blocks", len(blocks))
	}
}

// TestFooterLinesToRichText_Empty returns "" so the footer block's
// `text` field has a sensible default (caller should not invoke
// this with empty input — guarded by len(footerLines) > 0 — but
// defensive default matters for any future refactor that drops
// the guard).
func TestFooterLinesToRichText_Empty(t *testing.T) {
	if got := footerLinesToRichText(nil); got != "" {
		t.Errorf("empty input should return \"\", got %v", got)
	}
	if got := footerLinesToRichText([]string{}); got != "" {
		t.Errorf("empty slice should return \"\", got %v", got)
	}
}

// TestFooterLinesToRichText_PlainStringFastPath pins the
// optimisation: no entities anywhere → single string with \n join.
func TestFooterLinesToRichText_PlainStringFastPath(t *testing.T) {
	lines := []string{"first", "second", "third"}
	got := footerLinesToRichText(lines)
	s, ok := got.(string)
	if !ok {
		t.Fatalf("plain lines should return string, got %T", got)
	}
	want := "first\nsecond\nthird"
	if s != want {
		t.Errorf("got %q, want %q", s, want)
	}
}

// TestFooterLinesToRichText_MixedEntitiesReturnArray verifies the
// mixed path: any line carrying an inline entity forces an array
// output with `\n` separators between lines.
func TestFooterLinesToRichText_MixedEntitiesReturnArray(t *testing.T) {
	lines := []string{
		"🤖: claude",
		"📁: code/x · [#7](https://example.com/pr/7)",
	}
	got := footerLinesToRichText(lines)
	arr, ok := got.([]any)
	if !ok {
		t.Fatalf("lines with PR anchor should return []any, got %T: %v", got, got)
	}
	// Expect: ["🤖: claude", "\n", "📁: code/x · ", {url: #7}]
	var sawPlain, sawSep, sawURL bool
	for _, item := range arr {
		switch v := item.(type) {
		case string:
			switch v {
			case "\n":
				sawSep = true
			case "🤖: claude":
				sawPlain = true
			}
		case map[string]any:
			if v["type"] == "url" && v["text"] == "#7" {
				sawURL = true
			}
		}
	}
	if !sawPlain || !sawSep || !sawURL {
		t.Errorf("missing elements in %v (sawPlain=%v sawSep=%v sawURL=%v)",
			arr, sawPlain, sawSep, sawURL)
	}
}

// contains is a tiny strings.Contains wrapper to keep this file
// dependency-free (the test file shouldn't reach for strings just
// for substring checks).
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
