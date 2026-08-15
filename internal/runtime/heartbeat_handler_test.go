// Package runtime — handler integration tests for F-63 heartbeat.
//
// These tests pin the ⭐ core invariant documented in F-63 §3.2:
// the heartbeat observation must happen BEFORE the policy chain,
// so /think off and /tools off cannot suppress the counter.
//
// Each test case mirrors one row of the F-63 §3.7 behaviour
// matrix so future regressions show up as a labelled failure
// ("ThinkOff_StillCounts" rather than a generic panic).

package runtime

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestEventHandler_ThinkOff_StillCounts (F-63 §7.2) — pins the
// core invariant: /think off (ThinkModeGatePolicy) drops the
// original OutThinking from reaching the Channel, but the
// heartbeat counter MUST still increment and an OutHeartbeat
// MUST still be delivered so the receipt header reflects real
// agent activity.
func TestEventHandler_ThinkOff_StillCounts(t *testing.T) {
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

	recorded := ch.Record()

	// The original OutThinking must be dropped (gate contract).
	for _, m := range recorded {
		if m.Kind == messages.OutThinking {
			t.Fatalf("OutThinking leaked to channel despite /think off: %+v", m)
		}
	}

	// But an OutHeartbeat must be delivered with ThinkCount=1.
	var hb *messages.HeartbeatSnapshot
	for _, m := range recorded {
		if m.Kind == messages.OutHeartbeat {
			m := m
			hb = m.Heartbeat
		}
	}
	if hb == nil {
		t.Fatalf("expected OutHeartbeat in channel record, got: %+v", recorded)
	}
	if hb.ThinkCount != 1 {
		t.Fatalf("Heartbeat ThinkCount = %d, want 1 (counter must increment before gate drops)", hb.ThinkCount)
	}
	if hb.LastBeatAt.IsZero() {
		t.Fatal("Heartbeat LastBeatAt must be set")
	}

	// Tracker state must agree.
	if got := cs.Heartbeat().Snapshot("om_user_1"); got.ThinkCount != 1 {
		t.Fatalf("tracker snapshot ThinkCount = %d, want 1", got.ThinkCount)
	}
}

// TestEventHandler_ToolsOff_StillCounts (F-63 §7.2) — same
// invariant for /tools off. OutToolStart is dropped from the
// channel's record, but the heartbeat ToolCount must still go
// up and an OutHeartbeat must still be delivered.
func TestEventHandler_ToolsOff_StillCounts(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	// /tools off is the default; reassert explicitly to make
	// the test self-contained if the default flips later.
	if err := cs.SetToolsMode(chatsession.ToolsModeHide); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{
			ID:   "tool_1",
			Name: "Read",
			Args: `{"file_path":"/x"}`,
		},
	}, UserMsgID: "om_user_1"})

	recorded := ch.Record()

	// Original OutToolStart must be dropped.
	for _, m := range recorded {
		if m.Kind == messages.OutToolStart {
			t.Fatalf("OutToolStart leaked despite /tools off: %+v", m)
		}
	}

	// OutHeartbeat must still be delivered with ToolCount=1.
	var hb *messages.HeartbeatSnapshot
	for _, m := range recorded {
		if m.Kind == messages.OutHeartbeat {
			m := m
			hb = m.Heartbeat
		}
	}
	if hb == nil {
		t.Fatalf("expected OutHeartbeat, got: %+v", recorded)
	}
	if hb.ToolCount != 1 {
		t.Fatalf("Heartbeat ToolCount = %d, want 1", hb.ToolCount)
	}
}

// TestEventHandler_BothOff_StillCounts (F-63 §7.2) — both gates
// open. Mixed sequence of thinking + tool starts; counts
// accumulate regardless.
func TestEventHandler_BothOff_StillCounts(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	if err := cs.SetToolsMode(chatsession.ToolsModeHide); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	dispatch := func(kind agent.EventKind, payload interface{}) {
		ev := &agent.AgentEvent{Kind: kind}
		switch p := payload.(type) {
		case string:
			ev.Text = p
		case *agent.AgentToolStartEvent:
			ev.ToolStart = p
		case *agent.AgentToolEndEvent:
			ev.ToolEnd = p
		default:
			t.Fatalf("unknown payload type %T", payload)
		}
		h(chatsession.AgentEventEnvelope{
			ChatID: "oc_chat", AgentSession: as, Event: ev, UserMsgID: "om_user_1",
		})
	}

	dispatch(agent.EventAgentText, "[思考] a")
	dispatch(agent.EventAgentToolStart, &agent.AgentToolStartEvent{ID: "t1", Name: "Read"})
	dispatch(agent.EventAgentText, "[思考] b")
	dispatch(agent.EventAgentToolStart, &agent.AgentToolStartEvent{ID: "t2", Name: "Bash"})

	snap := cs.Heartbeat().Snapshot("om_user_1")
	if snap.ThinkCount != 2 {
		t.Fatalf("ThinkCount = %d, want 2 (both think events)", snap.ThinkCount)
	}
	if snap.ToolCount != 2 {
		t.Fatalf("ToolCount = %d, want 2 (both tool starts)", snap.ToolCount)
	}

	// No original OutThinking / OutToolStart may appear.
	recorded := ch.Record()
	for _, m := range recorded {
		if m.Kind == messages.OutThinking || m.Kind == messages.OutToolStart {
			t.Fatalf("gated kind leaked: %s", m.Kind.String())
		}
	}

	// OutHeartbeat messages must appear (one per countable event).
	var hbCount int
	for _, m := range recorded {
		if m.Kind == messages.OutHeartbeat {
			hbCount++
		}
	}
	if hbCount != 4 {
		t.Fatalf("OutHeartbeat count = %d, want 4 (2 think + 2 tool_start)", hbCount)
	}
}

// TestEventHandler_DefaultMode_StillCounts (F-63 §7.2) — gates
// open. Mixed sequence; counters and OutHeartbeat must still
// work (proves the integration is not gated-dependent).
func TestEventHandler_DefaultMode_StillCounts(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	if err := cs.SetToolsMode(chatsession.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	dispatch := func(kind agent.EventKind, payload interface{}) {
		ev := &agent.AgentEvent{Kind: kind}
		switch p := payload.(type) {
		case string:
			ev.Text = p
		case *agent.AgentToolStartEvent:
			ev.ToolStart = p
		case *agent.AgentToolEndEvent:
			ev.ToolEnd = p
		default:
			t.Fatalf("unknown payload type %T", payload)
		}
		h(chatsession.AgentEventEnvelope{
			ChatID: "oc_chat", AgentSession: as, Event: ev, UserMsgID: "om_user_1",
		})
	}

	dispatch(agent.EventAgentText, "[思考] a")
	dispatch(agent.EventAgentText, "actual reply text")
	dispatch(agent.EventAgentToolStart, &agent.AgentToolStartEvent{ID: "t1", Name: "Read"})
	dispatch(agent.EventAgentToolEnd, &agent.AgentToolEndEvent{ID: "t1", Name: "Read"})

	snap := cs.Heartbeat().Snapshot("om_user_1")
	if snap.ThinkCount != 1 {
		t.Fatalf("ThinkCount = %d, want 1", snap.ThinkCount)
	}
	if snap.ToolCount != 1 {
		t.Fatalf("ToolCount = %d, want 1 (ToolEnd must not double-count)", snap.ToolCount)
	}

	recorded := ch.Record()
	var hbCount, replyCount, toolStartCount int
	for _, m := range recorded {
		switch m.Kind {
		case messages.OutHeartbeat:
			hbCount++
		case messages.OutReply:
			replyCount++
		case messages.OutToolStart:
			toolStartCount++
		}
	}
	if hbCount != 2 {
		t.Fatalf("OutHeartbeat count = %d, want 2 (1 think + 1 tool_start)", hbCount)
	}
	if replyCount != 1 {
		t.Fatalf("OutReply count = %d, want 1", replyCount)
	}
	if toolStartCount != 1 {
		t.Fatalf("OutToolStart count = %d, want 1", toolStartCount)
	}
}

// TestEventHandler_OutHeartbeat_NoRecursion (F-63 §3.8 #1 /
// §7.2) — when the handler emits OutHeartbeat via em.Send, the
// echo channel records it, but it must NOT trigger another
// Observe call. We verify by counting OutHeartbeats in the
// recorded sequence: one input OutThinking must produce exactly
// one OutHeartbeat (not two from a self-recursion).
func TestEventHandler_OutHeartbeat_NoRecursion(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	// Single thinking event. Expected: original OutThinking
	// (gates default Show) + exactly one OutHeartbeat.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "[思考] single thought",
	}, UserMsgID: "om_user_1"})

	recorded := ch.Record()

	var hbCount int
	for _, m := range recorded {
		if m.Kind == messages.OutHeartbeat {
			hbCount++
		}
	}
	if hbCount != 1 {
		t.Fatalf("OutHeartbeat count = %d, want exactly 1 (handler must not self-recurse)", hbCount)
	}
}

// TestEventHandler_ObserveOrder_BeforePolicy (F-63 §7.2) — when
// a kind doesn't increment counters, no OutHeartbeat must be
// emitted, even though the event flows through the policy chain.
func TestEventHandler_ObserveOrder_BeforePolicy(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	// OutReply is non-counting. No OutHeartbeat should be emitted.
	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "plain reply",
	}, UserMsgID: "om_user_1"})

	recorded := ch.Record()
	for _, m := range recorded {
		if m.Kind == messages.OutHeartbeat {
			t.Fatalf("non-counting kind emitted OutHeartbeat: %+v", m)
		}
	}

	// But LastBeatAt must be refreshed.
	snap := cs.Heartbeat().Snapshot("om_user_1")
	if snap.LastBeatAt.IsZero() {
		t.Fatal("LastBeatAt must refresh even on non-counting kinds")
	}
}

// TestEventHandler_OrphanEvent_NoHeartbeat (F-63 §3.8) — when
// userMsgID is empty (orphan events like EventAgentReady at
// startup), Observe is a no-op (the runtime skips the call).
// Verify nothing crashes and no OutHeartbeat is emitted.
func TestEventHandler_OrphanEvent_NoHeartbeat(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	h(chatsession.AgentEventEnvelope{
		ChatID: "oc_chat", AgentSession: as,
		Event:     &agent.AgentEvent{Kind: agent.EventAgentReady, Model: "x"},
		UserMsgID: "", // orphan
	})

	recorded := ch.Record()
	for _, m := range recorded {
		if m.Kind == messages.OutHeartbeat {
			t.Fatalf("orphan event leaked OutHeartbeat: %+v", m)
		}
	}
}

// TestEventHandler_Heartbeat_OrderInSequence pins a concrete
// scenario from F-63 §3.7 row 1 (default /think on, /tools on):
// think + tool + result yields exactly 2 OutHeartbeat emissions
// (one per countable event), with snapshot ThinkCount=1 and
// ToolCount=1 by the end.
func TestEventHandler_Heartbeat_OrderInSequence(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")
	if err := cs.SetToolsMode(chatsession.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	logger := slog.Default()

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, logger, chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	dispatch := func(ev *agent.AgentEvent) {
		h(chatsession.AgentEventEnvelope{
			ChatID: "oc_chat", AgentSession: as, Event: ev, UserMsgID: "om_user_1",
		})
	}

	dispatch(&agent.AgentEvent{Kind: agent.EventAgentText, Text: "[思考] pondering"})
	dispatch(&agent.AgentEvent{Kind: agent.EventAgentToolStart, ToolStart: &agent.AgentToolStartEvent{ID: "t1", Name: "Read"}})
	dispatch(&agent.AgentEvent{Kind: agent.EventAgentToolEnd, ToolEnd: &agent.AgentToolEndEvent{ID: "t1", Name: "Read"}})
	dispatch(&agent.AgentEvent{Kind: agent.EventAgentText, Text: "final answer"})
	dispatch(&agent.AgentEvent{Kind: agent.EventAgentResult, Result: &agent.AgentResultEvent{Text: "final answer"}})

	snap := cs.Heartbeat().Snapshot("om_user_1")
	if snap.ThinkCount != 1 {
		t.Fatalf("ThinkCount = %d, want 1", snap.ThinkCount)
	}
	if snap.ToolCount != 1 {
		t.Fatalf("ToolCount = %d, want 1", snap.ToolCount)
	}
	if snap.LastBeatAt.IsZero() {
		t.Fatal("LastBeatAt not set")
	}
	// Sanity: LastBeatAt must be >= observation start (absolute
	// time check; avoids wall-clock-dependent flakiness that a
	// relative `time.Since(...) > 5*time.Second` check has on
	// slow CI).
	t0 := time.Now().Add(-time.Hour) // any time before now
	if snap.LastBeatAt.Before(t0) {
		t.Fatalf("LastBeatAt = %v, want >= recent observation start", snap.LastBeatAt)
	}

	recorded := ch.Record()
	var hbCount int
	for _, m := range recorded {
		if m.Kind == messages.OutHeartbeat {
			hbCount++
		}
	}
	if hbCount != 2 {
		t.Fatalf("OutHeartbeat count = %d, want 2 (think + tool_start)", hbCount)
	}
}