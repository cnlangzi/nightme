package main

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway"
)

// TestNewRuntimeStamper_NilMgr verifies that a nil Manager is
// handled defensively — production always provides one, but tests
// or future misconfigurations shouldn't panic. Returning nil
// matches the contract documented on newRuntimeStamper.
func TestNewRuntimeStamper_NilMgr(t *testing.T) {
	stamper := newRuntimeStamper(nil)
	if got := stamper("any-chat"); got != nil {
		t.Errorf("nil mgr should produce nil SessionContext, got %+v", got)
	}
}

// TestNewRuntimeStamper_UnknownChatID verifies the stamper looks
// up chat by id — a chat that was never created yields nil rather
// than panicking on a missing map entry.
func TestNewRuntimeStamper_UnknownChatID(t *testing.T) {
	mgr := chatsession.NewManager()
	stamper := newRuntimeStamper(mgr)
	if got := stamper("never-created"); got != nil {
		t.Errorf("unknown chatID should produce nil SessionContext, got %+v", got)
	}
}

// TestNewRuntimeStamper_NoActiveAS verifies the stamper yields
// nil when the chat exists but no AgentSession has been selected
// yet (early in /cwd setup, for example). selectedAS is set
// internally by attachAgentSession which is unexported, so we
// can't seed a non-nil AS from outside the chatsession package —
// this test exercises the "chat exists, no AS" branch.
func TestNewRuntimeStamper_NoActiveAS(t *testing.T) {
	mgr := chatsession.NewManager()
	_, _ = mgr.GetOrCreate("oc_no_as", "claude")
	stamper := newRuntimeStamper(mgr)
	if got := stamper("oc_no_as"); got != nil {
		t.Errorf("chat with no selected AS should produce nil SessionContext, got %+v", got)
	}
}

// TestBuildSessionContext_AllEmptyReturnsNil verifies the gate
// fires: a fresh AgentSession with no identity fields populated
// and no usage → buildSessionContext returns nil rather than
// producing an empty SessionContext (which would render as a
// blank footer line).
func TestBuildSessionContext_AllEmptyReturnsNil(t *testing.T) {
	as := agentsession.NewAgentSession("as_x", "cs_x", "", "/tmp", nil)
	if got := buildSessionContext(as, nil); got != nil {
		t.Errorf("buildSessionContext with no fields populated should return nil, got %+v", got)
	}
}

// TestBuildSessionContext_UsageOnlyReturnsPopulatedSC covers the
// "event has usage but no AS state yet" case (e.g. usage arrives
// before the AS reports its model). Usage alone is enough to
// materialize a SessionContext.
func TestBuildSessionContext_UsageOnlyReturnsPopulatedSC(t *testing.T) {
	as := agentsession.NewAgentSession("as_x", "cs_x", "", "/tmp", nil)
	usage := &agent.UsageInfo{InputTokens: 5, OutputTokens: 7}
	got := buildSessionContext(as, usage)
	if got == nil {
		t.Fatal("usage should be sufficient to materialize SessionContext")
	}
	if got.Usage != usage {
		t.Errorf("Usage = %+v, want %+v", got.Usage, usage)
	}
}

// TestSessionContextInto_LegacyGate verifies sessionContextInto
// still works for the MessageStateBus path (which pre-stamps
// explicitly so the emitter's stamper doesn't double-stamp).
func TestSessionContextInto_LegacyGate(t *testing.T) {
	as := agentsession.NewAgentSession("as_legacy", "cs_l", "claude", "/tmp", nil)
	msg := &gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_l",
		Usage:  &agent.UsageInfo{InputTokens: 1, OutputTokens: 1},
	}
	sessionContextInto(msg, as)
	if msg.SessionContext == nil {
		t.Fatal("MessageSubmitted-style path must populate SessionContext")
	}
	if msg.SessionContext.Usage == nil {
		t.Error("Usage should be co-located on SessionContext for legacy gate path")
	}
}
