// Package runtime — the background-subagent report on the runtime
// path.
//
// The report is an addition to the turn's own events: it reaches the
// channel with the same anchor the turn's other events carry, so the
// channel routes it to the turn's receipt card.
package runtime

import (
	"io"
	"log/slog"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestEventHandler_Subagents_CarriesTheTurnAnchor pins the routing
// contract: the report reaches the channel with the turn's anchor, so
// the channel finds the receipt card of the turn it belongs to.
func TestEventHandler_Subagents_CarriesTheTurnAnchor(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, slog.Default(), chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentSubagents,
		Subagents: &agent.SubagentEvent{Tasks: []agent.SubagentTask{
			{TaskID: "a1", Description: "one", TaskType: "local_agent", Status: agent.BgTaskStatusRunning},
			{TaskID: "a2", Description: "two", TaskType: "local_agent", Status: agent.BgTaskStatusRunning},
		}},
	}, UserMsgID: "om_user_1"})

	recorded := ch.Record()
	var got *messages.OutboundMessage
	for i := range recorded {
		if recorded[i].Kind == messages.OutSubagents {
			got = &recorded[i]
		}
	}
	if got == nil {
		t.Fatalf("expected OutSubagents in channel record, got: %+v", recorded)
	}
	if got.ReplyTo != "om_user_1" {
		t.Errorf("ReplyTo = %q, want om_user_1 (the turn's anchor)", got.ReplyTo)
	}
	if got.ChatID != "oc_chat" {
		t.Errorf("ChatID = %q, want oc_chat", got.ChatID)
	}
	if got.Subagents == nil || len(got.Subagents.Tasks) != 2 {
		t.Fatalf("Subagents payload = %+v, want 2 tasks", got.Subagents)
	}
}

// TestEventHandler_Subagents_DoesNotDisturbTheTurn: the report is an
// addition — the turn's own events keep their kind and their anchor.
func TestEventHandler_Subagents_DoesNotDisturbTheTurn(t *testing.T) {
	ch := echo.New("test", io.Discard)
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("oc_chat", "claude")

	h := NewEventHandler(outbound.New(ch, outbound.Options{}), cs, mgr, slog.Default(), chatsession.GitStatusDeps{})
	as := chatsession.NewAgentSession("as_test", "cs_oc_chat", "claude", "/tmp", nil)

	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "the agent's own reply",
	}, UserMsgID: "om_user_1"})

	h(chatsession.AgentEventEnvelope{ChatID: "oc_chat", AgentSession: as, Event: &agent.AgentEvent{
		Kind:      agent.EventAgentSubagents,
		Subagents: &agent.SubagentEvent{Tasks: []agent.SubagentTask{}},
	}, UserMsgID: "om_user_1"})

	recorded := ch.Record()
	var reply, report *messages.OutboundMessage
	for i := range recorded {
		switch recorded[i].Kind {
		case messages.OutReply:
			reply = &recorded[i]
		case messages.OutSubagents:
			report = &recorded[i]
		}
	}
	if reply == nil {
		t.Fatalf("the turn's own reply did not reach the channel: %+v", recorded)
	}
	if reply.Text != "the agent's own reply" {
		t.Errorf("reply text = %q, want the agent's own text", reply.Text)
	}
	if reply.ReplyTo != "om_user_1" {
		t.Errorf("reply ReplyTo = %q, want om_user_1", reply.ReplyTo)
	}
	if report == nil {
		t.Fatalf("the report did not reach the channel: %+v", recorded)
	}
	if report.ReplyTo != "om_user_1" {
		t.Errorf("report ReplyTo = %q, want om_user_1", report.ReplyTo)
	}
}
