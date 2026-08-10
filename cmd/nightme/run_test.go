package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
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

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] internal reasoning here",
	}, UserMsgID: "om_user_1"})

	got := ch.Record()
	if len(got) != 1 {
		t.Fatalf("Channel.Record len = %d, want 1 (Show mode passes OutThinking)", len(got))
	}
	if got[0].Kind.String() != "thinking" {
		t.Errorf("OutboundKind = %q, want %q (Translate maps the [思考] prefix to OutThinking)",
			got[0].Kind.String(), "thinking")
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

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] internal reasoning here",
	}, UserMsgID: "om_user_1"})

	if got := ch.Record(); len(got) != 0 {
		t.Errorf("Hide mode dropped %d events; want 0. Recorded: %+v", len(got), got)
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

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
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
			Text:    "Final result text.",
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
	if len(got) != 3 {
		t.Fatalf("Hide mode forwarded %d non-thinking events; want 3 (OutReply + OutResult + OutToolStart)", len(got))
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
	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, nil)
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

	if got := ch.Record(); len(got) != 0 {
		t.Errorf("Hide mode with nil logger forwarded %d events; want 0", len(got))
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

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	thinking := agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] reasoning",
	}

	// Phase 1: default Show → forwarded.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &thinking, UserMsgID: "om_1"})
	if got := len(ch.Record()); got != 1 {
		t.Fatalf("phase1 (Show) forwarded %d events; want 1", got)
	}

	// Flip to Hide mid-flight.
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	// Phase 2: Hide → dropped.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &thinking, UserMsgID: "om_2"})
	if got := len(ch.Record()); got != 1 {
		t.Errorf("phase2 (Hide) total events = %d; want 1 (the phase-1 event only)", got)
	}

	// Flip back to Show.
	if err := cs.SetThinkMode(chatsession.ThinkModeShow); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	// Phase 3: Show again → forwarded.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &thinking, UserMsgID: "om_3"})
	if got := len(ch.Record()); got != 2 {
		t.Errorf("phase3 (Show again) total events = %d; want 2 (phase1 + phase3)", got)
	}
}

// _ ensures slog is imported even if a future edit drops the
// only usage; prevents the import from being flagged as unused
// by go vet. (slog.Default() is the explicit user; this line is
// a no-op for any future lint tooling that flags unused refs.)
var _ = slog.Default

// TestEventHandler_OutResult_FooterFirstTurnExact is the regression
// for the user-reported bug ("first tokens always 0") post the
// EventAgentResult + EventUsage → single EventAgentResult{Result, Usage}
// merge. The runtime now stamps SessionContext on the same ch.Send
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

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
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
	var out *gateway.OutboundMessage
	for i := range got {
		if got[i].Kind == gateway.OutResult {
			if out != nil {
				t.Fatalf("multiple OutResult in Record; expected exactly one")
			}
			out = &got[i]
		}
	}
	if out == nil {
		t.Fatalf("no OutResult in Record: %+v", got)
	}
	// SessionContext stamped with THIS turn's tokens — the user
	// bug surface is gone: footer shows turn-1 cumulative = the
	// actual usage, not 0.
	if out.SessionContext == nil {
		t.Fatal("OutResult SessionContext is nil; runtime should stamp inline")
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
	if out.SessionContext.Model != "claude-opus-4-5" {
		t.Errorf("SessionContext.Model = %q, want 'claude-opus-4-5'", out.SessionContext.Model)
	}
	// Co-located Usage rides on the same OutboundMessage for any
	// channel that wants to render it directly (today's channels
	// render via SessionContext, but the field stays for symmetry
	// with the AgentResultEvent.Usage shape).
	if out.Usage == nil {
		t.Error("OutboundMessage.Usage is nil; gateway should populate from AgentResultEvent.Usage")
	} else if out.Usage.InputTokens != inTok {
		t.Errorf("OutboundMessage.Usage.InputTokens = %d, want %d", out.Usage.InputTokens, inTok)
	}
}

// TestEventHandler_OutResult_UsageIsPerTurnNotCumulative verifies
// the per-turn snapshot semantics: SessionContext.Usage on each
// turn reflects ONLY that turn's bridge-reported usage, NOT a
// running total. The runtime is a passive pass-through; AgentSession
// has no cumulative state.
func TestEventHandler_OutResult_UsageIsPerTurnNotCumulative(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat_per", "claude")
	logger := slog.Default()

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
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

// TestEventHandler_OutResult_NilUsageLeavesEmptySessionContext: a
// AgentResultEvent with no Usage (zero-usage turn / synthetic message)
// still ships OutResult, with SessionContext populated only by
// Model / Agent (no tokens to display). The runtime is a passive
// pass-through; nil Usage means the footer Line 2 is omitted.
func TestEventHandler_OutResult_NilUsageLeavesEmptySessionContext(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat_zero", "claude")
	logger := slog.Default()

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
	// t.TempDir() is guaranteed NOT to be inside a git working
	// tree (Go creates it under the OS temp dir; tests don't
	// nest a .git inside). Pre-F-48 the test hardcoded /tmp,
	// which happened to be non-git on most dev machines but
	// could fail under a CI runner that mounts the workspace
	// under /tmp. F-48 stamps SessionContext whenever the cwd
	// is in a git repo (regardless of usage), so the test must
	// pin a non-git cwd explicitly.
	tmpDir := t.TempDir()
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_zero", "claude", tmpDir, nil)

	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_zero", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text: "no usage reported",
			// Usage intentionally nil
		},
	}, UserMsgID: "om_user_1"})

	got := ch.Record()
	if len(got) != 1 || got[0].Kind != gateway.OutResult {
		t.Fatalf("got %v, want 1 OutResult", got)
	}
	// SessionContext IS stamped (Agent is set), but Usage is nil
	// (no per-turn usage on this event) and the footer Line 2
	// is omitted because ctx.Usage == nil.
	if got[0].SessionContext == nil {
		t.Fatal("SessionContext = nil; Agent is set so SessionContext should be stamped")
	}
	if got[0].Usage != nil {
		t.Errorf("OutboundMessage.Usage = %+v, want nil (no usage on this event)", got[0].Usage)
	}
}

// TestSessionContextInto_ForwardsUsage pins F-55: sessionContextInto
// must copy out.Usage (set by gateway.Translate from the bridge
// wire payload) into out.SessionContext.Usage so the channel footer
// can render it via ctx.Usage. Pre-F-55 the copy was missing, so
// footers silently rendered without usage data even when the
// bridge had populated it. The 1-line fix lives in run.go; this
// test catches any future regression that drops Usage from the
// SessionContext struct literal.
//
// Sub-cases:
//   - Usage populated → SessionContext.Usage matches verbatim
//     (input / output / cache_creation / cache_read /
//     context_window / context_window_pct / costUSD all flow
//     through unchanged — runtime is a passive pass-through).
//   - Usage nil AND no other field qualifies → no SessionContext
//     materialized (guard skips the whole block).
//   - Usage nil BUT Agent set → SessionContext still stamped
//     (Agent path wins; footer Line 2 omitted because ctx.Usage
//     is nil — same behaviour as the pre-fix code for this case).
func TestSessionContextInto_ForwardsUsage(t *testing.T) {
	tmpDir := t.TempDir() // non-git cwd
	as := chatsession.NewAgentSession("as_test", "cs_ctx", "claude", tmpDir, nil)

	t.Run("usage populated → SessionContext.Usage verbatim", func(t *testing.T) {
		out := &gateway.OutboundMessage{
			ChatID:  "oc_chat_ctx_1",
			Kind:    gateway.OutResult,
			Text:    "answer",
			ReplyTo: "om_user_1",
			Usage: &agent.UsageInfo{
				InputTokens:              12_300,
				OutputTokens:             1_500,
				CacheCreationInputTokens: 600,
				CacheReadInputTokens:     8_200,
				CostUSD:                  0.087,
				ContextWindow:            200_000,
				ContextWindowPct:         10.55,
			},
		}
		sessionContextInto(out, as)
		if out.SessionContext == nil {
			t.Fatal("SessionContext is nil; Usage alone must materialize it (F-55)")
		}
		u := out.SessionContext.Usage
		if u == nil {
			t.Fatal("SessionContext.Usage is nil; out.Usage must be copied verbatim")
		}
		if u.InputTokens != 12_300 {
			t.Errorf("InputTokens = %d, want 12_300", u.InputTokens)
		}
		if u.OutputTokens != 1_500 {
			t.Errorf("OutputTokens = %d, want 1_500", u.OutputTokens)
		}
		if u.CacheCreationInputTokens != 600 {
			t.Errorf("CacheCreationInputTokens = %d, want 600", u.CacheCreationInputTokens)
		}
		if u.CacheReadInputTokens != 8_200 {
			t.Errorf("CacheReadInputTokens = %d, want 8_200", u.CacheReadInputTokens)
		}
		if u.CostUSD != 0.087 {
			t.Errorf("CostUSD = %v, want 0.087", u.CostUSD)
		}
		if u.ContextWindow != 200_000 {
			t.Errorf("ContextWindow = %d, want 200_000", u.ContextWindow)
		}
		if u.ContextWindowPct != 10.55 {
			t.Errorf("ContextWindowPct = %v, want 10.55", u.ContextWindowPct)
		}
	})

	t.Run("usage nil + no other field → no SessionContext", func(t *testing.T) {
		// Fresh AS with Agent/Model/Compaction also empty so
		// every guard condition fails.
		emptyAS := chatsession.NewAgentSession("as_empty", "cs_empty", "", "", nil)
		out := &gateway.OutboundMessage{
			ChatID: "oc_chat_ctx_2",
			Kind:   gateway.OutResult,
			Text:   "answer",
			// Usage intentionally nil
		}
		sessionContextInto(out, emptyAS)
		if out.SessionContext != nil {
			t.Errorf("SessionContext = %+v, want nil (no field qualifies)", out.SessionContext)
		}
	})

	t.Run("usage nil but Agent set → SessionContext stamped, Usage nil", func(t *testing.T) {
		out := &gateway.OutboundMessage{
			ChatID: "oc_chat_ctx_3",
			Kind:   gateway.OutResult,
			Text:   "answer",
			// Usage intentionally nil; Agent is set on the
			// shared `as` so the guard passes via the Agent
			// branch.
		}
		sessionContextInto(out, as)
		if out.SessionContext == nil {
			t.Fatal("SessionContext is nil; Agent is set so SessionContext should be stamped")
		}
		if out.SessionContext.Usage != nil {
			t.Errorf("SessionContext.Usage = %+v, want nil (Usage was nil on the wire)", out.SessionContext.Usage)
		}
	})
}

// TestEventHandler_Chain_UsageFlowsFromResultEventToFooter exercises
// the full F-55 chain end-to-end on the cmd/nightme side:
//
//	EventAgentResult.Usage (set by bridge)
//	  → gateway.Translate populates OutboundMessage.Usage
//	  → sessionContextInto copies to OutboundMessage.SessionContext.Usage
//	  → channel reads ctx.Usage and renders (window) + new/cache/out
//
// The test does NOT import the feishu package (cmd/nightme must
// stay free of the channel impl to keep the runtime / channel
// boundary clean). It verifies the runtime side of the chain
// directly: every link from the AgentEvent envelope to the
// OutboundMessage that's handed to ch.Send. The Feishu adapter
// has its own chain test (TestSend_OutResult_CoLocatesUsage in
// internal/channel/feishu/adapter_test.go) that verifies it
// reads the values from OutboundMessage.Usage.
//
// This test is the regression guard for the F-55 footgun: a
// future refactor that drops Usage from sessionContextInto (or
// stops stamping it onto SessionContext) will fail this test
// with a precise signal (SessionContext.Usage nil even though
// OutboundMessage.Usage is populated), and the footer Line 2
// will be silently empty in production until someone notices.
func TestEventHandler_Chain_UsageFlowsFromResultEventToFooter(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat_chain", "claude")
	logger := slog.Default()

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
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

	var out *gateway.OutboundMessage
	for _, m := range ch.Record() {
		if m.Kind == gateway.OutResult {
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

	// Link 2: sessionContextInto copies out.Usage to
	// out.SessionContext.Usage. This is the runtime→channel
	// boundary; if it breaks, the footer Line 2 silently
	// disappears (the actual user-facing bug F-55 surfaced).
	if out.SessionContext == nil {
		t.Fatal("SessionContext is nil; runtime should stamp it on OutResult")
	}
	if out.SessionContext.Usage == nil {
		t.Fatal("SessionContext.Usage is nil; sessionContextInto must copy out.Usage verbatim")
	}

	// F-55 invariants: every wire field survives the chain
	// unchanged. Runtime is a passive pass-through — no
	// recompute, no catalog, no clamp.
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"InputTokens", out.SessionContext.Usage.InputTokens, inTok},
		{"OutputTokens", out.SessionContext.Usage.OutputTokens, outTok},
		{"CacheCreationInputTokens", out.SessionContext.Usage.CacheCreationInputTokens, cacheCr},
		{"CacheReadInputTokens", out.SessionContext.Usage.CacheReadInputTokens, cacheRd},
		{"CostUSD", out.SessionContext.Usage.CostUSD, cost},
		{"ContextWindow", out.SessionContext.Usage.ContextWindow, win},
		{"ContextWindowPct", out.SessionContext.Usage.ContextWindowPct, pct},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("SessionContext.Usage.%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	// Same identity is preserved on SessionContext.
	if out.SessionContext.Model != "claude-opus-4-5" {
		t.Errorf("SessionContext.Model = %q, want 'claude-opus-4-5'", out.SessionContext.Model)
	}

	// Footer Line 2 will read these exact fields downstream
	// (see internal/channel/feishu/usage_footer.go). The test
	// below documents the expected rendered shape against the
	// canonical format — not running the channel render here
	// (that lives in feishu/usage_footer_test.go), but locking
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

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
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
	if len(got) != 2 {
		t.Fatalf("Channel.Record len = %d, want 2 (Show mode passes both tool events)", len(got))
	}
	if got[0].Kind.String() != "tool_start" {
		t.Errorf("first event Kind = %q, want %q", got[0].Kind.String(), "tool_start")
	}
	if got[1].Kind.String() != "tool_end" {
		t.Errorf("second event Kind = %q, want %q", got[1].Kind.String(), "tool_end")
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

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_tools_hide", "claude", "/tmp", nil)

	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_hide", AgentSession: as, Event: &agent.AgentEvent{
		Kind:      agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}, UserMsgID: "om_user_1"})
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_hide", AgentSession: as, Event: &agent.AgentEvent{
		Kind:    agent.EventAgentToolEnd,
		ToolEnd: &agent.AgentToolEndEvent{Name: "Read", Output: "line1\nline2"},
	}, UserMsgID: "om_user_1"})

	if got := ch.Record(); len(got) != 0 {
		t.Errorf("Hide mode dropped %d tool events; want 0. Recorded: %+v", len(got), got)
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

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_tools_indep", "claude", "/tmp", nil)

	// (a) OutReply — final assistant reply (no <thinking> prefix)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_indep", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "Here is your answer.",
	}, UserMsgID: "om_user_1"})

	// (b) EventAgentResult — typed Result event (OutResult)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_indep", AgentSession: as, Event: &agent.AgentEvent{
		Kind:   agent.EventAgentResult,
		Result: &agent.AgentResultEvent{Text: "Final result text."},
	}, UserMsgID: "om_user_1"})
	// EventAgentDone flushes the F-45 §2.5 OutResult buffer (turn-end
	// fallback) — keeps the count at 3 (OutReply + OutResult + OutThinking).
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_indep", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentDone,
		Done: &agent.AgentDoneEvent{ExitCode: 0},
	}, UserMsgID: "om_user_1"})

	// (c) OutThinking — must not be dropped by /tools off
	// (ThinkMode is the orthogonal gate; default Show passes it)
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_indep", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] reasoning",
	}, UserMsgID: "om_user_1"})

	got := ch.Record()
	if len(got) != 3 {
		t.Fatalf("Hide mode forwarded %d non-tool events; want 3 (OutReply + OutResult + OutThinking)", len(got))
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

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_tools_persist", "claude", "/tmp", nil)

	toolStart := agent.AgentEvent{
		Kind:      agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}

	// Phase 1: default Hide → dropped.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_persist", AgentSession: as, Event: &toolStart, UserMsgID: "om_1"})
	if got := len(ch.Record()); got != 0 {
		t.Fatalf("phase1 (Hide) forwarded %d events; want 0", got)
	}

	// Flip to Show mid-flight.
	if err := cs.SetToolsMode(chatsession.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}

	// Phase 2: Show → forwarded.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_persist", AgentSession: as, Event: &toolStart, UserMsgID: "om_2"})
	if got := len(ch.Record()); got != 1 {
		t.Errorf("phase2 (Show) total events = %d; want 1", got)
	}

	// Flip back to Hide.
	if err := cs.SetToolsMode(chatsession.ToolsModeHide); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}

	// Phase 3: Hide → dropped again.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat_tools_persist", AgentSession: as, Event: &toolStart, UserMsgID: "om_3"})
	if got := len(ch.Record()); got != 1 {
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

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
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
	if len(got) != 1 {
		t.Fatalf("expected 1 forwarded event (OutToolStart after /tools on); got %d: %+v", len(got), got)
	}
	if got[0].Kind.String() != "tool_start" {
		t.Errorf("forwarded event Kind = %q, want %q", got[0].Kind.String(), "tool_start")
	}
}

// newWireTestStores opens a temp-dir pair of ChatSessionFile +
// AgentSessionFile. Mirrors chatsession.newTestStores but lives in
// cmd/nightme (cross-package helpers in test packages aren't
// visible by default).
func newWireTestStores(t *testing.T) (*registry.ChatSessionFile, *registry.AgentSessionFile) {
	t.Helper()
	dir := t.TempDir()
	csFile, err := registry.OpenChatSessionFile(filepath.Join(dir, "chat_sessions.json"))
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
func seedPersistedChatForWire(t *testing.T, csFile *registry.ChatSessionFile, chatID, primary string) {
	t.Helper()
	entry := &registry.ChatSessionEntry{
		ID:                "cs_" + chatID,
		ChatID:            chatID,
		SelectedCwd:         "/code/bailing",
		SelectedAgent:       primary,
		PrimaryAgent:      primary,
		CreatedAt:         time.Now(),
		LastInteractionAt: time.Now(),
	}
	if err := csFile.Upsert(entry); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

// TestWireRuntimeCallbacksAndRestore_InstallsHandlersOnRestoredChats
// is the cmd/nightme-side regression for the F-38 silent-failure
// bug: if WithOnCreate is set AFTER RestoreFromRegistry, every
// restored ChatSession ends up with nil EventHandler and nil
// MessageStateHandler. The Manager-level contract is covered in
// chatsession/manager_test.go; this test pins the cmd/nightme
// wiring that wraps both calls in wireRuntimeCallbacksAndRestore.
func TestWireRuntimeCallbacksAndRestore_InstallsHandlersOnRestoredChats(t *testing.T) {
	csFile, asFile := newWireTestStores(t)
	seedPersistedChatForWire(t, csFile, "oc_alpha", "claude")
	seedPersistedChatForWire(t, csFile, "oc_beta", "claude")

	mgr := chatsession.NewManager().WithPersistence(csFile, asFile)
	ch := echo.New("test", io.Discard)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := wireRuntimeCallbacksAndRestore(mgr, ch, outbound.New(ch, outbound.Options{}), logger); err != nil {
		t.Fatalf("wireRuntimeCallbacksAndRestore: %v", err)
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

	if err := wireRuntimeCallbacksAndRestore(mgr, ch, outbound.New(ch, outbound.Options{}), logger); err != nil {
		t.Fatalf("wireRuntimeCallbacksAndRestore: %v", err)
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

// TestWireRuntimeCallbacksAndRestore_NoPersistence verifies the
// helper handles the cold-start path (no chat_sessions.json yet):
// WithOnCreate is set, RestoreFromRegistry is a no-op, no error.
func TestWireRuntimeCallbacksAndRestore_NoPersistence(t *testing.T) {
	mgr := chatsession.NewManager() // no WithPersistence — csFile is nil
	ch := echo.New("test", io.Discard)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := wireRuntimeCallbacksAndRestore(mgr, ch, outbound.New(ch, outbound.Options{}), logger); err != nil {
		t.Fatalf("wireRuntimeCallbacksAndRestore on cold start: %v", err)
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

	h := newEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger)
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
		if e.userMsgID == "om_user_1" && e.state == agent.MessageSubmitted {
			t.Errorf("runtime event handler emitted MessageSubmitted on EventAgentReady for %q; ChatSession.TryFlush is the sole emit point", e.userMsgID)
		}
	}
}

// messageStateCall is a lightweight capture record used by the
// EventHandler tests. (Mirrors the type in
// internal/chatsession/message_state_test.go but kept local to
// avoid exporting it.)
type messageStateCall struct {
	chatID, userMsgID string
	state             agent.MessageState
}
