// Package feishu — F-63 heartbeat rendering tests.
//
// These tests cover the rendering-layer half of F-63:
//
//   - buildReceiptCard with the new hb parameter — header
//     rendering, fallback to legacy placeholder, fallback to no
//     header when entries/tasks exist without a heartbeat.
//   - MessageReceipt.ApplyHeartbeat — idempotency, throttle,
//     empty-snapshot short-circuit, PATCH generation.
//   - renderHeartbeatHeader — direct format test.
//
// The handler-side integration (OutHeartbeat emitted BEFORE the
// policy chain, /think off / /tools off still counting) is
// covered by internal/runtime/heartbeat_handler_test.go.

package feishu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// newTestReceipt builds a fresh receipt for F-63 tests. The
// throttle is disabled (heartbeatMinInterval=0) so tests don't
// need to time.Sleep between ApplyHeartbeat calls.
func newTestReceipt(t *testing.T) (*MessageReceipt, *mockReceiptBot) {
	t.Helper()
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card", bot)
	r.heartbeatMinInterval = 0 // disable throttle for deterministic tests
	return r, bot
}

// TestBuildReceiptCard_HeartbeatHeader_NilSnapshot pins the
// legacy path: hb == nil + no entries/tasks → "🤖 Working"
// placeholder (this is what the receipt shows at the moment of
// MessageForwarded, before any event has landed on the tracker).
func TestBuildReceiptCard_HeartbeatHeader_NilSnapshot(t *testing.T) {
	body, _, err := buildReceiptCard(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if !strings.Contains(body, "🤖 Working") {
		t.Fatalf("expected placeholder header; body=%s", body)
	}
}

// TestBuildReceiptCard_HeartbeatHeader_EmptySnapshot pins the
// unified-placeholder behaviour: an empty snapshot (tracker
// exists but no think/tool observed yet) renders just
// "🤖 Working" — same emoji as the populated state, no dots,
// no counters, no time. The visual transition at first
// activity is a content add, not a style swap.
func TestBuildReceiptCard_HeartbeatHeader_EmptySnapshot(t *testing.T) {
	body, _, err := buildReceiptCard(nil, nil, nil, &messages.HeartbeatSnapshot{})
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if !strings.Contains(body, "🤖 Working") {
		t.Fatalf("expected \"🤖 Working\" placeholder for empty snapshot; body=%s", body)
	}
	// And it must NOT carry any counters / time — just the bare
	// "🤖 Working" with no separator-suffixed data.
	if strings.Contains(body, "�") || strings.Contains(body, "🔧") || strings.Contains(body, "⏱") {
		t.Fatalf("empty snapshot should have no counters/time; body=%s", body)
	}
	// Exact: just "🤖 Working" (no trailing dots, no "...").
	if !strings.Contains(body, `"content":"🤖 Working"`) {
		t.Fatalf("body content should be exactly \"🤖 Working\" (no dots); body=%s", body)
	}
}

// TestBuildReceiptCard_HeartbeatHeader_ThinkOnly covers the
// "🤖 Working · 💭 3 · ⏱ HH:MM:SS" line shape with only the
// thinking counter (no tools).
func TestBuildReceiptCard_HeartbeatHeader_ThinkOnly(t *testing.T) {
	now := time.Date(2026, 8, 15, 14, 35, 22, 0, time.UTC)
	body, _, err := buildReceiptCard(nil, nil, nil, &messages.HeartbeatSnapshot{
		ThinkCount: 3, LastBeatAt: now,
	})
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if !strings.Contains(body, "🤖 Working") {
		t.Fatalf("missing 🤖 Working; body=%s", body)
	}
	if !strings.Contains(body, "💭 3") {
		t.Fatalf("missing 💭 3; body=%s", body)
	}
	if strings.Contains(body, "🔧") {
		t.Fatalf("unexpected 🔧 chip (ToolCount=0); body=%s", body)
	}
	if !strings.Contains(body, "⏱ 14:35:22") {
		t.Fatalf("missing ⏱ 14:35:22; body=%s", body)
	}
}

// TestBuildReceiptCard_HeartbeatHeader_ToolOnly covers the
// "no thinking, just tool calls" shape.
func TestBuildReceiptCard_HeartbeatHeader_ToolOnly(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 5, 0, time.UTC)
	body, _, err := buildReceiptCard(nil, nil, nil, &messages.HeartbeatSnapshot{
		ToolCount: 12, LastBeatAt: now,
	})
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if !strings.Contains(body, "🔧 12") {
		t.Fatalf("missing 🔧 12; body=%s", body)
	}
	if strings.Contains(body, "💭") {
		t.Fatalf("unexpected 💭 chip (ThinkCount=0); body=%s", body)
	}
}

// TestBuildReceiptCard_HeartbeatHeader_AllPopulated is the full
// shape: thinking + tool + lastBeat.
func TestBuildReceiptCard_HeartbeatHeader_AllPopulated(t *testing.T) {
	now := time.Date(2026, 8, 15, 14, 35, 22, 0, time.UTC)
	body, _, err := buildReceiptCard(nil, nil, nil, &messages.HeartbeatSnapshot{
		ThinkCount: 3, ToolCount: 12, LastBeatAt: now,
	})
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	want := "🤖 Working · 💭 3 · 🔧 12 · ⏱ 14:35:22"
	if !strings.Contains(body, want) {
		t.Fatalf("missing full header line; want %q in body:\n%s", want, body)
	}
}

// TestBuildReceiptCard_HeartbeatHeader_LastBeatOnly covers a
// "agent is alive but no think / no tool yet" turn — just the
// time stamp.
func TestBuildReceiptCard_HeartbeatHeader_LastBeatOnly(t *testing.T) {
	now := time.Date(2026, 8, 15, 14, 35, 22, 0, time.UTC)
	body, _, err := buildReceiptCard(nil, nil, nil, &messages.HeartbeatSnapshot{
		LastBeatAt: now,
	})
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if !strings.Contains(body, "🤖 Working") {
		t.Fatalf("missing 🤖 Working (LastBeatAt-only); body=%s", body)
	}
	if strings.Contains(body, "💭") || strings.Contains(body, "🔧") {
		t.Fatalf("unexpected counter chips for LastBeatAt-only; body=%s", body)
	}
}

// TestBuildReceiptCard_HeartbeatHeader_WithEntries pins that the
// heartbeat header sits at the TOP, before the rolling-log
// entries. The first markdown element must be the header line.
func TestBuildReceiptCard_HeartbeatHeader_WithEntries(t *testing.T) {
	body, _, err := buildReceiptCard(
		[]LogEntry{{Icon: "💬", Text: "first reply"}},
		nil, nil,
		&messages.HeartbeatSnapshot{
			ThinkCount: 1, ToolCount: 1,
			LastBeatAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}

	// Find the index of the heartbeat header vs the entry.
	hbIdx := strings.Index(body, "🤖 Working")
	entryIdx := strings.Index(body, "first reply")
	if hbIdx < 0 {
		t.Fatal("heartbeat header missing")
	}
	if entryIdx < 0 {
		t.Fatal("entry text missing")
	}
	if hbIdx >= entryIdx {
		t.Fatalf("heartbeat header (idx %d) must precede entry (idx %d); body:\n%s",
			hbIdx, entryIdx, body)
	}
}

// TestRenderHeartbeatHeader_Direct is a focused format test —
// pin the exact separator + ordering without going through
// buildReceiptCard.
func TestRenderHeartbeatHeader_Direct(t *testing.T) {
	now := time.Date(2026, 8, 15, 14, 35, 22, 0, time.UTC)
	hb := &messages.HeartbeatSnapshot{
		ThinkCount: 2, ToolCount: 5, LastBeatAt: now,
	}
	got := renderHeartbeatHeader(hb)
	want := "🤖 Working · 💭 2 · 🔧 5 · ⏱ 14:35:22"
	if got != want {
		t.Fatalf("renderHeartbeatHeader = %q, want %q", got, want)
	}

	// Empty / nil paths.
	if got := renderHeartbeatHeader(nil); got != "" {
		t.Fatalf("renderHeartbeatHeader(nil) = %q, want \"\"", got)
	}
	if got := renderHeartbeatHeader(&messages.HeartbeatSnapshot{}); got != "🤖 Working" {
		t.Fatalf("renderHeartbeatHeader(empty) = %q, want %q", got, "🤖 Working")
	}
}

// TestApplyHeartbeat_Idempotent (F-63 §7.1) — applying the same
// snapshot twice produces only one PATCH (ThinkCount/ToolCount
// didn't change). LastBeatAt is refreshed but is NOT a trigger.
func TestApplyHeartbeat_Idempotent(t *testing.T) {
	r, bot := newTestReceipt(t)
	r.heartbeatMinInterval = 0

	snap := messages.HeartbeatSnapshot{
		ThinkCount: 1,
		LastBeatAt: time.Now(),
	}
	r.ApplyHeartbeat(context.Background(), snap)
	r.ApplyHeartbeat(context.Background(), snap)

	if got := len(bot.patches); got != 1 {
		t.Fatalf("ApplyHeartbeat produced %d PATCHes; want 1 (idempotent)", got)
	}
}

// TestApplyHeartbeat_TriggersPatchOnCountChange is the happy
// path: a counting increment must produce a PATCH.
func TestApplyHeartbeat_TriggersPatchOnCountChange(t *testing.T) {
	r, bot := newTestReceipt(t)
	r.heartbeatMinInterval = 0

	r.ApplyHeartbeat(context.Background(), messages.HeartbeatSnapshot{
		ThinkCount: 1,
		LastBeatAt: time.Now(),
	})
	r.ApplyHeartbeat(context.Background(), messages.HeartbeatSnapshot{
		ThinkCount: 2,
		LastBeatAt: time.Now(),
	})

	if got := len(bot.patches); got != 2 {
		t.Fatalf("expected 2 PATCHes (one per count change); got %d", got)
	}

	// PATCH body must contain the heartbeat header.
	last := bot.patches[len(bot.patches)-1]
	if !strings.Contains(last.Body, "🤖 Working") {
		t.Fatalf("PATCH body missing heartbeat header: %s", last.Body)
	}
	if !strings.Contains(last.Body, "💭 2") {
		t.Fatalf("PATCH body missing latest count: %s", last.Body)
	}
}

// TestApplyHeartbeat_Throttled (F-63 §7.1) — heartbeatMinInterval
// caps the PATCH rate. A burst of 5 ApplyHeartbeat calls with
// count increments produces only 1 PATCH (the first), the rest
// are coalesced.
func TestApplyHeartbeat_Throttled(t *testing.T) {
	r, bot := newTestReceipt(t)
	r.heartbeatMinInterval = 2 * time.Second

	for i := 1; i <= 5; i++ {
		r.ApplyHeartbeat(context.Background(), messages.HeartbeatSnapshot{
			ThinkCount: i,
			LastBeatAt: time.Now(),
		})
	}
	if got := len(bot.patches); got != 1 {
		t.Fatalf("throttled ApplyHeartbeat produced %d PATCHes; want 1 (first wins, rest skipped)", got)
	}

	// Verify the tracker advanced despite the throttle — the
	// receipt's heartbeat snapshot has the latest count, so the
	// next unthrottled PATCH will show the latest value.
	if r.heartbeat.ThinkCount != 5 {
		t.Fatalf("receipt heartbeat ThinkCount = %d, want 5 (tracker must advance even when PATCH is throttled)",
			r.heartbeat.ThinkCount)
	}
}

// TestApplyHeartbeat_EmptySnapshotNoOp — passing a zero-valued
// snapshot does NOT trigger a PATCH (no counter change).
func TestApplyHeartbeat_EmptySnapshotNoOp(t *testing.T) {
	r, bot := newTestReceipt(t)
	r.heartbeatMinInterval = 0

	r.ApplyHeartbeat(context.Background(), messages.HeartbeatSnapshot{})

	if got := len(bot.patches); got != 0 {
		t.Fatalf("empty snapshot produced %d PATCHes; want 0", got)
	}
}

// TestApplyHeartbeat_ThrottleElapses — after heartbeatMinInterval
// passes, the next ApplyHeartbeat is allowed through. We use a
// short interval + time.Sleep to keep the test fast.
func TestApplyHeartbeat_ThrottleElapses(t *testing.T) {
	r, bot := newTestReceipt(t)
	r.heartbeatMinInterval = 20 * time.Millisecond

	r.ApplyHeartbeat(context.Background(), messages.HeartbeatSnapshot{
		ThinkCount: 1, LastBeatAt: time.Now(),
	})
	if got := len(bot.patches); got != 1 {
		t.Fatalf("phase1 PATCHes = %d, want 1", got)
	}

	r.ApplyHeartbeat(context.Background(), messages.HeartbeatSnapshot{
		ThinkCount: 2, LastBeatAt: time.Now(),
	})
	if got := len(bot.patches); got != 1 {
		t.Fatalf("phase2 (within throttle) PATCHes = %d, want 1 (still throttled)", got)
	}

	time.Sleep(40 * time.Millisecond)

	r.ApplyHeartbeat(context.Background(), messages.HeartbeatSnapshot{
		ThinkCount: 3, LastBeatAt: time.Now(),
	})
	if got := len(bot.patches); got != 2 {
		t.Fatalf("phase3 (after throttle) PATCHes = %d, want 2", got)
	}
}

// TestApplyHeartbeat_NilReceiptSafe — defensive nil check.
// ApplyHeartbeat is sometimes called via MessageReceipt helper
// paths in tests where the receipt might not exist yet.
func TestApplyHeartbeat_NilReceiptSafe(t *testing.T) {
	var r *MessageReceipt
	r.ApplyHeartbeat(context.Background(), messages.HeartbeatSnapshot{
		ThinkCount: 1, LastBeatAt: time.Now(),
	})
	// No panic = pass.
}

// TestApplyHeartbeat_PATCHCardHasHeader pins that the PATCHed
// card body actually contains the heartbeat header (not just an
// empty diff). This is the contract the Feishu adapter
// promises: every heartbeat-driven PATCH paints the user-visible
// "🤖 Working" line.
func TestApplyHeartbeat_PATCHCardHasHeader(t *testing.T) {
	r, bot := newTestReceipt(t)
	r.heartbeatMinInterval = 0

	r.ApplyHeartbeat(context.Background(), messages.HeartbeatSnapshot{
		ThinkCount: 7, ToolCount: 3,
		LastBeatAt: time.Date(2026, 8, 15, 14, 35, 22, 0, time.UTC),
	})

	if len(bot.patches) != 1 {
		t.Fatalf("expected 1 PATCH; got %d", len(bot.patches))
	}
	body := bot.patches[0].Body

	// Body must be valid JSON and contain the heartbeat line.
	var parsed struct {
		Body struct {
			Elements []map[string]any
		}
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("PATCH body not valid JSON: %v", err)
	}
	if len(parsed.Body.Elements) == 0 {
		t.Fatal("PATCH body has no elements")
	}
	first, ok := parsed.Body.Elements[0]["content"].(string)
	if !ok {
		t.Fatalf("first element is not a markdown string: %+v", parsed.Body.Elements[0])
	}
	if !strings.Contains(first, "🤖 Working") {
		t.Fatalf("first element missing heartbeat header: %q", first)
	}
	if !strings.Contains(first, "💭 7") {
		t.Fatalf("first element missing 💭 7: %q", first)
	}
	if !strings.Contains(first, "🔧 3") {
		t.Fatalf("first element missing 🔧 3: %q", first)
	}
}

// TestAdapterSend_OutHeartbeat_RoutesToReceipt is the full
// integration: Adapter.Send on an OutHeartbeat finds the
// receipt and triggers ApplyHeartbeat (which PATCHes).
//
// We test this via the buildReceiptCard path (already exercised
// above) plus a focused test that ensures the OutHeartbeat case
// in Adapter.Send reaches ApplyHeartbeat. The mockReceiptBot
// doesn't back a real *Adapter (it implements receiptBot), so
// we test the receipt-level contract here and trust the
// dispatch wiring (the case in adapter.go).
//
// What we CAN test here: buildReceiptCard reads r.heartbeat
// correctly into the header. The dispatch case in adapter.go
// (Adapter.Send -> receiptFor -> ApplyHeartbeat) is covered by
// the adapter's integration smoke tests when the daemon boots.
func TestBuildReceiptCard_RendersReceiptHeartbeatAfterApply(t *testing.T) {
	r, bot := newTestReceipt(t)
	r.heartbeatMinInterval = 0

	// ApplyHeartbeat takes care of the render + PATCH.
	r.ApplyHeartbeat(context.Background(), messages.HeartbeatSnapshot{
		ThinkCount: 4, ToolCount: 9,
		LastBeatAt: time.Date(2026, 8, 15, 14, 35, 22, 0, time.UTC),
	})

	if len(bot.patches) != 1 {
		t.Fatalf("expected 1 PATCH from ApplyHeartbeat; got %d", len(bot.patches))
	}

	// Build the card with the receipt's own heartbeat snapshot
	// to verify the snapshot is wired correctly.
	body, _, err := buildReceiptCard(r.entries, r.tasks, r.footerLines, &r.heartbeat)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if !strings.Contains(body, "💭 4") || !strings.Contains(body, "🔧 9") {
		t.Fatalf("built body missing counts; body=%s", body)
	}

	// Sanity: the receipt is in chatsession.PromptRunning state.
	if got := r.PromptState(); got != chatsession.PromptRunning {
		t.Fatalf("PromptState = %v, want chatsession.PromptRunning", got)
	}

	// Sanity: agent import isn't accidentally dropped (would
	// surface as a build error, but make it explicit so the
	// package boundary stays intentional).
	_ = agent.EventAgentToolStart
}