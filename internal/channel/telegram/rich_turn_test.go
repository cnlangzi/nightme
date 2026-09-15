package telegram

import (
	"encoding/json"
	"strconv"
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

// TestRenderRichTurnBlocks_FooterPRAnchorBecomesUrlEntity verifies
// that when a statusbar line carries a `[#N](url)` markdown link
// (the PR anchor format statusbar.formatPRSegment emits), the
// rich turn footer block renders the anchor as a `{"type":"url"}`
// RichText entity — not literal text or unparsed HTML. Same
// behaviour the standalone OutResult footer uses
// (footerLinesToRichText is shared between the two paths).
func TestRenderRichTurnBlocks_FooterPRAnchorBecomesUrlEntity(t *testing.T) {
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
			"💰:「 $0.05 」",
			"📁: code/nightme · ⎇ main · [#284](https://github.com/cnlangzi/nightme/pull/284)",
		},
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, body)
	}
	var footer map[string]any
	for _, b := range blocks {
		if b["type"] == "footer" {
			footer = b
			break
		}
	}
	if footer == nil {
		t.Fatalf("footer block missing; body=%s", body)
	}
	text, ok := footer["text"].([]any)
	if !ok {
		t.Fatalf("footer.text should be []any when PR anchor present, got %T: %v",
			footer["text"], footer["text"])
	}
	var foundPR bool
	for _, item := range text {
		m, _ := item.(map[string]any)
		if m["type"] == "url" && m["text"] == "#284" && m["url"] == "https://github.com/cnlangzi/nightme/pull/284" {
			foundPR = true
		}
	}
	if !foundPR {
		t.Errorf("expected url entity {text:#284 url:...} in footer; got %v", text)
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

// Chain integration tests below exercise ordered list / table /
// footnote / image / raw-HTML rendering through
// renderRichTurnBlocksLocked, not just at the walker unit. They
// guard against regressions where the walker output fails to flow
// through to the rich turn wire form, or where the bail path
// re-introduces a plain-text escape hatch.

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

// TestRenderRichTurnBlocks_FallbackParagraphEntry verifies that an
// entry whose body triggers ok=false (here: an unterminated fence)
// still lands in the chain as a rich paragraph block, never as
// plain text. The Telegram adapter contract — every outbound bubble
// is rich_message[blocks] — depends on this guard.
func TestRenderRichTurnBlocks_FallbackParagraphEntry(t *testing.T) {
	const raw = "before\n\n```\nunterminated fence"
	body := chainFixture(t, []richTurnEntry{
		{kind: "reply", body: raw},
	})
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\nbody=%s", err, body)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block from the fallback, got %d (types=%v)", len(blocks), countBlocksByType(blocks))
	}
	para := blocks[0]
	if para["type"] != "paragraph" {
		t.Fatalf("expected paragraph block, got type=%v", para["type"])
	}
	// Fallback paragraph carries the raw body verbatim — a plain-text
	// sendMessage escape hatch would surface here as the body being
	// the entire rich_message payload instead of a paragraph inside
	// a blocks array.
	text, _ := para["text"].(string)
	if text != raw {
		t.Errorf("paragraph text=%q, want %q (raw body)", text, raw)
	}
}

// TestRenderRichTurnBlocks_MultipleEntriesMix verifies a chain with
// multiple entries carrying different markdown shapes (heading,
// list, table, plain paragraph) — all flow through and produce the
// expected block mix without dropping entries or bailing the whole
// chain.
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

// ---------------------------------------------------------------------------
// v9 P2 task section (§11.12.6.1) — rich-turn list block rendering.
// Verifies the 📋 Tasks heading + native list block pipeline that
// landed after the v9 chain → rich-turn L3 retirement.
// ---------------------------------------------------------------------------

// TestRenderRichTurnBlocksLocked_TaskListSection pins the canonical
// task section shape: a `heading` block (📋 Tasks) followed by a
// `list` block of native Telegram list items. The order must be
// heading → list, not list → heading, so the user's eye lands on
// the section marker before scanning the rows.
func TestRenderRichTurnBlocksLocked_TaskListSection(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:        "123",
		topicID:       0,
		userMessageID: 0,
		messageID:     100,
		headerLine:    "💭 1",
		hasContent:    true,
		taskList: []taskListItem{
			{ID: "t1", Subject: "Plan the API", Status: "completed"},
			{ID: "t2", Subject: "Write code", Status: "in_progress", ActiveForm: "coding"},
			{ID: "t3", Subject: "Write tests", Status: "pending"},
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
	// Find the heading + list blocks (skip the header paragraph at
	// index 0 and the divider at index 1).
	var heading map[string]any
	var list map[string]any
	for _, b := range blocks {
		switch b["type"] {
		case "heading":
			heading = b
		case "list":
			list = b
		}
	}
	if heading == nil {
		t.Fatalf("expected a heading block; blocks=%v", blocks)
	}
	if got, want := heading["text"], richTurnTaskListHeadline; got != want {
		t.Fatalf("heading text = %q, want %q", got, want)
	}
	if list == nil {
		t.Fatalf("expected a list block; blocks=%v", blocks)
	}
	// Verify ordering: heading must precede the list in the wire form.
	headingIdx, listIdx := -1, -1
	for i, b := range blocks {
		if b["type"] == "heading" {
			headingIdx = i
		}
		if b["type"] == "list" {
			listIdx = i
		}
	}
	if !(headingIdx >= 0 && listIdx > headingIdx) {
		t.Fatalf("heading (%d) must precede list (%d); blocks=%v",
			headingIdx, listIdx, blocks)
	}
	// Verify rows: 3 items, prefix per status.
	rawItems, _ := list["items"].([]any)
	if len(rawItems) != 3 {
		t.Fatalf("expected 3 list items; got %d", len(rawItems))
	}
	type row struct{ text string }
	gotRows := make([]row, 0, len(rawItems))
	for _, raw := range rawItems {
		it, _ := raw.(map[string]any)
		bs, _ := it["blocks"].([]any)
		if len(bs) == 0 {
			continue
		}
		p, _ := bs[0].(map[string]any)
		text, _ := p["text"].(string)
		gotRows = append(gotRows, row{text})
	}
	wantPrefixes := []string{"✓ ", "• ", "• "}
	wantSubjects := []string{"Plan the API", "Write code (coding)", "Write tests"}
	for i, r := range gotRows {
		if !strings.HasPrefix(r.text, wantPrefixes[i]) {
			t.Errorf("row %d prefix = %q, want %q…; got %q",
				i, r.text[:min(2, len(r.text))], wantPrefixes[i], r.text)
		}
		if r.text[len(wantPrefixes[i]):] != wantSubjects[i] {
			t.Errorf("row %d subject = %q, want %q",
				i, r.text[len(wantPrefixes[i]):], wantSubjects[i])
		}
	}
}

// TestRenderRichTurnBlocksLocked_EmptyTaskListOmitsSection locks
// the no-orphan-headline rule: an empty / nil taskList produces no
// heading + no list block, so the user doesn't see a "📋 Tasks"
// banner over an empty list.
func TestRenderRichTurnBlocksLocked_EmptyTaskListOmitsSection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []taskListItem
	}{
		{"nil slice", nil},
		{"empty slice", []taskListItem{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newTestAdapter(t)
			turn := &richTurn{
				chatID: "123", topicID: 0, userMessageID: 0, messageID: 100,
				headerLine: "💭 1", hasContent: true,
				taskList: tc.items,
			}
			body, err := a.renderRichTurnBlocksLocked(turn)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.Contains(body, richTurnTaskListHeadline) {
				t.Fatalf("empty taskList must NOT paint headline; body=%s", body)
			}
			if strings.Contains(body, `"type":"list"`) {
				t.Fatalf("empty taskList must NOT emit list block; body=%s", body)
			}
		})
	}
}

// TestRenderRichTurnBlocksLocked_TaskCancelledAndDeletedFiltered
// locks the status filter: cancelled / deleted rows are dropped
// before the rune budget is consumed. Mirrors feishu's
// buildTaskChecklistChunks which only buckets InProgress / Pending
// / Completed.
func TestRenderRichTurnBlocksLocked_TaskCancelledAndDeletedFiltered(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID: "123", topicID: 0, userMessageID: 0, messageID: 100,
		headerLine: "💭 0", hasContent: true,
		taskList: []taskListItem{
			{ID: "t1", Subject: "alive-pending", Status: "pending"},
			{ID: "t2", Subject: "DEAD-cancelled", Status: "cancelled"},
			{ID: "t3", Subject: "alive-completed", Status: "completed"},
			{ID: "t4", Subject: "DEAD-deleted", Status: "deleted"},
		},
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(body, "DEAD-cancelled") {
		t.Fatalf("TaskCancelled row must be filtered; body=%s", body)
	}
	if strings.Contains(body, "DEAD-deleted") {
		t.Fatalf("TaskDeleted row must be filtered; body=%s", body)
	}
	if !strings.Contains(body, "alive-pending") {
		t.Fatalf("live pending row missing; body=%s", body)
	}
	if !strings.Contains(body, "alive-completed") {
		t.Fatalf("live completed row missing; body=%s", body)
	}
	// Headline must still render (live rows present).
	if !strings.Contains(body, richTurnTaskListHeadline) {
		t.Fatalf("headline missing despite live rows; body=%s", body)
	}
}

// TestRenderRichTurnBlocksLocked_TaskAllFilteredOmitsSection locks
// the dual-guard: when every row is filtered out, the section
// disappears entirely (no orphan headline, no empty list block).
func TestRenderRichTurnBlocksLocked_TaskAllFilteredOmitsSection(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID: "123", topicID: 0, userMessageID: 0, messageID: 100,
		headerLine: "💭 0", hasContent: true,
		taskList: []taskListItem{
			{ID: "t1", Subject: "DEAD-cancelled", Status: "cancelled"},
			{ID: "t2", Subject: "DEAD-deleted", Status: "deleted"},
		},
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(body, richTurnTaskListHeadline) {
		t.Fatalf("all-filtered taskList must NOT paint headline; body=%s", body)
	}
	if strings.Contains(body, `"type":"list"`) {
		t.Fatalf("all-filtered taskList must NOT emit list block; body=%s", body)
	}
}

// TestRenderRichTurnBlocksLocked_TaskListTruncatesAtBudget pins
// the rune-budget truncation: a long checklist is cut off at
// richTurnTaskListBudgetRunes, the last visible row gets a "…"
// suffix, and the markdown list shape stays well-formed. Without
// this, a 100-task snapshot would push the rich message past
// Telegram's 32K-char cap.
func TestRenderRichTurnBlocksLocked_TaskListTruncatesAtBudget(t *testing.T) {
	a, _ := newTestAdapter(t)
	const totalTasks = 60
	tasks := make([]taskListItem, totalTasks)
	for i := range tasks {
		tasks[i] = taskListItem{
			ID:      "t" + strconv.Itoa(i),
			Subject: strings.Repeat("x", 80), // 80-char subject
			Status:  "pending",
		}
	}
	turn := &richTurn{
		chatID: "123", topicID: 0, userMessageID: 0, messageID: 100,
		headerLine: "💭 0", hasContent: true,
		taskList: tasks,
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	var list map[string]any
	for _, b := range blocks {
		if b["type"] == "list" {
			list = b
			break
		}
	}
	if list == nil {
		t.Fatalf("expected list block; blocks=%v", blocks)
	}
	rawItems, _ := list["items"].([]any)
	items := make([]map[string]any, 0, len(rawItems))
	for _, raw := range rawItems {
		if it, ok := raw.(map[string]any); ok {
			items = append(items, it)
		}
	}
	if len(items) >= totalTasks {
		t.Fatalf("expected truncation to drop rows; got %d / %d", len(items), totalTasks)
	}
	if len(items) < 20 {
		t.Fatalf("too few rows; expected ~30, got %d (budget may be too tight)", len(items))
	}
	// Last visible row must carry the "…" suffix.
	if len(items) > 0 {
		last := items[len(items)-1]
		bs, _ := last["blocks"].([]any)
		if len(bs) > 0 {
			p, _ := bs[0].(map[string]any)
			text, _ := p["text"].(string)
			if !strings.HasSuffix(text, richTurnTaskListMore) {
				t.Errorf("last visible row must end with %q; got tail %q",
					richTurnTaskListMore, text[max(0, len(text)-10):])
			}
		}
	}
}

// TestRenderRichTurnBlocksLocked_TaskSubjectEscapesHTML locks the
// HTML-escape guard: a subject containing `<script>` must be
// escaped by the rich block renderer's inlineToRichText path so
// Telegram doesn't interpret it as a tag. XSS regression guard.
func TestRenderRichTurnBlocksLocked_TaskSubjectEscapesHTML(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID: "123", topicID: 0, userMessageID: 0, messageID: 100,
		headerLine: "💭 0", hasContent: true,
		taskList: []taskListItem{
			{ID: "t1", Subject: "<script>alert('xss')</script>", Status: "pending"},
			{ID: "t2", Subject: "with & ampersand", Status: "pending"},
		},
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The raw "<script>" literal must not appear in the wire form
	// (Telegram would refuse the message if it did, but we want a
	// tighter guard here — the rich block encoder should escape
	// every entity it touches).
	if strings.Contains(body, "<script>") {
		t.Fatalf("subject <script> must be HTML-escaped; body=%s", body)
	}
}

// TestRenderTaskRowText_FallbackSubjectTrimmed locks the
// subject→id fallback contract: a whitespace-only Subject must
// fall back to a (also-trimmed) ID, not produce a row of just
// "• ". Regression guard for the review finding that the fallback
// path skipped TrimSpace.
func TestRenderTaskRowText_FallbackSubjectTrimmed(t *testing.T) {
	cases := []struct {
		name string
		in   taskListItem
		want string
	}{
		{"empty subject falls back to id", taskListItem{ID: "t1", Status: "pending"}, "• t1"},
		{"whitespace subject falls back to id", taskListItem{ID: "t2", Subject: "   ", Status: "pending"}, "• t2"},
		{"whitespace id still produces a useful row", taskListItem{ID: "   ", Subject: "real", Status: "pending"}, "• real"},
		{"completed prefix intact with trimmed subject", taskListItem{ID: "x", Subject: "  done  ", Status: "completed"}, "✓ done"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderTaskRowText(c.in); got != c.want {
				t.Errorf("renderTaskRowText(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestRenderRichTurnTaskListBlocks_BudgetAccountsForSuffix pins
// the budget accounting: the trailing " " + … suffix is 2 runes
// (space + ellipsis), so the budget loop must reserve 2 — not 1 —
// for the marker. Regression guard for the review finding that
// the +1 reserve silently let the last row off-budget by 1 rune.
func TestRenderRichTurnTaskListBlocks_BudgetAccountsForSuffix(t *testing.T) {
	// Build N rows whose pre-suffix rune cost is exactly
	// (budget - headline). With a +1 reserve, the last row
	// would slip past the gate and end up over-budget after the
	// " …" suffix is appended.
	const totalRows = 5
	// Subject tuned so cost = headline + 5 rows + 2 = budget
	//  → headline (7) + 5*(len+1) + 2 = 3000  →  len = (3000-9)/5 - 1 = 597.4
	// pick a safe size that fits with margin
	subject := strings.Repeat("a", 590)
	items := make([]taskListItem, totalRows)
	for i := range items {
		items[i] = taskListItem{ID: "t", Subject: subject, Status: "pending"}
	}
	blocks, ok := renderRichTurnTaskListBlocks(items)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	// Find the list block.
	var list map[string]any
	for _, b := range blocks {
		if b["type"] == "list" {
			list = b
		}
	}
	if list == nil {
		t.Fatalf("no list block")
	}
	items2, _ := list["items"].([]map[string]any)
	if len(items2) != totalRows {
		t.Fatalf("budget should fit all %d rows (subject=590 runes + 2-reserve); got %d",
			totalRows, len(items2))
	}
}

// TestRenderRichTurnBlocksLocked_TaskListOrderAfterEntries pins the
// section ordering: entries → task list → footer. The task list
// must appear AFTER entries and BEFORE the footer divider so the
// user's eye reads the plan between the body content and the
// status trailer.
func TestRenderRichTurnBlocksLocked_TaskListOrderAfterEntries(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID: "123", topicID: 0, userMessageID: 0, messageID: 100,
		headerLine: "💭 1", hasContent: true,
		entries: []richTurnEntry{
			{kind: "reply", body: "thinking out loud"},
		},
		taskList: []taskListItem{
			{ID: "t1", Subject: "Plan", Status: "pending"},
		},
		footer: []string{"🤖: claude", "💰: tokens"},
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	headingIdx, listIdx, lastDividerIdx, footerIdx := -1, -1, -1, -1
	for i, b := range blocks {
		switch b["type"] {
		case "heading":
			if h, _ := b["text"].(string); h == richTurnTaskListHeadline {
				headingIdx = i
			}
		case "list":
			listIdx = i
		case "divider":
			lastDividerIdx = i
		case "footer":
			footerIdx = i
		}
	}
	// Order must be: <header> <divider> <entries> [heading, list] [divider, footer]
	if !(headingIdx > 0 && listIdx == headingIdx+1 &&
		lastDividerIdx > listIdx && footerIdx == lastDividerIdx+1) {
		t.Fatalf("expected order: heading(idx=%d) +1=list(idx=%d), divider(idx=%d) +1=footer(idx=%d); blocks=%v",
			headingIdx, listIdx, lastDividerIdx, footerIdx, blocks)
	}
}

// min / max are builtins since Go 1.21 (toolchain is 1.27), so
// no local helpers needed here.
