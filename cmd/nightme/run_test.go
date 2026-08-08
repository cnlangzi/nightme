package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/registry"
)

// TestEventHandler_ThinkGate_ShowPassesThrough verifies the
// default ThinkMode=Show does NOT drop OutThinking events — the
// F-thread-route / Feishu lark_md pipeline must still see them.
//
// The thinking prefix used by gateway.Translate is "[思考] "
// (defined in internal/gateway/translate.go as thinkingPrefix).
// EventText events whose text starts with that prefix become
// OutThinking in the Translate layer; everything else stays
// OutReply.
func TestEventHandler_ThinkGate_ShowPassesThrough(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs := mgr.GetOrCreate("oc_chat", "claude")
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	h("oc_chat", as, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "[思考] internal reasoning here",
	}, "om_user_1")

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
	cs := mgr.GetOrCreate("oc_chat", "claude")
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	h("oc_chat", as, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "[思考] internal reasoning here",
	}, "om_user_1")

	if got := ch.Record(); len(got) != 0 {
		t.Errorf("Hide mode dropped %d events; want 0. Recorded: %+v", len(got), got)
	}
}

// TestEventHandler_ThinkGate_HideDoesNotAffectOtherKinds verifies
// /think off only gates OutThinking — final assistant replies
// (EventText without the <thinking> prefix → OutReply) and other
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
	cs := mgr.GetOrCreate("oc_chat", "claude")
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	if err := cs.SetToolsMode(agent.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	// (a) OutReply — final assistant reply (no <thinking> prefix)
	h("oc_chat", as, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "Here is your answer.",
	}, "om_user_1")

	// (b) EventResult — typed Result event (OutResult)
	h("oc_chat", as, agent.AgentEvent{
		Kind: agent.EventResult,
		Result: &agent.ResultEvent{
			Text:    "Final result text.",
			IsError: false,
		},
	}, "om_user_1")
	// EventDone flushes the F-45 §2.5 OutResult buffer (turn-end
	// fallback path). EventDone also sends nothing else of its own
	// so the final count stays at 3 (OutReply + OutResult + OutToolStart).
	h("oc_chat", as, agent.AgentEvent{
		Kind: agent.EventDone,
		Done: &agent.DoneEvent{ExitCode: 0},
	}, "om_user_1")

	// (c) EventToolStart — OutToolStart (passes because /tools on)
	h("oc_chat", as, agent.AgentEvent{
		Kind:      agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}, "om_user_1")

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
	cs := mgr.GetOrCreate("oc_chat", "claude")
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	// nil logger — must not panic.
	h := newEventHandler(ch, cs, mgr, nil)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	// Run inside a recover probe to convert any panic into a
	// test failure with a useful message.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil logger caused panic: %v", r)
		}
	}()
	h("oc_chat", as, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "[思考] reasoning",
	}, "om_user_1")

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
	cs := mgr.GetOrCreate("oc_chat", "claude")
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	thinking := agent.AgentEvent{
		Kind: agent.EventText,
		Text: "[思考] reasoning",
	}

	// Phase 1: default Show → forwarded.
	h("oc_chat", as, thinking, "om_1")
	if got := len(ch.Record()); got != 1 {
		t.Fatalf("phase1 (Show) forwarded %d events; want 1", got)
	}

	// Flip to Hide mid-flight.
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	// Phase 2: Hide → dropped.
	h("oc_chat", as, thinking, "om_2")
	if got := len(ch.Record()); got != 1 {
		t.Errorf("phase2 (Hide) total events = %d; want 1 (the phase-1 event only)", got)
	}

	// Flip back to Show.
	if err := cs.SetThinkMode(chatsession.ThinkModeShow); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	// Phase 3: Show again → forwarded.
	h("oc_chat", as, thinking, "om_3")
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
// EventResult + EventUsage → single EventResult{Result, Usage}
// merge. The runtime now stamps SessionContext on the same ch.Send
// dispatch where AccumulateUsage runs — the footer sees the turn's
// own tokens on the very first send (cumulative=inTok on turn 1).
//
// Sequence:
//   1. AgentConnected → capture Model
//   2. EventResult with ResultEvent.Usage populated — single event,
//      dispatched once, footer stamped with this turn's tokens.
func TestEventHandler_OutResult_FooterFirstTurnExact(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs := mgr.GetOrCreate("oc_chat_first_turn", "claude")
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_first_turn", "claude", "/tmp", nil)

	// Step 1: AgentConnected captures Model.
	h("oc_chat_first_turn", as, agent.AgentEvent{
		Kind: agent.AgentConnected,
		Connected: &agent.AgentConnectedEvent{
			SessionID: "sess_test",
			Model:     "claude-opus-4-5",
		},
	}, "om_user_1")

	// Step 2: EventResult with co-located Usage. ONE event delivery
	// in real wire order — no EventUsage to follow, no buffer.
	const inTok, outTok, cost = 1234, 567, 0.012
	h("oc_chat_first_turn", as, agent.AgentEvent{
		Kind: agent.EventResult,
		Result: &agent.ResultEvent{
			Text:       "Final answer.",
			DurationMs: 4321,
			IsError:    false,
			Usage: &agent.UsageEvent{
				InputTokens:  inTok,
				OutputTokens: outTok,
				CostUSD:      cost,
			},
		},
	}, "om_user_1")

	got := ch.Record()
	// Find the OutResult specifically — AgentConnected may also have
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
		t.Fatal("OutResult SessionContext is nil; runtime should stamp inline after AccumulateUsage")
	}
	cum := out.SessionContext.CumulativeUsage
	if cum.InputTokens != inTok || cum.OutputTokens != outTok {
		t.Errorf("SessionContext.CumulativeUsage = %+v, want Input=%d Output=%d (first-turn tokens, NOT 0)",
			cum, inTok, outTok)
	}
	if cum.CostUSD != cost {
		t.Errorf("SessionContext.CumulativeUsage.CostUSD = %v, want %v", cum.CostUSD, cost)
	}
	if out.SessionContext.Model != "claude-opus-4-5" {
		t.Errorf("SessionContext.Model = %q, want 'claude-opus-4-5'", out.SessionContext.Model)
	}
	// Co-located Usage rides on the same OutboundMessage for any
	// channel that wants to render it directly (today's channels
	// render via SessionContext, but the field stays for symmetry
	// with the ResultEvent.Usage shape).
	if out.Usage == nil {
		t.Error("OutboundMessage.Usage is nil; gateway should populate from ResultEvent.Usage")
	} else if out.Usage.InputTokens != inTok {
		t.Errorf("OutboundMessage.Usage.InputTokens = %d, want %d", out.Usage.InputTokens, inTok)
	}
}

// TestEventHandler_OutResult_AccumulatesAcrossTurns verifies that
// successive EventResults with Usage fold into CumulativeUsage and
// the SessionContext stamp on each turn reflects the running total,
// not just the current turn.
func TestEventHandler_OutResult_AccumulatesAcrossTurns(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs := mgr.GetOrCreate("oc_chat_acc", "claude")
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_acc", "claude", "/tmp", nil)

	// Turn 1.
	h("oc_chat_acc", as, agent.AgentEvent{
		Kind: agent.EventResult,
		Result: &agent.ResultEvent{
			Text:  "first",
			Usage: &agent.UsageEvent{InputTokens: 10, OutputTokens: 5},
		},
	}, "om_user_1")
	// Turn 2.
	h("oc_chat_acc", as, agent.AgentEvent{
		Kind: agent.EventResult,
		Result: &agent.ResultEvent{
			Text:  "second",
			Usage: &agent.UsageEvent{InputTokens: 20, OutputTokens: 7},
		},
	}, "om_user_2")

	got := ch.Record()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	// First turn: cumulative = (10, 5).
	if cum := got[0].SessionContext.CumulativeUsage; cum.InputTokens != 10 || cum.OutputTokens != 5 {
		t.Errorf("turn-1 cumulative = %+v, want (10, 5)", cum)
	}
	// Second turn: cumulative = (10+20, 5+7) = (30, 12).
	if cum := got[1].SessionContext.CumulativeUsage; cum.InputTokens != 30 || cum.OutputTokens != 12 {
		t.Errorf("turn-2 cumulative = %+v, want (30, 12) — running total across turns", cum)
	}
}

// TestEventHandler_OutResult_NilUsageLeavesEmptySessionContext: a
// ResultEvent with no Usage (zero-usage turn / synthetic message)
// still ships OutResult, with SessionContext either nil or
// populated only by Model (no tokens to display). The runtime
// skips AccumulateUsage for that invocation.
func TestEventHandler_OutResult_NilUsageLeavesEmptySessionContext(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs := mgr.GetOrCreate("oc_chat_zero", "claude")
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
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

	h("oc_chat_zero", as, agent.AgentEvent{
		Kind: agent.EventResult,
		Result: &agent.ResultEvent{
			Text: "no usage reported",
			// Usage intentionally nil
		},
	}, "om_user_1")

	got := ch.Record()
	if len(got) != 1 || got[0].Kind != gateway.OutResult {
		t.Fatalf("got %v, want 1 OutResult", got)
	}
	if got[0].SessionContext != nil {
		t.Errorf("SessionContext = %+v, want nil (no Model, no usage → no footer)", got[0].SessionContext)
	}
}

// TestEventHandler_ToolsGate_ShowPassesThrough verifies the
// /tools on path: ToolsMode=Show does NOT drop OutToolStart /
// OutToolEnd events — the Feishu adapter must still see them so
// it can merge each pair into a single thread reply.
func TestEventHandler_ToolsGate_ShowPassesThrough(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs := mgr.GetOrCreate("oc_chat_tools_show", "claude")
	if err := cs.SetToolsMode(agent.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_tools_show", "claude", "/tmp", nil)

	// OutToolStart
	h("oc_chat_tools_show", as, agent.AgentEvent{
		Kind:      agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}, "om_user_1")
	// OutToolEnd
	h("oc_chat_tools_show", as, agent.AgentEvent{
		Kind:    agent.EventToolEnd,
		ToolEnd: &agent.ToolEndEvent{Name: "Read", Output: "line1\nline2"},
	}, "om_user_1")

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
	cs := mgr.GetOrCreate("oc_chat_tools_hide", "claude")
	// cs.ToolsMode is the default (ToolsModeHide) — no SetToolsMode
	// call needed, but assert it explicitly so the test reads as a
	// contract check.
	if got := cs.ToolsMode(); got != agent.ToolsModeHide {
		t.Fatalf("fresh ChatSession ToolsMode = %q, want ToolsModeHide (default)", got)
	}
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_tools_hide", "claude", "/tmp", nil)

	h("oc_chat_tools_hide", as, agent.AgentEvent{
		Kind:      agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}, "om_user_1")
	h("oc_chat_tools_hide", as, agent.AgentEvent{
		Kind:    agent.EventToolEnd,
		ToolEnd: &agent.ToolEndEvent{Name: "Read", Output: "line1\nline2"},
	}, "om_user_1")

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
	cs := mgr.GetOrCreate("oc_chat_tools_indep", "claude")
	if got := cs.ToolsMode(); got != agent.ToolsModeHide {
		t.Fatalf("fresh ChatSession ToolsMode = %q, want ToolsModeHide", got)
	}
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_tools_indep", "claude", "/tmp", nil)

	// (a) OutReply — final assistant reply (no <thinking> prefix)
	h("oc_chat_tools_indep", as, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "Here is your answer.",
	}, "om_user_1")

	// (b) EventResult — typed Result event (OutResult)
	h("oc_chat_tools_indep", as, agent.AgentEvent{
		Kind:   agent.EventResult,
		Result: &agent.ResultEvent{Text: "Final result text."},
	}, "om_user_1")
	// EventDone flushes the F-45 §2.5 OutResult buffer (turn-end
	// fallback) — keeps the count at 3 (OutReply + OutResult + OutThinking).
	h("oc_chat_tools_indep", as, agent.AgentEvent{
		Kind: agent.EventDone,
		Done: &agent.DoneEvent{ExitCode: 0},
	}, "om_user_1")

	// (c) OutThinking — must not be dropped by /tools off
	// (ThinkMode is the orthogonal gate; default Show passes it)
	h("oc_chat_tools_indep", as, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "[思考] reasoning",
	}, "om_user_1")

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
	cs := mgr.GetOrCreate("oc_chat_tools_persist", "claude")
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_tools_persist", "claude", "/tmp", nil)

	toolStart := agent.AgentEvent{
		Kind:      agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}

	// Phase 1: default Hide → dropped.
	h("oc_chat_tools_persist", as, toolStart, "om_1")
	if got := len(ch.Record()); got != 0 {
		t.Fatalf("phase1 (Hide) forwarded %d events; want 0", got)
	}

	// Flip to Show mid-flight.
	if err := cs.SetToolsMode(agent.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}

	// Phase 2: Show → forwarded.
	h("oc_chat_tools_persist", as, toolStart, "om_2")
	if got := len(ch.Record()); got != 1 {
		t.Errorf("phase2 (Show) total events = %d; want 1", got)
	}

	// Flip back to Hide.
	if err := cs.SetToolsMode(agent.ToolsModeHide); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}

	// Phase 3: Hide → dropped again.
	h("oc_chat_tools_persist", as, toolStart, "om_3")
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
	cs := mgr.GetOrCreate("oc_chat_both_gates", "claude")
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat_both_gates", "claude", "/tmp", nil)

	// Flip both off.
	if err := cs.SetToolsMode(agent.ToolsModeHide); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	// OutThinking → dropped (ThinkMode gate)
	h("oc_chat_both_gates", as, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "[思考] reasoning",
	}, "om_user_1")

	// OutToolStart → dropped (ToolsMode gate)
	h("oc_chat_both_gates", as, agent.AgentEvent{
		Kind:      agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}, "om_user_1")

	// Flip only /tools on.
	if err := cs.SetToolsMode(agent.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}

	// OutThinking → still dropped (ThinkMode gate unchanged)
	h("oc_chat_both_gates", as, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "[思考] more reasoning",
	}, "om_user_2")

	// OutToolStart → now forwarded (ToolsMode flipped to Show)
	h("oc_chat_both_gates", as, agent.AgentEvent{
		Kind:      agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{Name: "Bash", Args: "ls"},
	}, "om_user_2")

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
		ActiveCwd:         "/code/bailing",
		ActiveAgent:       primary,
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
	gwImpl := gateway.New(func(_ context.Context, _ *gateway.InboundMessage) error { return nil }).(*gateway.Router)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := wireRuntimeCallbacksAndRestore(mgr, ch, gwImpl, logger); err != nil {
		t.Fatalf("wireRuntimeCallbacksAndRestore: %v", err)
	}

	for _, cs := range mgr.List() {
		if cs.EventHandler() == nil {
			t.Errorf("%s: EventHandler is nil — wiring regression", cs.ChatID)
		}
		if cs.MessageStateHandler() == nil {
			t.Errorf("%s: MessageStateHandler is nil — wiring regression", cs.ChatID)
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
	gwImpl := gateway.New(func(_ context.Context, _ *gateway.InboundMessage) error { return nil }).(*gateway.Router)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := wireRuntimeCallbacksAndRestore(mgr, ch, gwImpl, logger); err != nil {
		t.Fatalf("wireRuntimeCallbacksAndRestore: %v", err)
	}

	csList := mgr.List()
	if len(csList) != 1 {
		t.Fatalf("mgr.List len = %d, want 1", len(csList))
	}
	cs := csList[0]
	handler := cs.MessageStateHandler()
	if handler == nil {
		t.Fatal("MessageStateHandler is nil")
	}

	// Empty chatID: handler must return silently without sending.
	handler("", "om_user", agent.MessageSubmitted)
	if got := ch.Record(); len(got) != 0 {
		t.Errorf("empty chatID should drop silently; got %d events", len(got))
	}

	// Empty userMsgID: same.
	handler(cs.ChatID, "", agent.MessageSubmitted)
	if got := ch.Record(); len(got) != 0 {
		t.Errorf("empty userMsgID should drop silently; got %d events", len(got))
	}

	// Both empty: same.
	handler("", "", agent.MessageSubmitted)
	if got := ch.Record(); len(got) != 0 {
		t.Errorf("both empty should drop silently; got %d events", len(got))
	}

	// Sanity: a valid call DOES produce an OutboundMessage (so
	// the silent drop is targeted, not "the handler never fires").
	// F-53: agent.MessageDone no longer exists; use the closest
	// live state (MessageSubmitted) for the sanity probe.
	handler(cs.ChatID, "om_user", agent.MessageSubmitted)
	if got := ch.Record(); len(got) != 1 {
		t.Errorf("valid call should fire; got %d events", len(got))
	}
}

// TestWireRuntimeCallbacksAndRestore_NoPersistence verifies the
// helper handles the cold-start path (no chat_sessions.json yet):
// WithOnCreate is set, RestoreFromRegistry is a no-op, no error.
func TestWireRuntimeCallbacksAndRestore_NoPersistence(t *testing.T) {
	mgr := chatsession.NewManager() // no WithPersistence — csFile is nil
	ch := echo.New("test", io.Discard)
	gwImpl := gateway.New(func(_ context.Context, _ *gateway.InboundMessage) error { return nil }).(*gateway.Router)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := wireRuntimeCallbacksAndRestore(mgr, ch, gwImpl, logger); err != nil {
		t.Fatalf("wireRuntimeCallbacksAndRestore on cold start: %v", err)
	}
	if len(mgr.List()) != 0 {
		t.Errorf("expected no restored chats; got %d", len(mgr.List()))
	}
}
