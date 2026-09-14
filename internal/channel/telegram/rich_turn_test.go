package telegram

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRichTurnsIndex_GetOrCreate(t *testing.T) {
	idx := newRichTurnsIndex(10)
	turn1 := idx.getOrCreate("chat1", 1, 100)
	if turn1 == nil {
		t.Fatal("getOrCreate returned nil")
	}
	if turn1.chatID != "chat1" || turn1.topicID != 1 || turn1.userMessageID != 100 {
		t.Errorf("turn key mismatch: %+v", turn1)
	}
	// Idempotent: same key returns same turn.
	turn1Again := idx.getOrCreate("chat1", 1, 100)
	if turn1Again != turn1 {
		t.Errorf("getOrCreate returned different instance for same key")
	}
	// Different key returns new turn.
	turn2 := idx.getOrCreate("chat1", 1, 200)
	if turn2 == turn1 {
		t.Errorf("different key should produce new turn")
	}
}

func TestRichTurnsIndex_Purge(t *testing.T) {
	idx := newRichTurnsIndex(10)
	idx.getOrCreate("chat1", 1, 100)
	idx.purge("chat1", 1, 100)
	if _, ok := idx.lookup("chat1", 1, 100); ok {
		t.Errorf("purge should remove entry")
	}
}

func TestRichTurnsIndex_Lookup(t *testing.T) {
	idx := newRichTurnsIndex(10)
	if _, ok := idx.lookup("chat1", 1, 100); ok {
		t.Errorf("lookup on empty should return false")
	}
	idx.getOrCreate("chat1", 1, 100)
	if _, ok := idx.lookup("chat1", 1, 100); !ok {
		t.Errorf("lookup on existing key should return true")
	}
	if _, ok := idx.lookup("chat1", 2, 100); ok {
		t.Errorf("lookup on different topic should return false")
	}
}

func TestRichTurnsIndex_Cap(t *testing.T) {
	idx := newRichTurnsIndex(3)
	idx.getOrCreate("chat1", 1, 1)
	idx.getOrCreate("chat1", 1, 2)
	idx.getOrCreate("chat1", 1, 3)
	// Adding a 4th should evict the oldest.
	idx.getOrCreate("chat1", 1, 4)
	if len(idx.turns) != 3 {
		t.Errorf("cap should hold at 3, got %d", len(idx.turns))
	}
	if _, ok := idx.lookup("chat1", 1, 1); ok {
		t.Errorf("oldest entry (chat1/1/1) should have been evicted")
	}
	if _, ok := idx.lookup("chat1", 1, 4); !ok {
		t.Errorf("newest entry (chat1/1/4) should exist")
	}
}

func TestEncodeBlocksArray_Empty(t *testing.T) {
	out, err := encodeBlocksArray(nil)
	if err != nil {
		t.Fatalf("encodeBlocksArray(nil) error: %v", err)
	}
	if out != "null" {
		t.Errorf("encodeBlocksArray(nil) = %q, want null", out)
	}
}

func TestEncodeBlocksArray_WithEntries(t *testing.T) {
	blocks := []map[string]any{
		{"type": "heading", "text": "Title", "size": 1},
		{"type": "paragraph", "text": "Body"},
	}
	out, err := encodeBlocksArray(blocks)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(out, `"type":"heading"`) {
		t.Errorf("expected heading block in output, got %s", out)
	}
	if !strings.Contains(out, `"type":"paragraph"`) {
		t.Errorf("expected paragraph block in output, got %s", out)
	}
}

func TestMustParseBlocksArray_Invalid(t *testing.T) {
	if arr := mustParseBlocksArray("not json"); arr != nil {
		t.Errorf("invalid JSON should return nil, got %v", arr)
	}
	if arr := mustParseBlocksArray("{}"); arr != nil {
		t.Errorf("non-array JSON should return nil, got %v", arr)
	}
}

func TestMustParseBlocksArray_Valid(t *testing.T) {
	arr := mustParseBlocksArray(`[{"type":"paragraph","text":"hi"}]`)
	if len(arr) != 1 {
		t.Fatalf("expected 1 block, got %d", len(arr))
	}
	if arr[0]["type"] != "paragraph" {
		t.Errorf("type=%v", arr[0]["type"])
	}
}

func TestJsonNumberString(t *testing.T) {
	cases := map[int64]string{
		0:         "0",
		1:         "1",
		123:       "123",
		-1:        "-1",
		-123:      "-123",
		9999999:   "9999999",
		100000000: "100000000",
	}
	for in, want := range cases {
		if got := strconvFormatInt(in); got != want {
			t.Errorf("strconvFormatInt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestNormaliseRichMode_AutoConservative(t *testing.T) {
	// "auto" is reserved for future per-chat client-version
	// detection. Until then, normaliseRichMode should preserve it
	// as "auto" so the gate in richModeAllowsSend can decide.
	if got := normaliseRichMode("auto"); got != "auto" {
		t.Errorf("normaliseRichMode(\"auto\") = %q, want auto", got)
	}
}

func TestRenderRichTurnBlocks_ColdCreateDefaultHeading(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 0,
		messageID:     100,
		headerLine:    defaultRichTurnHeader,
		hasContent:    false,
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, defaultRichTurnHeader) {
		t.Fatalf("cold-create banner must render; got %q", body)
	}
	// Heartbeat is emitted as a paragraph block, not a heading —
	// it sits at the same scale as surrounding content entries.
	if !strings.Contains(body, `"type":"paragraph"`) {
		t.Fatalf("cold-create heartbeat must be a paragraph block; got %q", body)
	}
	if strings.Contains(body, `"type":"heading"`) {
		t.Fatalf("cold-create heartbeat must NOT be a heading block; got %q", body)
	}
}

func TestRenderRichTurnBlocks_DefaultSuppressedAfterContent(t *testing.T) {
	// Once content has arrived and no real heartbeat stamped
	// the header, the default fallback must NOT render — the
	// "🤖 Working…" banner would read as a stale "still thinking"
	// cue over already-arrived entries.
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 0,
		messageID:     100,
		headerLine:    defaultRichTurnHeader,
		hasContent:    true,
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(body, defaultRichTurnHeader) {
		t.Fatalf("default banner must be suppressed after content; got %q", body)
	}
}

func TestRenderRichTurnBlocks_RealHeartbeat(t *testing.T) {
	// Real heartbeat (non-default text) replaces the default and
	// renders as a paragraph block. heartbeatText no longer wraps
	// in <b>/</b> at construction — so the rendered text is
	// the raw line.
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 0,
		messageID:     100,
		headerLine:    "🔧 5 · ⏱ 09:50",
		hasContent:    true,
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "🔧 5 · ⏱ 09:50") {
		t.Fatalf("real heartbeat must render; got %q", body)
	}
	if strings.Contains(body, "<b>") || strings.Contains(body, "</b>") {
		t.Fatalf("heartbeatText should not emit HTML tags; got %q", body)
	}
}

func TestRenderRichTurnBlocks_TerminalVerdict(t *testing.T) {
	// Done / Error verdicts carry "✅ " / "❌ " prefix. Render
	// as a paragraph block — same shape as a normal heartbeat.
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 0,
		messageID:     100,
		headerLine:    "✅ ⏱ 14:05:40",
		hasContent:    true,
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "✅ ⏱ 14:05:40") {
		t.Fatalf("verdict + time must render; got %q", body)
	}
	if !strings.Contains(body, `"type":"paragraph"`) {
		t.Fatalf("verdict must be a paragraph block; got %q", body)
	}
}

// TestRenderRichTurnBlocks_FooterAsFooterBlock verifies the
// statusbar lands in a dedicated InputRichBlockFooter block
// (Telegram Bot API 10.1) rather than the legacy "pre" / code
// block. The pre block rendered the chevron-tail frame inside a
// code fence with a "copy" affordance — visually noisy and
// indistinguishable from LLM-emitted code. The footer block type
// is the platform-native "session metadata at the bottom of the
// message" surface and renders as a muted caption region.
func TestRenderRichTurnBlocks_FooterAsFooterBlock(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 0,
		messageID:     100,
		headerLine:    "💭 1",
		hasContent:    true,
		footer: []string{
			"🤖: claude opus-4-5",
			"💰:「 1.2k / 0 / 234 · 5.0% (200k) · $0.012 」",
			"📁: code/nightme · ⎇ main",
		},
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, `"type":"footer"`) {
		t.Fatalf("statusbar must render as a footer block; got %q", body)
	}
	if strings.Contains(body, `"type":"pre"`) {
		t.Fatalf("statusbar must NOT render as a pre block; got %q", body)
	}
}

// TestRenderRichTurnBlocks_FooterPrecededByDivider verifies a
// divider sits between the entries and the footer so the eye
// gets a clean break before the metadata block.
func TestRenderRichTurnBlocks_FooterPrecededByDivider(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 0,
		messageID:     100,
		headerLine:    "💭 1",
		hasContent:    true,
		footer:        []string{"🤖: claude", "💰:「 1k 」", "📁: code/nightme"},
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, `"type":"divider"`) {
		t.Fatalf("divider must precede the footer; got %q", body)
	}
	dividerIdx := strings.Index(body, `"type":"divider"`)
	footerIdx := strings.Index(body, `"type":"footer"`)
	if dividerIdx < 0 || footerIdx < 0 || dividerIdx >= footerIdx {
		t.Fatalf("divider must come before footer; got %q", body)
	}
}

// TestRenderRichTurnBlocks_NoFooterNoDivider verifies that an
// empty footer (zero-line / nil) emits neither a divider nor a
// footer block — they ride together as a pair.
func TestRenderRichTurnBlocks_NoFooterNoDivider(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 0,
		messageID:     100,
		headerLine:    "💭 1",
		hasContent:    true,
		footer:        nil,
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(body, `"type":"divider"`) {
		t.Fatalf("no footer → no divider; got %q", body)
	}
	if strings.Contains(body, `"type":"footer"`) {
		t.Fatalf("no footer → no footer block; got %q", body)
	}
}

// TestRenderRichTurnBlocks_FooterPreservesChevronFrame verifies
// the three statusbar lines land in the footer block's `text`
// field as a single multi-line string, joined with newlines so
// Telegram renders each icon-prefixed line as its own visual
// row inside the footer caption region. The chevron-tail
// box-drawing frame (┌──› / └──›) is deliberately NOT added
// here — the native footer block type supplies the visual
// frame, so hand-rendered box-drawing would double up and
// clash with Telegram's own footer caption styling.
func TestRenderRichTurnBlocks_FooterPreservesChevronFrame(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 0,
		messageID:     100,
		headerLine:    "💭 1",
		hasContent:    true,
		footer: []string{
			"🤖: claude opus-4-5 abc-123",
			"💰:「 1.2k / 0 / 234 · 5.0% (200k) · $0.012 」",
			"📁: code/nightme · ⎇ main",
		},
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("render output must be valid JSON: %v\nbody=%s", err, body)
	}
	var footer map[string]any
	for _, b := range blocks {
		if b["type"] == "footer" {
			footer = b
			break
		}
	}
	if footer == nil {
		t.Fatalf("footer block not found in %v", blocks)
	}
	text, ok := footer["text"].(string)
	if !ok {
		t.Fatalf("footer text must be a string; got %T", footer["text"])
	}
	// Each of the three statusbar lines must appear in order, in a
	// single string with newlines between them.
	wantLines := []string{
		"🤖: claude opus-4-5 abc-123",
		"💰:「 1.2k / 0 / 234 · 5.0% (200k) · $0.012 」",
		"📁: code/nightme · ⎇ main",
	}
	want := strings.Join(wantLines, "\n")
	if text != want {
		t.Fatalf("footer text mismatch:\n got: %q\nwant: %q", text, want)
	}
	// No chevron frame: native footer block supplies the visual
	// surround. If a future change accidentally re-adds the
	// ┌──› / └──› via statusbar.RenderPanel here, the assertion
	// above (exact-string match) will fail first.
	if strings.Contains(text, "┌") || strings.Contains(text, "└") {
		t.Fatalf("footer block should not hand-draw box frame; got %q", text)
	}
}

// === Chain integration tests for 2026-09-14 walker扩面 =======================
//
// The cases below exercise the扩面 features at the chain entry layer
// (renderRichTurnBlocksLocked), not just at the walker level. They
// guard against regressions where the walker's new outputs fail to
// flow through to the rich turn wire form, or where the bail path
// re-introduces a plain text fallback.

// countBlocksByType walks a rendered blocks array and tallies block
// types. Used by the integration tests below to assert the chain
// produced the expected block mix.
func countBlocksByType(blocks []map[string]any) map[string]int {
	out := map[string]int{}
	for _, b := range blocks {
		if t, ok := b["type"].(string); ok {
			out[t]++
		}
	}
	return out
}

// findBlockOfType returns the first block of the given type (or nil).
func findBlockOfType(blocks []map[string]any, kind string) map[string]any {
	for _, b := range blocks {
		if b["type"] == kind {
			return b
		}
	}
	return nil
}

// chainFixture is a small helper that constructs a richTurn with a
// fixed chatID/topic/userMsg/headerLine and the given entries, then
// returns the rendered blocks JSON. The renderer doesn't reach for
// the network — `renderRichTurnBlocksLocked` is a pure function over
// the struct.
func chainFixture(t *testing.T, entries []richTurnEntry) string {
	t.Helper()
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 1,
		messageID:     100,
		headerLine:    defaultRichTurnHeader,
		hasContent:    true,
		entries:       entries,
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return body
}

// TestRenderRichTurnBlocks_OrderedListEntry verifies a chain entry
// containing an ordered list reaches the wire as a list block — the
// walker扩面 contract.
func TestRenderRichTurnBlocks_OrderedListEntry(t *testing.T) {
	body := chainFixture(t, []richTurnEntry{
		{kind: "reply", body: "1. one\n2. two\n3. three"},
	})
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\nbody=%s", err, body)
	}
	list := findBlockOfType(blocks, "list")
	if list == nil {
		t.Fatalf("expected a list block in chain output; got blocks=%v", countBlocksByType(blocks))
	}
	items, _ := list["items"].([]any)
	if len(items) != 3 {
		t.Errorf("list items=%d, want 3", len(items))
	}
}

// TestRenderRichTurnBlocks_TableEntry verifies a chain entry with a
// GFM table reaches the wire as a table block (not the older
// paragraph-fallback path that stripped `|` chars into literal text).
func TestRenderRichTurnBlocks_TableEntry(t *testing.T) {
	body := chainFixture(t, []richTurnEntry{
		{kind: "reply", body: "| A | B |\n|---|---|\n| 1 | 2 |"},
	})
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\nbody=%s", err, body)
	}
	table := findBlockOfType(blocks, "table")
	if table == nil {
		t.Fatalf("expected a table block; got types=%v", countBlocksByType(blocks))
	}
	cells, _ := table["cells"].([]any)
	if len(cells) != 2 {
		t.Errorf("table rows=%d, want 2 (header + 1 data)", len(cells))
	}
}

// TestRenderRichTurnBlocks_FallbackParagraphEntry verifies that an
// entry whose body triggers ok=false (here: an unterminated fence)
// still lands in the chain as a rich paragraph block, never as
// plain text. The Telegram adapter contract — every outbound bubble
// is rich_message[blocks] — depends on this guard.
func TestRenderRichTurnBlocks_FallbackParagraphEntry(t *testing.T) {
	body := chainFixture(t, []richTurnEntry{
		{kind: "reply", body: "before\n\n```\nunterminated fence"},
	})
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\nbody=%s", err, body)
	}
	para := findBlockOfType(blocks, "paragraph")
	if para == nil {
		t.Fatalf("expected a paragraph block from the fallback; got types=%v", countBlocksByType(blocks))
	}
	// Sanity: no "text" top-level field (which would be the
	// plain-text sendMessage shape, not rich blocks). If a future
	// change ever introduces a plain-text escape hatch, this
	// assertion will surface it.
	for _, b := range blocks {
		if _, ok := b["text"]; ok && b["type"] != "paragraph" && b["type"] != "heading" && b["type"] != "pre" && b["type"] != "footer" {
			// blocks above are the only ones allowed to hold
			// `text`; any other type with a top-level `text`
			// is a regression toward plain-text rendering.
			t.Errorf("unexpected top-level text on block %v", b)
		}
	}
}

// TestRenderRichTurnBlocks_FootnoteStrippedInEntry verifies the
// chain output for a paragraph with footnote refs is still rich
// blocks — the footnote is silently stripped, surrounding text
// survives, no plain-text escape is taken.
func TestRenderRichTurnBlocks_FootnoteStrippedInEntry(t *testing.T) {
	body := chainFixture(t, []richTurnEntry{
		{kind: "reply", body: "claim[^1] after"},
	})
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\nbody=%s", err, body)
	}
	para := findBlockOfType(blocks, "paragraph")
	if para == nil {
		t.Fatalf("expected a paragraph block; got types=%v", countBlocksByType(blocks))
	}
	// Wire form: array (entity slot present). Verify the
	// surrounding text pieces survived in order.
	arr, ok := para["text"].([]any)
	if !ok {
		t.Fatalf("paragraph text should be array form, got %T", para["text"])
	}
	want := []string{"claim", " after"}
	got := []string{}
	for _, item := range arr {
		if s, ok := item.(string); ok {
			got = append(got, s)
		}
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing string piece %q in %v", w, got)
		}
	}
}

// TestRenderRichTurnBlocks_ImageAsURLInEntry verifies an inline
// image ref in an entry renders through the chain as a clickable
// url entity (not a plain-text `![alt](url)` literal).
func TestRenderRichTurnBlocks_ImageAsURLInEntry(t *testing.T) {
	body := chainFixture(t, []richTurnEntry{
		{kind: "reply", body: "see ![logo](https://x.png) here"},
	})
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\nbody=%s", err, body)
	}
	para := findBlockOfType(blocks, "paragraph")
	if para == nil {
		t.Fatalf("expected a paragraph block; got types=%v", countBlocksByType(blocks))
	}
	arr, _ := para["text"].([]any)
	var found bool
	for _, item := range arr {
		m, _ := item.(map[string]any)
		if m["type"] == "url" && m["url"] == "https://x.png" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected url entity with url=https://x.png; got %+v", arr)
	}
}

// TestRenderRichTurnBlocks_RawHTMLInEntry verifies raw HTML in a
// chain entry lands as a paragraph block with the tags kept literal —
// not as a separate rich block, not as plain-text escape, not bailed.
func TestRenderRichTurnBlocks_RawHTMLInEntry(t *testing.T) {
	body := chainFixture(t, []richTurnEntry{
		{kind: "reply", body: `click <a href="https://x">here</a> now`},
	})
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\nbody=%s", err, body)
	}
	para := findBlockOfType(blocks, "paragraph")
	if para == nil {
		t.Fatalf("expected a paragraph block; got types=%v", countBlocksByType(blocks))
	}
	text, _ := para["text"].(string)
	want := `click <a href="https://x">here</a> now`
	if text != want {
		t.Errorf("text=%q, want literal %q", text, want)
	}
}

// TestRenderRichTurnBlocks_MultipleEntriesMix verifies a chain with
// multiple entries each carrying different扩面 features — heading
// (via entry body that's just a heading line), list, table, plain
// paragraph — all flow through and produce the expected block mix
// without dropping entries or bailing the whole chain.
func TestRenderRichTurnBlocks_MultipleEntriesMix(t *testing.T) {
	body := chainFixture(t, []richTurnEntry{
		{kind: "reply", body: "1. one\n2. two"},
		{kind: "reply", body: "| A | B |\n|---|---|\n| 1 | 2 |"},
		{kind: "reply", body: "afterword"},
	})
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\nbody=%s", err, body)
	}
	types := countBlocksByType(blocks)
	wantList := types["list"] >= 1
	wantTable := types["table"] >= 1
	wantPara := types["paragraph"] >= 1
	if !wantList {
		t.Errorf("missing list block; types=%v", types)
	}
	if !wantTable {
		t.Errorf("missing table block; types=%v", types)
	}
	if !wantPara {
		t.Errorf("missing paragraph block; types=%v", types)
	}
}
