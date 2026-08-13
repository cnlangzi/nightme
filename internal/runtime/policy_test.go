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
	"log/slog"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/statusbar"
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

// TestStatusBarStampPolicy_StampsFourKinds verifies the F-45
// §2.5 改动 C contract: only the four main-chat Kinds
// (OutReply / OutResult / OutTaskCreate / OutTaskUpdate)
// receive a StatusBar stamp. Other Kinds must NOT —
// thread-only / lifecycle / init payloads would only inflate
// wire size.
func TestStatusBarStampPolicy_StampsFourKinds(t *testing.T) {
	cs := makeAS(t)
	p := StatusBarStampPolicy(statusbar.Deps{})

	stamped := []messages.OutboundKind{
		messages.OutReply, messages.OutResult,
		messages.OutTaskCreate, messages.OutTaskUpdate,
	}
	for _, kind := range stamped {
		out := &messages.OutboundMessage{Kind: kind, ChatID: cs.ChatID}
		if drop := p.Apply(out, makeEnvelope(cs)); drop {
			t.Errorf("Kind %s: policy returned drop=true (want false)", kind)
		}
		if out.StatusBar == nil {
			t.Errorf("Kind %s: StatusBar was not stamped", kind)
		}
	}
}

// TestStatusBarStampPolicy_SkipsOtherKinds is the negative
// half: thread-only / lifecycle / init / usage / result Kinds
// must NOT incur a stamp (would inflate payload size with no
// user-visible benefit). The four Kinds above are covered
// separately.
func TestStatusBarStampPolicy_SkipsOtherKinds(t *testing.T) {
	cs := makeAS(t)
	p := StatusBarStampPolicy(statusbar.Deps{})

	skipped := []messages.OutboundKind{
		messages.OutCard, messages.OutInit,
		messages.OutMessageState, messages.OutThinking,
		messages.OutToolStart, messages.OutToolEnd,
	}
	for _, kind := range skipped {
		out := &messages.OutboundMessage{Kind: kind, ChatID: cs.ChatID}
		if drop := p.Apply(out, makeEnvelope(cs)); drop {
			t.Errorf("Kind %s: policy returned drop=true (want false)", kind)
		}
		if out.StatusBar != nil {
			t.Errorf("Kind %s: StatusBar stamped (want nil — not in the 4 main-chat Kinds)", kind)
		}
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
// policy ordering: statusbar runs FIRST so a stamp never
// happens after a drop would have made it moot (small wire-
// size win), and the think gate runs BEFORE the tools gate
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
	policies := DefaultPolicies(statusbar.Deps{}, cs, logger)
	if len(policies) != 3 {
		t.Fatalf("DefaultPolicies returned %d policies, want 3", len(policies))
	}

	// policy[0] (statusbar): stamps OutReply, doesn't drop.
	out := &messages.OutboundMessage{Kind: messages.OutReply, ChatID: cs.ChatID}
	if drop := policies[0].Apply(out, makeEnvelope(cs)); drop {
		t.Errorf("policy[0] dropped OutReply (want stamp, not drop)")
	}
	if out.StatusBar == nil {
		t.Errorf("policy[0] did not stamp OutReply's StatusBar")
	}

	// policy[1] (think gate) drops OutThinking when Hide.
	out2 := &messages.OutboundMessage{Kind: messages.OutThinking, ChatID: cs.ChatID}
	if drop := policies[1].Apply(out2, makeEnvelope(cs)); !drop {
		t.Errorf("policy[1] did not drop OutThinking under Hide mode")
	}

	// policy[2] (tools gate) drops OutToolStart when Hide.
	out3 := &messages.OutboundMessage{Kind: messages.OutToolStart, ChatID: cs.ChatID}
	if drop := policies[2].Apply(out3, makeEnvelope(cs)); !drop {
		t.Errorf("policy[2] did not drop OutToolStart under Hide mode")
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

// testDevNull is an io.Writer that discards. Used so the
// policy tests can construct a *slog.Logger without polluting
// test output (each Info-level "X dropped" log line would
// otherwise show up).
type testDevNull struct{ t *testing.T }

func (d testDevNull) Write(p []byte) (int, error) { return len(p), nil }