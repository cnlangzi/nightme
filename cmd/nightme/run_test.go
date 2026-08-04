package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestEventHandler_ThinkGate_ShowPassesThrough verifies the
// default ThinkMode=Show does NOT drop OutThinking events — the
// F-thread-route / Feishu lark_md pipeline must still see them.
//
// The thinking prefix used by gateway.Translate is "[思考] "
// (defined in internal/gateway/translate.go as thinkingPrefix).
// EventText events whose text starts with that prefix become
// OutThinking in the Translate layer; everything else stays
// OutText.
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
// (EventText without the <thinking> prefix → OutText) and other
// kinds must still flow to the Channel.
func TestEventHandler_ThinkGate_HideDoesNotAffectOtherKinds(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs := mgr.GetOrCreate("oc_chat", "claude")
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	logger := slog.Default()

	h := newEventHandler(ch, cs, mgr, logger)
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	// (a) OutText — final assistant reply (no <thinking> prefix)
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

	// (c) EventToolStart — OutToolStart
	h("oc_chat", as, agent.AgentEvent{
		Kind:      agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{Name: "Read", Args: "/tmp/foo"},
	}, "om_user_1")

	got := ch.Record()
	if len(got) != 3 {
		t.Fatalf("Hide mode forwarded %d non-thinking events; want 3 (OutText + OutResult + OutToolStart)", len(got))
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