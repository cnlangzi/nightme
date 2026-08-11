package inbound

import (
	"context"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/gateway/inbound/teststubs"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestDispatchInbound_ActionBranch covers the F-50 §6.1 reaction
// routing path: when msg.Reaction (or msg.Action) is set,
// Dispatch must route to tryActionDispatch, NOT to
// tryMessageDispatch (the agent loop). Without this branch the
// whole reaction pipeline is dead end-to-end — reactions get
// pushed onto the channel, hit Dispatch, fail to match a
// slash command, and (pre-fix) used to be sent to the agent
// as empty text. That was F-45 review finding #9+#10.
func TestDispatchInbound_ActionBranch(t *testing.T) {
	const chatID = "oc_chat"

	t.Run("msg.Reaction routes to ReactionRouter", func(t *testing.T) {
		action := teststubs.NewReaction(true)
		msg := teststubs.NewMessage(chatsession.NewManager())
		r := New(msg, teststubs.AlwaysFallThrough{}, teststubs.AlwaysFallThroughShell{}, action, "primary")

		res, err := r.Dispatch(context.Background(), &messages.InboundMessage{
			ChatID: chatID,
			Text:   "", // reactions have no text
			Reaction: &commandServices.ReactionEvent{
				TargetMsgID: "om_card_abc",
				Emoji:       "✅",
				UserID:      "ou_user_1",
				ChatID:      chatID,
			},
		})
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if res == nil || !res.Consumed {
			t.Fatalf("result = %+v, want Consumed=true", res)
		}
		if len(action.Events) != 1 {
			t.Fatalf("ReactionRouter got %d events, want 1", len(action.Events))
		}
		if action.Events[0].TargetMsgID != "om_card_abc" {
			t.Errorf("ReactionEvent = %+v, want TargetMsgID=om_card_abc", action.Events[0])
		}
		if action.CtxSeen == nil {
			t.Error("ctx not propagated to ReactionRouter")
		}
		// The message handler (agent loop) must NOT be called.
		if msg.Hits() != 0 {
			t.Errorf("msg.hits = %d, want 0 (action branch owned the event)", msg.Hits())
		}
	})

	t.Run("router returns false → drop, no agent dispatch", func(t *testing.T) {
		// Router ran but decided not to consume (e.g. no matching
		// gtwDraft, or emoji wasn't recognised). Should be marked
		// Consumed=true Dropped=true so the runtime knows the
		// gateway took ownership; the agent loop is not invoked.
		action := teststubs.NewReaction(false)
		msg := teststubs.NewMessage(chatsession.NewManager())
		r := New(msg, teststubs.AlwaysFallThrough{}, teststubs.AlwaysFallThroughShell{}, action, "primary")

		res, err := r.Dispatch(context.Background(), &messages.InboundMessage{
			ChatID: chatID,
			Text:   "",
			Reaction: &commandServices.ReactionEvent{
				TargetMsgID: "om_orphan",
				Emoji:       "👀",
				ChatID:      chatID,
			},
		})
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if res == nil || !res.Consumed || !res.Dropped {
			t.Errorf("result = %+v, want Consumed=true Dropped=true (router declined)", res)
		}
		if len(action.Events) != 1 {
			t.Errorf("router hits = %d, want 1", len(action.Events))
		}
		if msg.Hits() != 0 {
			t.Errorf("msg.hits = %d, want 0 (router declined, no agent dispatch)", msg.Hits())
		}
	})

	t.Run("plain text routes to message handler (regression guard)", func(t *testing.T) {
		// Sanity: adding the action branch must not break the
		// plain-text path. A non-action message with text should
		// still go to the message handler for the agent loop.
		action := teststubs.NewReaction(true)
		msg := teststubs.NewMessage(chatsession.NewManager())
		r := New(msg, teststubs.AlwaysFallThrough{}, teststubs.AlwaysFallThroughShell{}, action, "primary")

		_, err := r.Dispatch(context.Background(), &messages.InboundMessage{
			ChatID:     chatID,
			UserID:     "ou_user_1",
			Text:       "hello world",
			HasMention: true,
		})
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if len(action.Events) != 0 {
			t.Errorf("router events = %d, want 0 (plain text must not hit router)", len(action.Events))
		}
		if msg.Hits() != 1 {
			t.Errorf("msg.hits = %d, want 1 (plain text must hit agent)", msg.Hits())
		}
	})
}

// Compile-time guard that the teststubs satisfy the inbound
// interfaces (a drift in the interface signature would fail
// the build at this line, before any test runs).
var (
	_ MessageHandler    = (*teststubs.Message)(nil)
	_ CommandDispatcher = teststubs.AlwaysFallThrough{}
	_ ShellDispatcher   = teststubs.AlwaysFallThroughShell{}
	_ ReactionRouter    = (*teststubs.Reaction)(nil)
)

// Reference command packages so the import isn't flagged as
// unused when this test file is the only consumer.
var _ = command.SlashInput{}
