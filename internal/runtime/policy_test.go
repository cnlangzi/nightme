// policy_test.go — direct tests for each OutboundPolicy + the
// default chain ordering.
//
// Pre-Phase-2.4 these decisions were inlined in
// NewEventHandler and only exercised via the end-to-end event
// handler tests. The extracted policies get per-policy
// coverage so a future regression in any single decision
// doesn't require spinning up a full ChatSession + AgentEvent
// to detect.

package runtime

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// makeAS is a tiny helper that builds an AgentSession with
// the bare-minimum fields the policies inspect. The session
// is never persisted in these tests — we only read
// .ThinkMode() / .ToolsMode() from the ChatSession wrapper.
func makeAS(t *testing.T) *chatsession.ChatSession {
	t.Helper()
	cs, _ := chatsession.NewManager().WithPrimaryAgent("claude").GetOrCreate("oc_policy_test", "claude")
	return cs
}

// makeEnvelope builds the minimal envelope a policy sees.
// Tests only read AgentSession + Kind from it.
func makeEnvelope(cs *chatsession.ChatSession) chatsession.AgentEventEnvelope {
	return chatsession.AgentEventEnvelope{
		ChatID:       cs.ChatID,
		AgentSession: chatsession.NewAgentSession("as_policy_test", cs.ID, "claude", "/tmp", nil),
		Event:        &agent.AgentEvent{Kind: agent.EventAgentText},
		UserMsgID:    "om_policy_test",
	}
}

// TestThinkModeGatePolicy_ShowPassesThrough is the positive
// half: default ThinkMode=Show must NOT drop OutThinking.
// Mirrors the original TestEventHandler_ThinkGate_ShowPassesThrough
// but at the policy level.
func TestThinkModeGatePolicy_ShowPassesThrough(t *testing.T) {
	cs := makeAS(t)
	if got := cs.ThinkMode(); got != chatsession.ThinkModeShow {
		t.Fatalf("fresh ChatSession ThinkMode = %q, want ThinkModeShow (default)", got)
	}
	p := ThinkModeGatePolicy(cs, slog.New(slog.NewTextHandler(testDevNull{t}, nil)))
	out := &messages.OutboundMessage{Kind: messages.OutThinking, ChatID: cs.ChatID}
	if drop := p.Apply(out, makeEnvelope(cs)); drop {
		t.Errorf("Show mode: drop=true (want false)")
	}
}

// TestThinkModeGatePolicy_HideDropsOutThinking is the core
// F-think §3.1.2 contract: /think off → OutThinking is
// dropped before reaching em.Send.
func TestThinkModeGatePolicy_HideDropsOutThinking(t *testing.T) {
	cs := makeAS(t)
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	p := ThinkModeGatePolicy(cs, slog.New(slog.NewTextHandler(testDevNull{t}, nil)))
	out := &messages.OutboundMessage{Kind: messages.OutThinking, ChatID: cs.ChatID}
	if drop := p.Apply(out, makeEnvelope(cs)); !drop {
		t.Errorf("Hide mode: drop=false (want true)")
	}
}

// TestThinkModeGatePolicy_HideDoesNotAffectOtherKinds pins
// the F-think contract that the gate only affects
// OutThinking — OutReply / OutResult / OutToolStart all pass
// through regardless of ThinkMode. A future widening of the
// gate would silently silence these Kinds; this test catches
// the regression.
func TestThinkModeGatePolicy_HideDoesNotAffectOtherKinds(t *testing.T) {
	cs := makeAS(t)
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	p := ThinkModeGatePolicy(cs, slog.New(slog.NewTextHandler(testDevNull{t}, nil)))

	kinds := []messages.OutboundKind{
		messages.OutReply, messages.OutResult,
		messages.OutToolStart, messages.OutToolEnd,
		messages.OutInit, messages.OutMessageState,
	}
	for _, kind := range kinds {
		out := &messages.OutboundMessage{Kind: kind, ChatID: cs.ChatID}
		if drop := p.Apply(out, makeEnvelope(cs)); drop {
			t.Errorf("Kind %s: dropped (want pass-through)", kind)
		}
	}
}

// TestToolsModeGatePolicy_HideDropsBothToolKinds pins the
// F-38 §3.1.3 contract: /tools off (the default) drops
// BOTH OutToolStart and OutToolEnd. A future gate that only
// catches one would let the other leak to the channel and
// waste a thread reply slot.
func TestToolsModeGatePolicy_HideDropsBothToolKinds(t *testing.T) {
	cs := makeAS(t)
	if got := cs.ToolsMode(); got != chatsession.ToolsModeHide {
		t.Fatalf("fresh ChatSession ToolsMode = %q, want ToolsModeHide (default)", got)
	}
	p := ToolsModeGatePolicy(cs, slog.New(slog.NewTextHandler(testDevNull{t}, nil)))

	for _, kind := range []messages.OutboundKind{messages.OutToolStart, messages.OutToolEnd} {
		out := &messages.OutboundMessage{Kind: kind, ChatID: cs.ChatID}
		if drop := p.Apply(out, makeEnvelope(cs)); !drop {
			t.Errorf("Kind %s: drop=false (want true)", kind)
		}
	}
}

// TestToolsModeGatePolicy_ShowPassesThrough: ToolsMode=Show
// must pass OutToolStart/End through to the channel.
func TestToolsModeGatePolicy_ShowPassesThrough(t *testing.T) {
	cs := makeAS(t)
	if err := cs.SetToolsMode(chatsession.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	p := ToolsModeGatePolicy(cs, slog.New(slog.NewTextHandler(testDevNull{t}, nil)))

	for _, kind := range []messages.OutboundKind{messages.OutToolStart, messages.OutToolEnd} {
		out := &messages.OutboundMessage{Kind: kind, ChatID: cs.ChatID}
		if drop := p.Apply(out, makeEnvelope(cs)); drop {
			t.Errorf("Show mode: drop=true (want false)")
		}
	}
}

// TestToolsModeGatePolicy_HideDoesNotAffectOtherKinds pins
// the F-38 contract that the gate only affects the two tool
// Kinds — OutReply / OutResult / OutThinking / OutInit /
// OutUsage all pass through regardless of ToolsMode.
func TestToolsModeGatePolicy_HideDoesNotAffectOtherKinds(t *testing.T) {
	cs := makeAS(t)
	p := ToolsModeGatePolicy(cs, slog.New(slog.NewTextHandler(testDevNull{t}, nil)))

	kinds := []messages.OutboundKind{
		messages.OutReply, messages.OutResult,
		messages.OutThinking, messages.OutInit, messages.OutMessageState,
	}
	for _, kind := range kinds {
		out := &messages.OutboundMessage{Kind: kind, ChatID: cs.ChatID}
		if drop := p.Apply(out, makeEnvelope(cs)); drop {
			t.Errorf("Kind %s: dropped (want pass-through)", kind)
		}
	}
}

// TestDefaultPolicies_OrderMatters pins the production
// policy ordering: post-fix-status-bar-git the chain is just
// the think gate + tools gate (no StatusBar policy — that
// moved to the outbound Emitter). The think gate runs BEFORE
// the tools gate
// so a chat with both modes hidden still produces a single
// coherent "X dropped" log entry instead of two stacked
// lines per OutThinking event.
//
// To test: construct an envelope where BOTH gates would
// drop (OutThinking + ThinkMode=Hide), apply the chain,
// and assert that policy[0] stamps, policy[1] (think gate)
// drops, and policy[2] (tools gate) is never reached (it
// doesn't drop OutThinking anyway).
func TestDefaultPolicies_OrderMatters(t *testing.T) {
	cs := makeAS(t)
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(testDevNull{t}, nil))
	policies := DefaultPolicies(chatsession.GitStatusDeps{}, cs, logger)
	if len(policies) != 2 {
		t.Fatalf("DefaultPolicies returned %d policies, want 2 (F-CLAUDE-PRINT-002 dropped StatusBarStampPolicy)", len(policies))
	}

	// F-CLAUDE-PRINT-002: StatusBarStampPolicy is gone. The
	// runtime event hook (handler.go) stamps chatsession.GitStatus
	// onto out.GitStatus directly. The remaining two policies
	// (think gate, tools gate) are unchanged.

	// policy[0] (think gate) drops OutThinking when Hide.
	out := &messages.OutboundMessage{Kind: messages.OutThinking, ChatID: cs.ChatID}
	if drop := policies[0].Apply(out, makeEnvelope(cs)); !drop {
		t.Errorf("policy[0] did not drop OutThinking under Hide mode")
	}

	// policy[1] (tools gate) drops OutToolStart when Hide.
	out2 := &messages.OutboundMessage{Kind: messages.OutToolStart, ChatID: cs.ChatID}
	if drop := policies[1].Apply(out2, makeEnvelope(cs)); !drop {
		t.Errorf("policy[1] did not drop OutToolStart under Hide mode")
	}
}

// TestPolicyFunc_NilSafe verifies the PolicyFunc adapter
// handles nil receivers — a future caller that registers a
// nil PolicyFunc via DefaultPolicies (or a typo in a custom
// policy) must not panic at handler install time. The drop
// returns false (no-op), and the handler skips the policy
// silently.
func TestPolicyFunc_NilSafe(t *testing.T) {
	var f PolicyFunc
	out := &messages.OutboundMessage{Kind: messages.OutReply}
	drop := f.Apply(out, makeEnvelope(makeAS(t)))
	if drop {
		t.Errorf("nil PolicyFunc returned drop=true")
	}
}

// TestThinkModeGatePolicy_NilAgentSession_DoesNotPanic pins
// the runtime-handler contract documented in
// agentsession/event_types.go: "AgentSession is ALWAYS non-nil
// in production. … If you add a new publish site that omits
// this guard, the runtime handler will panic — fix the
// publisher, not the subscriber."
//
// Despite that contract, the gate itself is built to defend
// against a missed guard: the "think dropped" log line uses
// env.AgentSession.ID, and a missed publisher guard would
// panic here. This test sends env.AgentSession=nil and asserts
// the gate still returns drop=true (it doesn't panic, and the
// drop decision is unaffected). The asID log field just
// serializes as "".
func TestThinkModeGatePolicy_NilAgentSession_DoesNotPanic(t *testing.T) {
	cs := makeAS(t)
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	// Capture log output so we can assert the gate ran the
	// "think dropped" log line without panicking on nil AS.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	p := ThinkModeGatePolicy(cs, logger)

	// envelope with AgentSession deliberately nil — simulates
	// a future publisher that misses the as==nil guard.
	env := chatsession.AgentEventEnvelope{
		ChatID:       cs.ChatID,
		AgentSession: nil,
		Event:        &agent.AgentEvent{Kind: agent.EventAgentText},
		UserMsgID:    "om_nil_as",
	}
	out := &messages.OutboundMessage{Kind: messages.OutThinking, ChatID: cs.ChatID}

	// Must not panic. If the nil-guard regressed, this call
	// would panic on env.AgentSession.ID.
	drop := p.Apply(out, env)
	if !drop {
		t.Errorf("Hide mode: drop=false (want true) — gate silently passed OutThinking")
	}

	// And the log line was emitted (proves the guard is the
	// nil-safe path, not a panic-recovered path).
	logs := logBuf.String()
	if !strings.Contains(logs, "think dropped") {
		t.Errorf("expected 'think dropped' log line; got %q", logs)
	}
	// asID is empty because AgentSession was nil — the
	// operator can grep the empty string in production logs
	// to spot missed guards.
	if !strings.Contains(logs, "agent_session_id=") {
		t.Errorf("expected agent_session_id field in log; got %q", logs)
	}
}

// TestToolsModeGatePolicy_NilAgentSession_DoesNotPanic is the
// symmetric regression for ToolsModeGatePolicy. Same rationale
// as TestThinkModeGatePolicy_NilAgentSession_DoesNotPanic.
func TestToolsModeGatePolicy_NilAgentSession_DoesNotPanic(t *testing.T) {
	cs := makeAS(t)
	if got := cs.ToolsMode(); got != chatsession.ToolsModeHide {
		t.Fatalf("fresh ChatSession ToolsMode = %q, want ToolsModeHide", got)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	p := ToolsModeGatePolicy(cs, logger)

	env := chatsession.AgentEventEnvelope{
		ChatID:       cs.ChatID,
		AgentSession: nil,
		Event:        &agent.AgentEvent{Kind: agent.EventAgentText},
		UserMsgID:    "om_nil_as_tools",
	}
	for _, kind := range []messages.OutboundKind{messages.OutToolStart, messages.OutToolEnd} {
		out := &messages.OutboundMessage{Kind: kind, ChatID: cs.ChatID}
		drop := p.Apply(out, env)
		if !drop {
			t.Errorf("Kind %s: drop=false (want true) — gate silently passed tool event", kind)
		}
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "tools dropped") {
		t.Errorf("expected 'tools dropped' log line; got %q", logs)
	}
}

// TestDefaultPolicies_NilAgentSession_ChainSurvives verifies
// that the full DefaultPolicies chain (think gate + tools gate)
// handles a nil-AgentSession envelope without panicking. This
// is the end-to-end form of the per-policy nil-safety tests —
// a regression in either gate would surface here as a panic
// during the DefaultPolicies call.
func TestDefaultPolicies_NilAgentSession_ChainSurvives(t *testing.T) {
	cs := makeAS(t)
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(&testDevNull{t}, nil))
	policies := DefaultPolicies(chatsession.GitStatusDeps{}, cs, logger)

	env := chatsession.AgentEventEnvelope{
		ChatID:       cs.ChatID,
		AgentSession: nil,
		Event:        &agent.AgentEvent{Kind: agent.EventAgentText},
		UserMsgID:    "om_chain_nil",
	}
	cases := []struct {
		kind     messages.OutboundKind
		wantDrop bool
	}{
		// think gate fires (Hide + OutThinking → drop)
		{messages.OutThinking, true},
		// tools gate fires (Hide + OutToolStart → drop)
		{messages.OutToolStart, true},
		// neither gate fires — pass-through
		{messages.OutReply, false},
		{messages.OutResult, false},
	}
	for _, tc := range cases {
		out := &messages.OutboundMessage{Kind: tc.kind, ChatID: cs.ChatID}
		drop := false
		for _, pol := range policies {
			if drop = pol.Apply(out, env); drop {
				break
			}
		}
		if drop != tc.wantDrop {
			t.Errorf("kind=%s nil-AS: drop=%v, want %v", tc.kind, drop, tc.wantDrop)
		}
	}
}

// testDevNull is an io.Writer that discards. Used so the
// policy tests can construct a *slog.Logger without polluting
// test output (each Info-level "X dropped" log line would
// otherwise show up).
type testDevNull struct{ t *testing.T }

func (d testDevNull) Write(p []byte) (int, error) { return len(p), nil }