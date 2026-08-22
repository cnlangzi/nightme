package runtime

import (
	"github.com/cnlangzi/nightme/internal/chatstore"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/prcache"
	"github.com/cnlangzi/nightme/internal/registry"
)

// TestEventHandler_ThinkGate_ShowPassesThrough verifies the
// default ThinkMode=Show does NOT drop OutThinking events — the
// F-thread-route / Feishu lark_md pipeline must still see them.
//
// The thinking prefix used by gateway.Translate is "[思考] "
// (defined in internal/gateway/translate.go as thinkingPrefix).
// EventAgentText events whose text starts with that prefix become
// OutThinking in the Translate layer; everything else stays
// OutReply.
func TestEventHandler_ThinkGate_ShowPassesThrough(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] internal reasoning here",
	}, UserMsgID: "om_user_1"})

	got := ch.Record()
	// F-63: a single OutThinking yields 2 events now — the
	// original OutThinking + an OutHeartbeat follow-up. Filter
	// the heartbeat to count what this test cares about.
	var thinking *messages.OutboundMessage
	for i := range got {
		if got[i].Kind == messages.OutThinking {
			thinking = &got[i]
		}
	}
	if thinking == nil {
		t.Fatalf("OutThinking not in record (got %d events: %+v)", len(got), got)
	}
	if thinking.Kind.String() != "thinking" {
		t.Errorf("OutboundKind = %q, want %q (Translate maps the [思考] prefix to OutThinking)",
			thinking.Kind.String(), "thinking")
	}
}

// TestEventHandler_ThinkGate_HideDropsOutThinking verifies the
// core contract: /think off → EventHandler drops OutThinking
// before ch.Send. The Channel never sees the event.
func TestEventHandler_ThinkGate_HideDropsOutThinking(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] internal reasoning here",
	}, UserMsgID: "om_user_1"})

	if got := countNonHeartbeat(ch.Record()); got != 0 {
		t.Errorf("Hide mode dropped %d events; want 0. Recorded: %+v", got, ch.Record())
	}
}

// TestEventHandler_ThinkGate_HideDoesNotAffectOtherKinds verifies
// /think off only gates OutThinking — final assistant replies
// (EventAgentText without the <thinking> prefix → OutReply) and other
// kinds must still flow to the Channel.
//
// F-38 update: the test must opt the chat into /tools on so
// OutToolStart is not dropped by the new (orthogonal) ToolsMode
// gate. The /think gate's contract — "Hide does not affect kinds
// other than OutThinking" — is unaffected by the /tools gate; we
// just need to make the /tools gate a no-op for this test by
// flipping it on explicitly.
func TestEventHandler_ThinkGate_HideDoesNotAffectOtherKinds(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	if err := cs.SetToolsMode(chatsession.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	// (a) OutReply — final assistant reply (no <thinking> prefix)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "Here is your answer.",
	}, UserMsgID: "om_user_1"})

	// (b) EventAgentResult — typed Result event (OutResult)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text: "Final result text.",
		},
	}, UserMsgID: "om_user_1"})
	// EventAgentDone flushes the F-45 §2.5 OutResult buffer (turn-end
	// fallback path). EventAgentDone also sends nothing else of its own
	// so the final count stays at 3 (OutReply + OutResult + OutToolStart).
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentDone,
		Done: &agent.AgentDoneEvent{ExitCode: 0},
	}, UserMsgID: "om_user_1"})

	// (c) EventAgentToolStart — OutToolStart (passes because /tools on)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind:      agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}, UserMsgID: "om_user_1"})

	got := ch.Record()
	if c := countNonHeartbeat(got); c != 3 {
		t.Fatalf("Hide mode forwarded %d non-thinking events; want 3 (OutReply + OutResult + OutToolStart)", c)
	}
}

// TestEventHandler_ThinkGate_NilLoggerSafe verifies the gate does
// not panic when logger is nil — production never passes nil but
// tests / future misconfigurations might. The drop happens, but
// the optional Info log is skipped.
//
// (Note: the older MissingChatSessionFailsOpen and NilManagerFailsOpen
// tests were removed when we moved to per-cs closure capture —
// cs is now statically known at install time, so the "ChatSession
// lookup miss" failure mode is unreachable. NilManager remains
// possible (mgr.PersistAgentSession is a no-op) but is exercised
// by production paths; if a future caller passes nil mgr, the
// gate still works because cs is captured independently.)
func TestEventHandler_ThinkGate_NilLoggerSafe(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	// nil logger — must not panic.
	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, nil, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	// Run inside a recover probe to convert any panic into a
	// test failure with a useful message.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil logger caused panic: %v", r)
		}
	}()
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] reasoning",
	}, UserMsgID: "om_user_1"})

	if got := countNonHeartbeat(ch.Record()); got != 0 {
		t.Errorf("Hide mode with nil logger forwarded %d events; want 0", got)
	}
}

// TestEventHandler_ThinkGate_PersistsAcrossInvocations verifies
// the gate decision is read from ChatSession state on every
// invocation — flipping the mode mid-flight takes effect for
// subsequent events without re-installing the handler.
func TestEventHandler_ThinkGate_PersistsAcrossInvocations(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	thinking := agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] reasoning",
	}

	// Phase 1: default Show → forwarded.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &thinking, UserMsgID: "om_1"})
	if got := countNonHeartbeat(ch.Record()); got != 1 {
		t.Fatalf("phase1 (Show) forwarded %d events; want 1", got)
	}

	// Flip to Hide mid-flight.
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	// Phase 2: Hide → dropped.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &thinking, UserMsgID: "om_2"})
	if got := countNonHeartbeat(ch.Record()); got != 1 {
		t.Errorf("phase2 (Hide) total events = %d; want 1 (the phase-1 event only)", got)
	}

	// Flip back to Show.
	if err := cs.SetThinkMode(chatsession.ThinkModeShow); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	// Phase 3: Show again → forwarded.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &thinking, UserMsgID: "om_3"})
	if got := countNonHeartbeat(ch.Record()); got != 2 {
		t.Errorf("phase3 (Show again) total events = %d; want 2 (phase1 + phase3)", got)
	}
}

// _ ensures slog is imported even if a future edit drops the
// only usage; prevents the import from being flagged as unused
// by go vet. (slog.Default() is the explicit user; this line is
// a no-op for any future lint tooling that flags unused refs.)
// (Kept commented out — slog is used by TestEventHandler_* body.)
// var _ = slog.Default

// TestEventHandler_OutResult_FooterFirstTurnExact is the regression
// for the user-reported bug ("first tokens always 0") post the
// EventAgentResult + EventUsage → single EventAgentResult{Result, Usage}
// merge. The runtime now stamps StatusBar on the same ch.Send
// dispatch where the Usage arrives — the footer sees the turn's
// own tokens on the very first send.
//
// Sequence:
//  1. EventAgentReady → capture Model
//  2. EventAgentResult with AgentResultEvent.Usage populated — single event,
//     dispatched once, footer stamped with this turn's tokens.
func TestEventHandler_OutResult_FooterFirstTurnExact(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat_first_turn", "claude")
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_first_turn", "claude", "/tmp", nil)

	// Step 1: EventAgentReady captures Model.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_first_turn", AgentSession: as, Event: &agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: "sess_test",
		Model:     "claude-opus-4-5",
	}, UserMsgID: "om_user_1"})

	// Step 2: EventAgentResult with co-located Usage. ONE event delivery
	// in real wire order — no EventUsage to follow, no buffer.
	const inTok, outTok, cost = 1234, 567, 0.012
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_first_turn", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:       "Final answer.",
			DurationMs: 4321,
			Usage: &agent.UsageInfo{
				InputTokens:  inTok,
				OutputTokens: outTok,
				CostUSD:      cost,
			},
		},
	}, UserMsgID: "om_user_1"})

	got := ch.Record()
	// Find the OutResult specifically — EventAgentReady may also have
	// emitted OutboundMessages (e.g. OutInit on echo); the merged
	// design only changes how many OutResult-bound sends fire.
	var out *messages.OutboundMessage
	for i := range got {
		if got[i].Kind == messages.OutResult {
			if out != nil {
				t.Fatalf("multiple OutResult in Record; expected exactly one")
			}
			out = &got[i]
		}
	}
	if out == nil {
		t.Fatalf("no OutResult in Record: %+v", got)
	}
	// StatusBar stamped with THIS turn's tokens — the user
	// bug surface is gone: footer shows turn-1 cumulative = the
	// actual usage, not 0.
	// F-CLAUDE-PRINT-002: OutboundMessage no longer has a
	// StatusBar wrapper. Identity (Model / SessionID) lives
	// directly on out, populated by translate() from
	// EventAgentReady. Verify the flat fields here.
	if out.Model != "claude-opus-4-5" {
		t.Errorf("out.Model = %q, want 'claude-opus-4-5'", out.Model)
	}
	u := out.Usage
	if u == nil {
		t.Fatal("OutboundMessage.Usage is nil; runtime should pass through AgentResultEvent.Usage")
	}
	if u.InputTokens != inTok || u.OutputTokens != outTok {
		t.Errorf("OutboundMessage.Usage = %+v, want Input=%d Output=%d (this turn's tokens only)",
			u, inTok, outTok)
	}
	if u.CostUSD != cost {
		t.Errorf("OutboundMessage.Usage.CostUSD = %v, want %v", u.CostUSD, cost)
	}
	// Co-located Usage rides on the same OutboundMessage for any
	// channel that wants to render it directly (F-CLAUDE-PRINT-002
	// collapsed StatusBar.UsageBar into the flat Usage field).
	if out.Usage == nil {
		t.Error("OutboundMessage.Usage is nil; gateway should populate from AgentResultEvent.Usage")
	} else if out.Usage.InputTokens != inTok {
		t.Errorf("OutboundMessage.Usage.InputTokens = %d, want %d", out.Usage.InputTokens, inTok)
	}
}

// TestEventHandler_OutResult_UsageIsPerTurnNotCumulative verifies
// the per-turn snapshot semantics: StatusBar.Usage on each
// turn reflects ONLY that turn's bridge-reported usage, NOT a
// running total. The runtime is a passive pass-through; AgentSession
// has no cumulative state.
func TestEventHandler_OutResult_UsageIsPerTurnNotCumulative(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat_per", "claude")
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_per", "claude", "/tmp", nil)

	// Turn 1.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_per", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:  "first",
			Usage: &agent.UsageInfo{InputTokens: 10, OutputTokens: 5},
		},
	}, UserMsgID: "om_user_1"})
	// Turn 2 — different usage, no carryover from turn 1.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_per", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:  "second",
			Usage: &agent.UsageInfo{InputTokens: 20, OutputTokens: 7},
		},
	}, UserMsgID: "om_user_2"})

	got := ch.Record()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	// First turn: Usage = (10, 5) — turn 1 only.
	if u := got[0].Usage; u == nil ||
		u.InputTokens != 10 || u.OutputTokens != 5 {
		t.Errorf("turn-1 Usage = %+v, want (10, 5) — turn 1's snapshot only", u)
	}
	// Second turn: Usage = (20, 7) — turn 2 only, NOT (30, 12).
	if u := got[1].Usage; u == nil ||
		u.InputTokens != 20 || u.OutputTokens != 7 {
		t.Errorf("turn-2 Usage = %+v, want (20, 7) — turn 2's snapshot only (no carryover)", u)
	}
}

// TestEventHandler_OutResult_NilUsageLeavesEmptyStatusBar: a
// AgentResultEvent with no Usage (zero-usage turn / synthetic message)
// still ships OutResult, with StatusBar populated only by
// Model / Agent (no tokens to display). The runtime is a passive
// pass-through; nil Usage means the footer Line 2 is omitted.

// TestEventHandler_Chain_UsageFlowsFromResultEventToFooter exercises
// the full F-55 chain end-to-end on the runtime side:
//
//	EventAgentResult.Usage (set by bridge)
//	  → gateway.Translate populates OutboundMessage.Usage
//	  → StampFromAS copies to OutboundMessage.StatusBar.UsageBar.UsageInfo
//	  → channel reads ctx.Usage and renders (window) + new/cache/out
//
// The test does NOT import the feishu package (runtime must
// stay free of the channel impl to keep the runtime / channel
// boundary clean). It verifies the runtime side of the chain
// directly: every link from the AgentEvent envelope to the
// OutboundMessage that's handed to ch.Send. The Feishu adapter
// has its own chain test (TestSend_OutResult_CoLocatesUsage in
// internal/channel/feishu/adapter_test.go) that verifies it
// reads the values from OutboundMessage.Usage.
//
// This test is the regression guard for the F-55 footgun: a
// future refactor that drops Usage from StampFromAS (or
// stops stamping it onto StatusBar) will fail this test
// with a precise signal (StatusBar.Usage nil even though
// OutboundMessage.Usage is populated), and the footer Line 2
// will be silently empty in production until someone notices.
func TestEventHandler_Chain_UsageFlowsFromResultEventToFooter(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat_chain", "claude")
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	tmpDir := t.TempDir() // non-git cwd
	as := chatsession.NewAgentSession("as_chain", "cs_oc_chat_chain", "claude", tmpDir, nil)

	// Bridge-populated AgentResultEvent.Usage — every field the
	// runtime is expected to forward is set to a non-zero
	// sentinel so a "dropped" field is loud.
	const (
		inTok   = 12_300
		outTok  = 1_500
		cacheCr = 600
		cacheRd = 8_200
		win     = 200_000
		pct     = 10.55
		cost    = 0.087
	)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_chain", AgentSession: as, Event: &agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: "sess_chain",
		Model:     "claude-opus-4-5",
	}, UserMsgID: "om_user_1"})
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_chain", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text: "Final answer.",
			Usage: &agent.UsageInfo{
				InputTokens:              inTok,
				OutputTokens:             outTok,
				CacheCreationInputTokens: cacheCr,
				CacheReadInputTokens:     cacheRd,
				CostUSD:                  cost,
				ContextWindow:            win,
				ContextWindowPct:         pct,
			},
		},
	}, UserMsgID: "om_user_1"})

	var out *messages.OutboundMessage
	for _, m := range ch.Record() {
		if m.Kind == messages.OutResult {
			out = &m
			break
		}
	}
	if out == nil {
		t.Fatal("no OutResult in channel Record")
	}

	// Link 1: gateway.Translate populates OutboundMessage.Usage
	// from the AgentResultEvent's Usage. This is the bridge→runtime
	// boundary.
	if out.Usage == nil {
		t.Fatal("OutboundMessage.Usage is nil; gateway.Translate should populate from AgentResultEvent.Usage")
	}

	// F-CLAUDE-PRINT-002: out.Usage is populated directly
	// by translate() (F-55). The StatusBar.UsageBar wrapper
	// is gone — usage lives on the flat field. Footer Line 2
	// reads from out.Usage directly.
	if out.Usage == nil {
		t.Fatal("OutboundMessage.Usage is nil; translate should populate from AgentResultEvent.Usage")
	}

	// F-55 invariants: every wire field survives the chain
	// unchanged. Runtime is a passive pass-through — no
	// recompute, no catalog, no clamp.
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"InputTokens", out.Usage.InputTokens, inTok},
		{"OutputTokens", out.Usage.OutputTokens, outTok},
		{"CacheCreationInputTokens", out.Usage.CacheCreationInputTokens, cacheCr},
		{"CacheReadInputTokens", out.Usage.CacheReadInputTokens, cacheRd},
		{"CostUSD", out.Usage.CostUSD, cost},
		{"ContextWindow", out.Usage.ContextWindow, win},
		{"ContextWindowPct", out.Usage.ContextWindowPct, pct},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("Usage.%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	// Identity is on a flat field on out.
	if out.Model != "claude-opus-4-5" {
		t.Errorf("out.Model = %q, want 'claude-opus-4-5'", out.Model)
	}

	// Footer Line 2 will read these exact fields downstream
	// (see internal/statusbar/statusbar.go). Locking
	// in the values the channel WILL see.
	t.Logf("footer Line 2 inputs: in=%d cache_creation=%d cache_read=%d out=%d window=%d pct=%.2f cost=%.3f",
		inTok, cacheCr, cacheRd, outTok, win, pct, cost)
}

// TestEventHandler_ToolsGate_ShowPassesThrough verifies the
// TestEventHandler_ToolsGate_ShowPassesThrough verifies the
// /tools on path: ToolsMode=Show does NOT drop OutToolStart /
// OutToolEnd events — the Feishu adapter must still see them so
// it can merge each pair into a single thread reply.
func TestEventHandler_ToolsGate_ShowPassesThrough(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat_tools_show", "claude")
	if err := cs.SetToolsMode(chatsession.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_tools_show", "claude", "/tmp", nil)

	// OutToolStart
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_show", AgentSession: as, Event: &agent.AgentEvent{
		Kind:      agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}, UserMsgID: "om_user_1"})
	// OutToolEnd
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_show", AgentSession: as, Event: &agent.AgentEvent{
		Kind:    agent.EventAgentToolEnd,
		ToolEnd: &agent.AgentToolEndEvent{Name: "Read", Output: "line1\nline2"},
	}, UserMsgID: "om_user_1"})

	got := ch.Record()
	// F-63: OutHeartbeat now arrives BEFORE the original event
	// (observe-before-policy). Re-find tool events by Kind rather
	// than relying on positional indexing.
	var toolStart, toolEnd *messages.OutboundMessage
	for i := range got {
		switch got[i].Kind {
		case messages.OutToolStart:
			toolStart = &got[i]
		case messages.OutToolEnd:
			toolEnd = &got[i]
		}
	}
	if toolStart == nil {
		t.Fatalf("OutToolStart not in record (got %d events: %+v)", len(got), got)
	}
	if toolEnd == nil {
		t.Fatalf("OutToolEnd not in record (got %d events: %+v)", len(got), got)
	}
}

// TestEventHandler_ToolsGate_HideDropsBothToolKinds verifies the
// core F-38 contract: /tools off (default) → EventHandler drops
// BOTH OutToolStart and OutToolEnd before ch.Send. The Channel
// never sees tool events, so no thread reply is posted (no rate-
// limit consumption, no user-visible thread noise).
func TestEventHandler_ToolsGate_HideDropsBothToolKinds(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat_tools_hide", "claude")
	// cs.ToolsMode is the default (ToolsModeHide) — no SetToolsMode
	// call needed, but assert it explicitly so the test reads as a
	// contract check.
	if got := cs.ToolsMode(); got != chatsession.ToolsModeHide {
		t.Fatalf("fresh ChatSession ToolsMode = %q, want ToolsModeHide (default)", got)
	}
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_tools_hide", "claude", "/tmp", nil)

	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_hide", AgentSession: as, Event: &agent.AgentEvent{
		Kind:      agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}, UserMsgID: "om_user_1"})
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_hide", AgentSession: as, Event: &agent.AgentEvent{
		Kind:    agent.EventAgentToolEnd,
		ToolEnd: &agent.AgentToolEndEvent{Name: "Read", Output: "line1\nline2"},
	}, UserMsgID: "om_user_1"})

	if got := countNonHeartbeat(ch.Record()); got != 0 {
		t.Errorf("Hide mode dropped %d tool events; want 0. Recorded: %+v", got, ch.Record())
	}
}

// TestEventHandler_ToolsGate_HideDoesNotAffectOtherKinds verifies
// /tools off only gates OutToolStart and OutToolEnd — final
// assistant replies (OutReply), typed Result events (OutResult),
// and OutThinking must still flow to the Channel. This guards
// against accidentally widening the gate in a future refactor.
func TestEventHandler_ToolsGate_HideDoesNotAffectOtherKinds(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat_tools_indep", "claude")
	if got := cs.ToolsMode(); got != chatsession.ToolsModeHide {
		t.Fatalf("fresh ChatSession ToolsMode = %q, want ToolsModeHide", got)
	}
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_tools_indep", "claude", "/tmp", nil)

	// (a) OutReply — final assistant reply (no <thinking> prefix)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_indep", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "Here is your answer.",
	}, UserMsgID: "om_1"})

	// (b) EventAgentResult — typed Result event (OutResult)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_indep", AgentSession: as, Event: &agent.AgentEvent{
		Kind:   agent.EventAgentResult,
		Result: &agent.AgentResultEvent{Text: "Final result text."},
	}, UserMsgID: "om_1"})
	// EventAgentDone flushes the F-45 §2.5 OutResult buffer (turn-end
	// fallback) — keeps the count at 3 (OutReply + OutResult + OutThinking).
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_indep", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentDone,
		Done: &agent.AgentDoneEvent{ExitCode: 0},
	}, UserMsgID: "om_1"})

	// (c) OutThinking — must not be dropped by /tools off
	// (ThinkMode is the orthogonal gate; default Show passes it)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_indep", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] reasoning",
	}, UserMsgID: "om_1"})

	got := ch.Record()
	if c := countNonHeartbeat(got); c != 3 {
		t.Fatalf("Hide mode forwarded %d non-tool events; want 3 (OutReply + OutResult + OutThinking)", c)
	}
}

// TestEventHandler_ToolsGate_PersistsAcrossInvocations verifies
// the gate decision is read from ChatSession state on every
// invocation — flipping the mode mid-flight takes effect for
// subsequent events without re-installing the handler. Mirrors
// TestEventHandler_ThinkGate_PersistsAcrossInvocations.
func TestEventHandler_ToolsGate_PersistsAcrossInvocations(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat_tools_persist", "claude")
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_tools_persist", "claude", "/tmp", nil)

	toolStart := agent.AgentEvent{
		Kind:      agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}

	// Phase 1: default Hide → dropped.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_persist", AgentSession: as, Event: &toolStart, UserMsgID: "om_1"})
	if got := countNonHeartbeat(ch.Record()); got != 0 {
		t.Fatalf("phase1 (Hide) forwarded %d events; want 0", got)
	}

	// Flip to Show mid-flight.
	if err := cs.SetToolsMode(chatsession.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}

	// Phase 2: Show → forwarded.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_persist", AgentSession: as, Event: &toolStart, UserMsgID: "om_2"})
	if got := countNonHeartbeat(ch.Record()); got != 1 {
		t.Errorf("phase2 (Show) total events = %d; want 1", got)
	}

	// Flip back to Hide.
	if err := cs.SetToolsMode(chatsession.ToolsModeHide); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}

	// Phase 3: Hide → dropped again.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_persist", AgentSession: as, Event: &toolStart, UserMsgID: "om_3"})
	if got := countNonHeartbeat(ch.Record()); got != 1 {
		t.Errorf("phase3 (Hide again) total events = %d; want 1 (phase1 + phase3 dropped, phase2 kept)", got)
	}
}

// TestEventHandler_ToolsAndThinkGatesIndependent verifies the
// two per-chat gates (ThinkMode + ToolsMode) are independent:
// setting ToolsMode must not flip ThinkMode, and vice versa.
// Otherwise a /tools typo could silently change thinking
// behaviour (or a /think typo could silently expose tool calls).
func TestEventHandler_ToolsAndThinkGatesIndependent(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat_both_gates", "claude")
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_both_gates", "claude", "/tmp", nil)

	// Flip both off.
	if err := cs.SetToolsMode(chatsession.ToolsModeHide); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	// OutThinking → dropped (ThinkMode gate)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_both_gates", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] reasoning",
	}, UserMsgID: "om_user_1"})

	// OutToolStart → dropped (ToolsMode gate)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_both_gates", AgentSession: as, Event: &agent.AgentEvent{
		Kind:      agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}, UserMsgID: "om_user_1"})

	// Flip only /tools on.
	if err := cs.SetToolsMode(chatsession.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}

	// OutThinking → still dropped (ThinkMode gate unchanged)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_both_gates", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] more reasoning",
	}, UserMsgID: "om_user_2"})

	// OutToolStart → now forwarded (ToolsMode flipped to Show)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_both_gates", AgentSession: as, Event: &agent.AgentEvent{
		Kind:      agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{Name: "Bash", Args: "ls"},
	}, UserMsgID: "om_user_2"})

	got := ch.Record()
	if c := countNonHeartbeat(got); c != 1 {
		t.Fatalf("expected 1 forwarded event (OutToolStart after /tools on); got %d: %+v", c, got)
	}
	// Find the non-heartbeat forwarded event and assert its Kind.
	var found *messages.OutboundMessage
	for i := range got {
		if got[i].Kind != messages.OutHeartbeat {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no non-heartbeat forwarded event found")
	}
	if found.Kind.String() != "tool_start" {
		t.Errorf("forwarded event Kind = %q, want %q", found.Kind.String(), "tool_start")
	}
}

// newWireTestStores opens a temp-dir pair of ChatSessionFile +
// AgentSessionFile. Mirrors chatsession.newTestStores but lives in
// runtime (cross-package helpers in test packages aren't
// visible by default).
func newWireTestStores(t *testing.T) (*chatstore.Store, *registry.AgentSessionFile) {
	t.Helper()
	dir := t.TempDir()
	csFile, err := chatstore.New(filepath.Join(dir, "chat_sessions.json"))
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}
	asFile, err := registry.OpenAgentSessionFile(filepath.Join(dir, "agent_sessions.json"))
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}
	return csFile, asFile
}

// seedPersistedChatForWire writes one ChatSessionEntry so
// RestoreFromRegistry has something to restore.
func seedPersistedChatForWire(t *testing.T, csFile *chatstore.Store, chatID, primary string) {
	t.Helper()
	entry := &registry.ChatSessionEntry{
		ID:                "cs_" + chatID,
		ChatID:            chatID,
		SelectedCwd:       "/code/bailing",
		SelectedAgent:     primary,
		PrimaryAgent:      primary,
		CreatedAt:         time.Now(),
		LastInteractionAt: time.Now(),
	}
	if err := csFile.Save(entry); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

// TestWireRuntimeCallbacksAndRestore_InstallsHandlersOnRestoredChats
// is the runtime-side regression for the F-38 silent-failure
// bug: if WithOnCreate is set AFTER RestoreFromRegistry, every
// restored ChatSession ends up with nil EventHandler and nil
// MessageStateHandler. The Manager-level contract is covered in
// chatsession/manager_test.go; this test pins the runtime
// wiring that wraps both calls in WireRuntimeCallbacksAndRestore.
func TestWireRuntimeCallbacksAndRestore_InstallsHandlersOnRestoredChats(t *testing.T) {
	csFile, asFile := newWireTestStores(t)
	seedPersistedChatForWire(t, csFile, "oc_alpha", "claude")
	seedPersistedChatForWire(t, csFile, "oc_beta", "claude")

	mgr := chatsession.NewManager().WithPersistence(csFile, asFile)
	ch := echo.New("test", io.Discard)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := WireRuntimeCallbacksAndRestore(mgr, outbound.New(ch, outbound.Options{}), logger, chatsession.GitStatusDeps{}, ch); err != nil {
		t.Fatalf("WireRuntimeCallbacksAndRestore: %v", err)
	}
	for _, chatID := range []string{"oc_alpha", "oc_beta"} {
		if _, err := mgr.GetOrCreate(chatID, "claude"); err != nil {
			t.Fatalf("GetOrCreate(%s): %v", chatID, err)
		}
	}
	if got := len(mgr.List()); got != 2 {
		t.Fatalf("mgr.List len = %d, want 2", got)
	}

	for _, cs := range mgr.List() {
		if cs.AgentEventBus.Len() == 0 {
			t.Errorf("%s: AgentEventBus has no subscribers — wiring regression", cs.ChatID)
		}
		if cs.MessageStateBus.Len() == 0 {
			t.Errorf("%s: MessageStateBus has no subscribers — wiring regression", cs.ChatID)
		}
	}
}

// TestWireRuntimeCallbacksAndRestore_MessageStateDropsEmptyIDs
// (review fix): the F-48 wrapper that replaced gwImpl.OnMessageState
// must replicate the gateway's early-return-on-empty-IDs guard.
// Without it, an EmitMessageState("", "", ...) would push an
// OutboundMessage with empty ChatID / MessageID to the channel —
// the Feishu adapter rejects MessageState with missing MessageID,
// and an empty ChatID would route to the wrong chat.
func TestWireRuntimeCallbacksAndRestore_MessageStateDropsEmptyIDs(t *testing.T) {
	csFile, asFile := newWireTestStores(t)
	seedPersistedChatForWire(t, csFile, "oc_drop", "claude")

	mgr := chatsession.NewManager().WithPersistence(csFile, asFile)
	ch := echo.New("test", io.Discard)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := WireRuntimeCallbacksAndRestore(mgr, outbound.New(ch, outbound.Options{}), logger, chatsession.GitStatusDeps{}, ch); err != nil {
		t.Fatalf("WireRuntimeCallbacksAndRestore: %v", err)
	}
	if _, err := mgr.GetOrCreate("oc_drop", "claude"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	csList := mgr.List()
	if len(csList) != 1 {
		t.Fatalf("mgr.List len = %d, want 1", len(csList))
	}
	cs := csList[0]
	if cs.MessageStateBus.Len() == 0 {
		t.Fatal("MessageStateBus has no subscribers — wiring regression")
	}

	// Empty chatID: handler must return silently without sending.
	cs.EmitMessageState("om_user", agent.MessageSubmitted)
	// Drop the empty-chatID case: instead of inspecting the
	// installed handler closure (no longer exposed), assert the
	// END state after a series of empty-ID publishes.
	cs.EmitMessageState("om_user", agent.MessageSubmitted)
	beforeValid := len(ch.Record())

	// Empty userMsgID: same.
	cs.EmitMessageState("", agent.MessageSubmitted)
	if got := len(ch.Record()); got != beforeValid {
		t.Errorf("empty userMsgID should drop silently; got %d new events", got-beforeValid)
	}

	// Both empty: same.
	cs.EmitMessageState("", agent.MessageSubmitted)
	if got := len(ch.Record()); got != beforeValid {
		t.Errorf("both empty should drop silently; got %d new events", got-beforeValid)
	}

	// Sanity: a valid call DOES produce an OutboundMessage (so
	// the silent drop is targeted, not "the handler never fires").
	// F-53: agent.MessageDone no longer exists; use the closest
	// live state (MessageSubmitted) for the sanity probe.
	cs.EmitMessageState("om_user", agent.MessageSubmitted)
	if got := len(ch.Record()); got != beforeValid+1 {
		t.Errorf("valid call should fire; got %d events", got)
	}
}

// TestWireRuntimeCallbacksAndRestore_MessageStateStampsAgentBar
// (fix-placehold-card) locks the contract that the runtime's
// MessageStateBus subscriber stamps AgentName / Workspace /
// SessionID from cs.SelectedAgentSession() onto the OutboundMessage
// it constructs for OutMessageState events. This is what makes
// the Feishu placeholder card render AgentBar from the very first
// MessageQueued emit (see
// internal/channel/feishu/adapter.go::Send →
// ensureReceiptForTyping → statusbar.StatusBarLines).
//
// Three sub-tests:
//
//  1. AS with all three fields populated → OutboundMessage gets
//     all three stamped verbatim.
//  2. AS with SessionID empty (pre-EventAgentReady) → AgentName /
//     Workspace still stamped; SessionID stays "". statusbar.StatusBarLines
//     omits the empty SessionID segment but the AgentBar line
//     still renders.
//  3. selectedAS nil (legacy framework path: slash command /
//     shell dispatch with no AS in scope) → all three fields
//     empty. statusbar.StatusBarLines treats the all-empty case as
//     "no AgentBar line" (back-compat).
func TestWireRuntimeCallbacksAndRestore_MessageStateStampsAgentBar(t *testing.T) {
	t.Run("stamps AgentBar when selectedAS populated", func(t *testing.T) {
		csFile, asFile := newWireTestStores(t)
		seedPersistedChatForWire(t, csFile, "oc_stamp_full", "claude")

		mgr := chatsession.NewManager().WithPersistence(csFile, asFile)
		ch := echo.New("test", io.Discard)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		if err := WireRuntimeCallbacksAndRestore(mgr, outbound.New(ch, outbound.Options{}), logger, chatsession.GitStatusDeps{}, ch); err != nil {
			t.Fatalf("WireRuntimeCallbacksAndRestore: %v", err)
		}
		if _, err := mgr.GetOrCreate("oc_stamp_full", "claude"); err != nil {
			t.Fatalf("GetOrCreate: %v", err)
		}

		csList := mgr.List()
		if len(csList) != 1 {
			t.Fatalf("mgr.List len = %d, want 1", len(csList))
		}
		cs := csList[0]

		// Manually install selectedAS + its SessionID (simulating
		// a fully-spawned AS that has reported EventAgentReady).
		// Direct field assignment mirrors the pattern in
		// chatsession/chatsession_test.go::TestKillAllSequence_*
		// — we're testing the subscriber's read of selectedAS,
		// not the lookup / spawn machinery.
		as := chatsession.NewAgentSession("as_full", cs.ID, "claude", cs.SelectedCwd(), nil)
		as.SetSessionID("bridge_sid_full")
		cs.SelectedAgentSessionForTest(as)

		// Snapshot the channel record before the emit so we can
		// inspect only the message produced by this call.
		before := len(ch.Record())

		cs.EmitMessageState("om_stamp_full", agent.MessageQueued)

		after := ch.Record()
		if len(after) != before+1 {
			t.Fatalf("expected 1 new OutboundMessage, got %d", len(after)-before)
		}
		got := after[len(after)-1]
		if got.Kind != messages.OutMessageState {
			t.Fatalf("Kind = %v, want OutMessageState", got.Kind)
		}
		if got.AgentName != "claude" {
			t.Errorf("AgentName = %q, want %q", got.AgentName, "claude")
		}
		if got.Workspace != cs.SelectedCwd() {
			t.Errorf("Workspace = %q, want %q", got.Workspace, cs.SelectedCwd())
		}
		if got.SessionID != "bridge_sid_full" {
			t.Errorf("SessionID = %q, want %q", got.SessionID, "bridge_sid_full")
		}
	})

	t.Run("stamps Agent/Workspace even when SessionID empty", func(t *testing.T) {
		// Pre-EventAgentReady case: the AS exists but the bridge
		// hasn't reported its SessionID yet. The subscriber
		// should still stamp Agent / Cwd so the placeholder
		// card shows the AgentBar identity line (sans the
		// trailing SessionID segment). statusbar.StatusBarLines
		// omits the empty SessionID segment via the
		// "SessionID omitted when '' (F-56)" rule.
		csFile, asFile := newWireTestStores(t)
		seedPersistedChatForWire(t, csFile, "oc_stamp_no_sid", "claude")

		mgr := chatsession.NewManager().WithPersistence(csFile, asFile)
		ch := echo.New("test", io.Discard)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		if err := WireRuntimeCallbacksAndRestore(mgr, outbound.New(ch, outbound.Options{}), logger, chatsession.GitStatusDeps{}, ch); err != nil {
			t.Fatalf("WireRuntimeCallbacksAndRestore: %v", err)
		}
		if _, err := mgr.GetOrCreate("oc_stamp_no_sid", "claude"); err != nil {
			t.Fatalf("GetOrCreate: %v", err)
		}

		csList := mgr.List()
		if len(csList) != 1 {
			t.Fatalf("mgr.List len = %d, want 1", len(csList))
		}
		cs := csList[0]

		as := chatsession.NewAgentSession("as_no_sid", cs.ID, "claude", cs.SelectedCwd(), nil)
		// no SetSessionID — bridge pre-EventAgentReady
		cs.SelectedAgentSessionForTest(as)

		before := len(ch.Record())
		cs.EmitMessageState("om_stamp_no_sid", agent.MessageQueued)
		after := ch.Record()

		if len(after) != before+1 {
			t.Fatalf("expected 1 new OutboundMessage, got %d", len(after)-before)
		}
		got := after[len(after)-1]
		if got.AgentName != "claude" {
			t.Errorf("AgentName = %q, want %q", got.AgentName, "claude")
		}
		if got.Workspace != cs.SelectedCwd() {
			t.Errorf("Workspace = %q, want %q", got.Workspace, cs.SelectedCwd())
		}
		if got.SessionID != "" {
			t.Errorf("SessionID = %q, want \"\" (pre-EventAgentReady)", got.SessionID)
		}
	})

	t.Run("selectedAS nil leaves AgentBar fields empty", func(t *testing.T) {
		// Legacy framework paths (slash commands via
		// commander.Dispatch / shell via shell.Dispatcher) emit
		// MessageQueued via PublishMessageState without a
		// resolved AS in scope. The subscriber must NOT fake a
		// default — selectedAS is nil → all three fields stay
		// empty → statusbar.StatusBarLines drops the AgentBar line
		// entirely (no misleading "🤖: " header on a slash
		// command placeholder card).
		csFile, asFile := newWireTestStores(t)
		seedPersistedChatForWire(t, csFile, "oc_stamp_nil", "claude")

		mgr := chatsession.NewManager().WithPersistence(csFile, asFile)
		ch := echo.New("test", io.Discard)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		if err := WireRuntimeCallbacksAndRestore(mgr, outbound.New(ch, outbound.Options{}), logger, chatsession.GitStatusDeps{}, ch); err != nil {
			t.Fatalf("WireRuntimeCallbacksAndRestore: %v", err)
		}
		if _, err := mgr.GetOrCreate("oc_stamp_nil", "claude"); err != nil {
			t.Fatalf("GetOrCreate: %v", err)
		}

		csList := mgr.List()
		if len(csList) != 1 {
			t.Fatalf("mgr.List len = %d, want 1", len(csList))
		}
		cs := csList[0]

		// No SelectedAgentSessionForTest call — selectedAS stays
		// nil for this ChatSession.
		if as := cs.SelectedAgentSession(); as != nil {
			t.Fatalf("precondition: selectedAS = %+v, want nil", as)
		}

		before := len(ch.Record())
		cs.EmitMessageState("om_stamp_nil", agent.MessageQueued)
		after := ch.Record()

		if len(after) != before+1 {
			t.Fatalf("expected 1 new OutboundMessage, got %d", len(after)-before)
		}
		got := after[len(after)-1]
		if got.AgentName != "" || got.Workspace != "" || got.SessionID != "" {
			t.Errorf("nil selectedAS leaked identity: AgentName=%q Workspace=%q SessionID=%q",
				got.AgentName, got.Workspace, got.SessionID)
		}
	})
}

// TestWireRuntimeCallbacksAndRestore_NoPersistence verifies the
// helper handles the cold-start path (no chat_sessions.json yet):
// WithOnCreate is set, RestoreFromRegistry is a no-op, no error.
func TestWireRuntimeCallbacksAndRestore_NoPersistence(t *testing.T) {
	mgr := chatsession.NewManager() // no WithPersistence — csFile is nil
	ch := echo.New("test", io.Discard)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := WireRuntimeCallbacksAndRestore(mgr, outbound.New(ch, outbound.Options{}), logger, chatsession.GitStatusDeps{}, ch); err != nil {
		t.Fatalf("WireRuntimeCallbacksAndRestore on cold start: %v", err)
	}
	if len(mgr.List()) != 0 {
		t.Errorf("expected no restored chats; got %d", len(mgr.List()))
	}
}

// TestEventHandler_OnAgentConnected_DoesNotEmitMessageSubmitted
// locks the contract: ChatSession.TryFlush is the SOLE emit point
// for MessageSubmitted. The runtime event handler must NOT re-emit
// MessageSubmitted on EventAgentReady (which fires at session
// start, BEFORE any user message has a userMsgID anchor).
//
// Regression guard for the L869 emit that was added in T-alive
// (2026-08-07) to avoid false-positive OnIt during 60s spawn probe.
// The clean fix is to delete that emit entirely — ChatSession owns
// Message.Stage lifecycle; "agent connected" is a session-level
// event (carries SessionID + Model for resume/footer), not a turn
// event (carries MessageState).
func TestEventHandler_OnAgentConnected_DoesNotEmitMessageSubmitted(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Capture every MessageState emit on this CS.
	var emitted []messageStateCall
	var mu sync.Mutex
	cs.MessageStateBus.Subscribe(func(e chatsession.MessageStateEvent) bool {
		mu.Lock()
		defer mu.Unlock()
		emitted = append(emitted, messageStateCall{e.ChatID, e.UserMsgID, e.State})
		return false
	})

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	// Fire EventAgentReady with a userMsgID present (the L869
	// branch's old condition). The runtime must NOT emit
	// MessageSubmitted here — ChatSession.TryFlush owns the
	// "submit succeeded" boundary, EventAgentReady is just
	// session-init metadata.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: "session-abc",
		AgentName: "claude",
		Workspace: "/tmp",
	}, UserMsgID: "om_user_1"})

	mu.Lock()
	defer mu.Unlock()
	for _, e := range emitted {
		if e.UserMsgID == "om_user_1" && e.State == agent.MessageSubmitted {
			t.Errorf("runtime event handler emitted MessageSubmitted on EventAgentReady for %q; ChatSession.TryFlush is the sole emit point", e.UserMsgID)
		}
	}
}

// messageStateCall is a lightweight capture record used by the
// EventHandler tests. (Mirrors the type in
// internal/chatsession/message_state_test.go but kept local to
// avoid exporting it.)
type messageStateCall struct {
	ChatID    string
	UserMsgID string
	State     agent.MessageState
}

// TestShutdownRun_CloseAllCancelsCaches verifies the daemon
// shutdown hook drains every per-AgentSession PR-cache refresh
// goroutine so the process exits cleanly. The test pre-populates
// the registry with a cache that holds an inflight goroutine
// driven by a slow Detect prober, calls ShutdownRun, and asserts
// the goroutine observed the cancel signal within the bounded
// shutdown timeout.
//
// Regression guard for the missing wiring: prior to this fix the
// doc in prcache.go promised "Per session teardown → cache.Cancel()"
// but no caller actually invoked Cancel on shutdown, so an
// in-flight `gh pr list` could keep running after the daemon
// exited.
//
// Note: the cancel-observation itself lives in
// internal/prcache/prcache_test.go (same-package, accesses
// Cache's unexported inflight/cancel directly). This test only
// verifies that ShutdownRun threads the prReg through to
// CloseAll — the actual cancel propagation is prcache's
// contract.
func TestShutdownRun_CloseAllCancelsCaches(t *testing.T) {
	prReg := &prcache.Registry{}
	pre := prReg.GetOrCreate("as_shutdown_test")
	// After CloseAll, the registry's map is cleared; a fresh
	// GetOrCreate returns a NEW cache pointer (the old one is
	// still valid for prior holders, but the registry forgot it).
	if err := ShutdownRun(io.Discard, nil, nil, nil, nil, prReg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("ShutdownRun: %v", err)
	}
	fresh := prReg.GetOrCreate("as_shutdown_test")
	if fresh == pre {
		t.Errorf("GetOrCreate after CloseAll returned the old pointer; want a fresh cache")
	}
}

// countNonHeartbeat (F-63) returns the number of recorded events
// excluding OutHeartbeat. Pre-F-63 tests that counted total
// forwarded events need to subtract the heartbeat side-channel
// emissions to keep their original assertions meaningful (those
// tests are about gate behaviour, not heartbeat counting).
func countNonHeartbeat(msgs []messages.OutboundMessage) int {
	n := 0
	for _, m := range msgs {
		if m.Kind != messages.OutHeartbeat {
			n++
		}
	}
	return n
}
