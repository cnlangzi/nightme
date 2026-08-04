package feishu

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildThinkingCard_ShortSingleDiv verifies the happy path:
// a short body (< divTextCharLimit) becomes a single lark_md
// div card. The emoji prefix must live inside the markdown
// content so Feishu renders it inline.
func TestBuildThinkingCard_ShortSingleDiv(t *testing.T) {
	body := "💭 Let me check that file."
	card, err := buildThinkingCard(body)
	if err != nil {
		t.Fatalf("buildThinkingCard: %v", err)
	}

	// Card JSON shape: { config, elements: [{ tag: div, text: { tag: lark_md, content } }] }
	var parsed struct {
		Config struct {
			WideScreenMode bool `json:"wide_screen_mode"`
		} `json:"config"`
		Elements []struct {
			Tag  string `json:"tag"`
			Text struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(card), &parsed); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}

	if !parsed.Config.WideScreenMode {
		t.Errorf("config.wide_screen_mode = false, want true (so the card fills the chat width)")
	}
	if len(parsed.Elements) != 1 {
		t.Fatalf("elements len = %d, want 1 (short body stays single-div)", len(parsed.Elements))
	}
	if parsed.Elements[0].Tag != "div" {
		t.Errorf("elements[0].tag = %q, want %q", parsed.Elements[0].Tag, "div")
	}
	if parsed.Elements[0].Text.Tag != "lark_md" {
		t.Errorf("elements[0].text.tag = %q, want %q (markdown element)", parsed.Elements[0].Text.Tag, "lark_md")
	}
	if parsed.Elements[0].Text.Content != body {
		t.Errorf("elements[0].text.content = %q, want %q", parsed.Elements[0].Text.Content, body)
	}
}

// TestBuildThinkingCard_LongSplitsIntoMultipleDivs verifies that
// a body exceeding divTextCharLimit is split into multiple div
// elements via F-37 splitMarkdownForDivs. Each div's content
// must be ≤ divTextCharLimit runes; the total preserves the
// original text (no truncation).
func TestBuildThinkingCard_LongSplitsIntoMultipleDivs(t *testing.T) {
	// Build a body that is well over 1000 runes (mix of ASCII
	// and prose).
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString("This is a sentence of thinking prose that adds up. ")
	}
	body := sb.String() // ~3000 chars

	card, err := buildThinkingCard(body)
	if err != nil {
		t.Fatalf("buildThinkingCard: %v", err)
	}

	var parsed struct {
		Elements []struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(card), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Elements) < 2 {
		t.Fatalf("elements len = %d, want ≥ 2 (long body must split)", len(parsed.Elements))
	}

	// Each div must respect the rune cap.
	for i, e := range parsed.Elements {
		// Count runes, not bytes — Feishu's limit is on visual chars.
		runes := []rune(e.Text.Content)
		if len(runes) > divTextCharLimit {
			t.Errorf("elements[%d] runes = %d, want ≤ %d", i, len(runes), divTextCharLimit)
		}
	}

	// Total content (reassembled) must contain the original
	// body — splitMarkdownForDivs is non-destructive for content
	// runes. Whitespace at split boundaries may be trimmed by
	// hardSplitRunes (single trailing space/newline per chunk),
	// so we allow a small (≤ 1% of body length) loss to bound
	// boundary rebalancing while still catching runaway drops.
	var joined strings.Builder
	for _, e := range parsed.Elements {
		joined.WriteString(e.Text.Content)
	}
	got := []rune(joined.String())
	want := []rune(body)
	delta := len(want) - len(got)
	if delta < 0 {
		delta = -delta
	}
	if delta > len(want)/100 {
		t.Errorf("joined rune count = %d, want ~%d (split dropped %d runes; >1%% boundary loss)",
			len(got), len(want), delta)
	}
}

// TestBuildThinkingCard_CodeBlockStaysAtomicAcrossSplit locks
// the F-37 atomic-split invariant: a fenced code block inside the
// thinking body must NOT be split across two div elements. If the
// split function ever regresses on this, code blocks would render
// broken in Feishu (missing ``` fence marker → no syntax
// highlighting).
//
// The body is intentionally padded past divTextCharLimit so
// the split MUST fire; the code block sits in the middle so any
// naive line-based split would land the opening fence in one div
// and the closing fence in another.
func TestBuildThinkingCard_CodeBlockStaysAtomicAcrossSplit(t *testing.T) {
	// Padding: 30 paragraphs of 60 chars each = ~1800 chars, well
	// over the 1000-rune div cap. Code block sits in the middle.
	pad := strings.Repeat("Padding prose for the thinking body that adds up. ", 30)
	body := "💭 " + pad[:len(pad)/2] +
		"\n```go\npackage main\nfunc main() { println(\"hi\") }\n```\n" +
		pad[len(pad)/2:]

	card, err := buildThinkingCard(body)
	if err != nil {
		t.Fatalf("buildThinkingCard: %v", err)
	}

	var parsed struct {
		Elements []struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(card), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// First check: the split actually fired (multiple divs).
	// Otherwise this test wouldn't be exercising the atomic
	// invariant at all.
	if len(parsed.Elements) < 2 {
		t.Fatalf("elements len = %d, want ≥ 2 (body is %d runes; must split)",
			len(parsed.Elements), len([]rune(body)))
	}

	// Atomic invariant: exactly ONE div contains the opening
	// fence (```go), and the SAME div contains the closing fence
	// (```) — never split. A naive line-based splitter would
	// distribute ``` markers across multiple divs.
	var divWithOpening int = -1
	for i, e := range parsed.Elements {
		if strings.Contains(e.Text.Content, "```go") {
			divWithOpening = i
			break
		}
	}
	if divWithOpening < 0 {
		t.Fatalf("no div contains opening fence ```go; code block was lost")
	}

	// The opening-fence div must also contain a closing fence
	// (the standalone ``` on its own line at the end of the code
	// block). Count occurrences of "```" in that div — at least
	// 2 means opening + closing both present.
	openingDiv := parsed.Elements[divWithOpening].Text.Content
	fenceCount := strings.Count(openingDiv, "```")
	if fenceCount < 2 {
		t.Errorf("opening-fence div (elements[%d]) has %d ``` markers, want ≥ 2 (opening + closing must coexist)",
			divWithOpening, fenceCount)
	}

	// No other div should contain a lone ``` (a stray closing
	// fence would render as broken markdown in Feishu).
	for i, e := range parsed.Elements {
		if i == divWithOpening {
			continue
		}
		if strings.Contains(e.Text.Content, "```") {
			t.Errorf("elements[%d] contains stray ``` (code block fences must stay in one div)", i)
		}
	}
}

// TestBuildThinkingCard_EmptyErrors confirms the helper rejects
// empty input rather than producing an empty card (which would
// 400 on Feishu). The runtime never passes empty bodies because
// gateway.Translate already filters them out, but the helper
// must be total for direct callers.
func TestBuildThinkingCard_EmptyErrors(t *testing.T) {
	if _, err := buildThinkingCard(""); err == nil {
		t.Fatal("buildThinkingCard(\"\") = nil error; want non-nil")
	}
}

// TestBuildThinkingCard_OutputIsValidJSON ensures every emitted
// card parses as JSON (sanity guard against future encoder bugs).
func TestBuildThinkingCard_OutputIsValidJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"short", "💭 short"},
		{"ascii boundary", strings.Repeat("a", divTextCharLimit+10)},
		{"multi-byte runes", strings.Repeat("中文", 600)},
	}
	for _, c := range cases {
		card, err := buildThinkingCard(c.body)
		if err != nil {
			t.Errorf("buildThinkingCard(%s): %v", c.name, err)
			continue
		}
		var any_ map[string]any
		if err := json.Unmarshal([]byte(card), &any_); err != nil {
			t.Errorf("card for %s is not valid JSON: %v", c.name, err)
		}
	}
}