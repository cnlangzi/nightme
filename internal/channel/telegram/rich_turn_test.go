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

// TestRenderRichTurnBlocks_HeaderFollowedByDivider verifies a
// divider sits between the heartbeat paragraph and the first
// entry so the card reads as three distinct regions:
// [heartbeat] ─ [body] ─ [footer]. The divider must come AFTER
// the heartbeat text and BEFORE the first entry.
func TestRenderRichTurnBlocks_HeaderFollowedByDivider(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 0,
		messageID:     100,
		headerLine:    "💭 3",
		hasContent:    true,
		entries: []richTurnEntry{
			{kind: "reply", body: "first body line"},
		},
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, `"type":"divider"`) {
		t.Fatalf("header → body divider must render; got %q", body)
	}
	headerIdx := strings.Index(body, "💭 3")
	dividerIdx := strings.Index(body, `"type":"divider"`)
	entryIdx := strings.Index(body, "first body line")
	if headerIdx < 0 || dividerIdx < 0 || entryIdx < 0 {
		t.Fatalf("missing header / divider / entry; got %q", body)
	}
	if !(headerIdx < dividerIdx && dividerIdx < entryIdx) {
		t.Fatalf("divider must sit between header and entry; got %q", body)
	}
}

// TestRenderRichTurnBlocks_NoBodyNoHeaderDivider verifies the
// header divider is gated on body presence — a heartbeat alone
// (no entries, no task list) emits no divider, since a stray
// <hr/> with nothing below reads as visual noise.
func TestRenderRichTurnBlocks_NoBodyNoHeaderDivider(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 0,
		messageID:     100,
		headerLine:    "💭 3",
		hasContent:    true,
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(body, `"type":"divider"`) {
		t.Fatalf("header without body must not emit a divider; got %q", body)
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
