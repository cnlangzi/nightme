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
// "💭 3 · ⏱ HH:MM:SS" back-part shape (F-63 mutual exclusion:
// no "🤖 Working" prefix when activity > 0) with only the
// thinking counter (no tools).
func TestBuildReceiptCard_HeartbeatHeader_ThinkOnly(t *testing.T) {
	now := time.Date(2026, 8, 15, 14, 35, 22, 0, time.UTC)
	body, _, err := buildReceiptCard(nil, nil, nil, &messages.HeartbeatSnapshot{
		ThinkCount: 3, LastBeatAt: now,
	})
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	// F-63 mutual exclusion: ThinkCount>0 → back part only,
	// "🤖 Working" front part must NOT appear alongside it.
	if strings.Contains(body, "🤖 Working") {
		t.Fatalf("header must omit 🤖 Working when activity > 0; body=%s", body)
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
	// F-63 mutual exclusion: full back-part line, no "🤖 Working" prefix.
	want := "💭 3 · 🔧 12 · ⏱ 14:35:22"
	if !strings.Contains(body, want) {
		t.Fatalf("missing full header line; want %q in body:\n%s", want, body)
	}
	if strings.Contains(body, "🤖 Working") {
		t.Fatalf("header must omit 🤖 Working prefix when activity > 0; body=%s", body)
	}
}

// TestBuildReceiptCard_HeartbeatHeader_LastBeatOnly pins the
// F-63 mutual-exclusion edge: a snapshot with LastBeatAt set
// but BOTH counters zero is treated as "no activity" (front
// part), so the time chip must NOT bleed through to the body.
// The old !hb.Empty() gate would have rendered
// "🤖 Working · ⏱ 14:35:22" here; the strict ThinkCount/ToolCount
// gate instead routes to the bare "🤖 Working" placeholder.
func TestBuildReceiptCard_HeartbeatHeader_LastBeatOnly(t *testing.T) {
	now := time.Date(2026, 8, 15, 14, 35, 22, 0, time.UTC)
	body, _, err := buildReceiptCard(nil, nil, nil, &messages.HeartbeatSnapshot{
		LastBeatAt: now,
	})
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if !strings.Contains(body, "🤖 Working") {
		t.Fatalf("missing 🤖 Working (LastBeatAt-only counts as no activity → front part); body=%s", body)
	}
	if strings.Contains(body, "💭") || strings.Contains(body, "🔧") {
		t.Fatalf("unexpected counter chips for LastBeatAt-only; body=%s", body)
	}
	// F-63: counts == 0 means the time chip must stay suppressed
	// (the "front" half owns this state). A regression that
	// re-introduces the !hb.Empty() gate would render
	// "🤖 Working · ⏱ 14:35:22" and fail this assertion.
	if strings.Contains(body, "⏱") {
		t.Fatalf("LastBeatAt-only snapshot must not emit ⏱ (no activity → front part, no back part); body=%s", body)
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

	// F-63: back-part line shape is "💭 1 · 🔧 1 · ⏱ HH:MM:SS" (no
	// "🤖 Working" prefix when activity > 0). Find the heartbeat
	// line by its first non-zero counter chip.
	hbIdx := strings.Index(body, "💭 1")
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
	if strings.Contains(body, "🤖 Working") {
		t.Fatalf("back-part header must omit 🤖 Working prefix; body=%s", body)
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
	// F-63 mutual exclusion: renderHeartbeatHeader emits ONLY the
	// back part; the "🤖 Working" front part is added by the
	// buildReceiptCard switch when no activity is present. Verify
	// the back-part shape directly.
	want := "💭 2 · 🔧 5 · ⏱ 14:35:22"
	if got != want {
		t.Fatalf("renderHeartbeatHeader = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, "🤖 Working") {
		t.Fatalf("renderHeartbeatHeader must not start with 🤖 Working prefix; got %q", got)
	}

	// Empty / nil paths. With mutual exclusion in force, an empty
	// snapshot (counts all zero, no LastBeatAt) yields NO parts to
	// join → "". The "🤖 Working" front part is the caller's
	// (buildReceiptCard's) job, NOT renderHeartbeatHeader's.
	if got := renderHeartbeatHeader(nil); got != "" {
		t.Fatalf("renderHeartbeatHeader(nil) = %q, want \"\"", got)
	}
	if got := renderHeartbeatHeader(&messages.HeartbeatSnapshot{}); got != "" {
		t.Fatalf("renderHeartbeatHeader(empty) = %q, want \"\" (no parts to join)", got)
	}
}

// TestBuildReceiptCard_HeartbeatHeader_MutualExclusion pins
// the F-63 §3.6 four-way contract end-to-end through buildReceiptCard:
//   (a) hb counts all zero + entries/tasks empty → "🤖 Working" (front)
//   (b) hb ThinkCount>0 + ToolCount==0 → "💭 N · ⏱ HH:MM:SS" (back, no front)
//   (c) hb ThinkCount==0 + ToolCount>0 → "🔧 M · ⏱ HH:MM:SS" (back, no front)
//   (d) hb ThinkCount>0 + ToolCount>0 + LastBeatAt → full back line, no front
//   (e) hb counts all zero but entries non-empty → no header at all
// At no point do (a) and (b/c/d) ever produce a body that contains
// BOTH "🤖 Working" AND a "💭 "/"🔧 " counter chip.
func TestBuildReceiptCard_HeartbeatHeader_MutualExclusion(t *testing.T) {
	now := time.Date(2026, 8, 15, 14, 35, 22, 0, time.UTC)

	type tc struct {
		name    string
		entries []LogEntry
		tasks   []agent.AgentTaskItem
		hb      *messages.HeartbeatSnapshot
		wantHas []string // substrings the body MUST contain
		wantNot []string // substrings the body MUST NOT contain
	}
	cases := []tc{
		{
			name:    "front: empty snapshot, no entries/tasks",
			entries: nil, tasks: nil,
			hb: &messages.HeartbeatSnapshot{},
			wantHas: []string{"🤖 Working"},
			wantNot: []string{"💭 ", "🔧 ", "⏱ "},
		},
		{
			name:    "back: think only",
			entries: nil, tasks: nil,
			hb: &messages.HeartbeatSnapshot{ThinkCount: 1, LastBeatAt: now},
			wantHas: []string{"💭 1", "⏱ 14:35:22"},
			wantNot: []string{"🤖 Working", "🔧 "},
		},
		{
			name:    "back: tool only",
			entries: nil, tasks: nil,
			hb: &messages.HeartbeatSnapshot{ToolCount: 1, LastBeatAt: now},
			wantHas: []string{"🔧 1", "⏱ 14:35:22"},
			wantNot: []string{"🤖 Working", "💭 "},
		},
		{
			name:    "back: think + tool + time",
			entries: nil, tasks: nil,
			hb:      &messages.HeartbeatSnapshot{ThinkCount: 2, ToolCount: 3, LastBeatAt: now},
			wantHas: []string{"💭 2", "🔧 3", "⏱ 14:35:22"},
			wantNot: []string{"🤖 Working"},
		},
		{
			// F-63 strict-gate edge: LastBeatAt set but counts == 0.
			// Old !hb.Empty() gate would render "🤖 Working · ⏱ ..."
			// (front + time). New gate routes to the bare front part —
			// the time chip is suppressed. This is the case that
			// TestBuildReceiptCard_HeartbeatHeader_LastBeatOnly
			// covers in single-test form; the table entry pins it
			// alongside the rest of the contract.
			name:    "front: LastBeatAt-only (counts == 0, no entries/tasks)",
			entries: nil, tasks: nil,
			hb:      &messages.HeartbeatSnapshot{LastBeatAt: now},
			wantHas: []string{"🤖 Working"},
			wantNot: []string{"💭 ", "🔧 ", "⏱ "},
		},
		{
			name:    "no header: empty snapshot but entries present",
			entries: []LogEntry{{Icon: "💬", Text: "first reply"}}, tasks: nil,
			hb: &messages.HeartbeatSnapshot{},
			wantHas: []string{"first reply"},
			wantNot: []string{"🤖 Working", "💭 ", "🔧 ", "⏱ "},
		},
		{
			// nil hb + no entries/tasks → front part (same as the
			// "empty snapshot" case but exercising the hb == nil
			// branch of the switch).
			name:    "front: nil hb, no entries/tasks",
			entries: nil, tasks: nil,
			hb:      nil,
			wantHas: []string{"🤖 Working"},
			wantNot: []string{"💭 ", "🔧 ", "⏱ "},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, _, err := buildReceiptCard(c.entries, c.tasks, nil, c.hb)
			if err != nil {
				t.Fatalf("buildReceiptCard: %v", err)
			}
			for _, want := range c.wantHas {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q\nbody: %s", want, body)
				}
			}
			for _, ban := range c.wantNot {
				if strings.Contains(body, ban) {
					t.Errorf("body unexpectedly contains %q\nbody: %s", ban, body)
				}
			}
		})
	}
}

// TestRenderHeartbeatHeader_OmitsWorkingPrefix is the direct
// guarantee on the contract boundary: renderHeartbeatHeader is
// NEVER allowed to start its output with "🤖 Working". The
// "🤖 Working" placeholder is the buildReceiptCard front-part
// case's job; renderHeartbeatHeader only owns the back-part
// shape. A regression that re-introduces the prefix would
// surface as both halves rendered side-by-side (breaking §3.6
// mutual exclusion).
func TestRenderHeartbeatHeader_OmitsWorkingPrefix(t *testing.T) {
	now := time.Date(2026, 8, 15, 14, 35, 22, 0, time.UTC)
	cases := []messages.HeartbeatSnapshot{
		{},
		{ThinkCount: 1, LastBeatAt: now},
		{ToolCount: 1, LastBeatAt: now},
		{ThinkCount: 1, ToolCount: 1, LastBeatAt: now},
		{LastBeatAt: now},
	}
	for _, snap := range cases {
		got := renderHeartbeatHeader(&snap)
		if strings.HasPrefix(got, "🤖 Working") {
			t.Errorf("renderHeartbeatHeader must not start with 🤖 Working; got %q for snap=%+v", got, snap)
		}
		if strings.Contains(got, "🤖 Working") {
			t.Errorf("renderHeartbeatHeader must not contain 🤖 Working at all; got %q for snap=%+v", got, snap)
		}
	}
}

// TestRenderHeartbeatHeader_NoLastBeat pins the
// zero-LastBeatAt case for the back part: when ThinkCount/ToolCount
// are non-zero but LastBeatAt is the zero time, the time chip
// must be omitted (the function only appends a "⏱ ..." part
// when !hb.LastBeatAt.IsZero()). This case is unreachable in
// production — runtime's HeartbeatTracker.Observe stamps
// LastBeatAt on every event — but renderHeartbeatHeader is a
// public function, so the contract gets a sanity test.
func TestRenderHeartbeatHeader_NoLastBeat(t *testing.T) {
	cases := []struct {
		name string
		snap messages.HeartbeatSnapshot
		want string
	}{
		{
			name: "think only, no beat",
			snap: messages.HeartbeatSnapshot{ThinkCount: 2},
			want: "💭 2",
		},
		{
			name: "tool only, no beat",
			snap: messages.HeartbeatSnapshot{ToolCount: 7},
			want: "🔧 7",
		},
		{
			name: "both, no beat",
			snap: messages.HeartbeatSnapshot{ThinkCount: 1, ToolCount: 3},
			want: "💭 1 · 🔧 3",
		},
		{
			name: "zero everything → empty",
			snap: messages.HeartbeatSnapshot{},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderHeartbeatHeader(&c.snap)
			if got != c.want {
				t.Errorf("renderHeartbeatHeader = %q, want %q", got, c.want)
			}
			if strings.Contains(got, "⏱") {
				t.Errorf("renderHeartbeatHeader must omit ⏱ when LastBeatAt is zero; got %q", got)
			}
		})
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

	// PATCH body must contain the heartbeat header (back part
	// only — no "🤖 Working" prefix when activity > 0).
	last := bot.patches[len(bot.patches)-1]
	if strings.Contains(last.Body, "🤖 Working") {
		t.Fatalf("PATCH body must omit 🤖 Working prefix when activity > 0: %s", last.Body)
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
	// F-63 mutual exclusion: back-part header is counters + time,
	// no "🤖 Working" prefix. Verify the prefix is NOT there, and
	// the back-part chips are.
	if strings.Contains(first, "🤖 Working") {
		t.Fatalf("first element must omit 🤖 Working prefix when activity > 0: %q", first)
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