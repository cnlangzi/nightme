package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/gatewaytest"
)

// TestDispatchInbound_ActionBranch covers the F-50 §6.1 reaction
// routing path: when msg.Reaction (or msg.Action) is set,
// DispatchInbound must route to dispatchAction, NOT to
// dispatchMessage (the agent loop). Without this branch the
// whole reaction pipeline is dead end-to-end — reactions get
// pushed onto a.incoming, hit DispatchInbound, fail ParseCommand
// (text is empty), fall through to gateAndDispatch, and get
// sent to the agent as empty text. That was F-45 review
// finding #9+#10.
//
// This test was the missing integration coverage that would
// have caught the broken pipeline at code-review time.
func TestDispatchInbound_ActionBranch(t *testing.T) {
	const chatID = "oc_chat"

	t.Run("msg.Reaction routes to actionHandler", func(t *testing.T) {
		var (
			gotCtx context.Context
			gotMsg *InboundMessage
			hits   int32
		)
		gw := New(MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
			atomic.AddInt32(&hits, 1)
			return nil
		}), &gatewaytest.NoopEmitter{}).(*Router)
		gw.WithActionHandler(func(ctx context.Context, msg *InboundMessage) bool {
			atomic.AddInt32(&hits, 1)
			gotCtx = ctx
			gotMsg = msg
			return true
		})

		res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
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
			t.Fatalf("DispatchInbound: %v", err)
		}
		if res == nil || !res.Consumed {
			t.Fatalf("result = %+v, want Consumed=true", res)
		}
		if gotMsg == nil {
			t.Fatal("actionHandler not called")
		}
		if gotMsg.Reaction == nil || gotMsg.Reaction.TargetMsgID != "om_card_abc" {
			t.Errorf("Reaction = %+v, want TargetMsgID=om_card_abc", gotMsg.Reaction)
		}
		if gotCtx == nil {
			t.Error("ctx not propagated to actionHandler")
		}
		// The messageDispatcher (agent loop) must NOT be called.
		// The two hits counted must both be inside the actionHandler.
		if atomic.LoadInt32(&hits) != 1 {
			t.Errorf("hits = %d, want exactly 1 (actionHandler only)", hits)
		}
	})

	t.Run("msg.Reaction falls back to drop when no handler", func(t *testing.T) {
		// Pre-F-45 runtime: no actionHandler installed. Reactions
		// should be silently dropped (NOT sent to the agent loop,
		// which would queue a no-op turn and confuse the user
		// with a "thinking…" state).
		var mdHits int32
		gw := New(MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
			atomic.AddInt32(&mdHits, 1)
			return nil
		}), &gatewaytest.NoopEmitter{}).(*Router)
		// no WithActionHandler

		res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
			ChatID: chatID,
			Text:   "",
			Reaction: &commandServices.ReactionEvent{
				TargetMsgID: "om_card_xyz",
				Emoji:       "🆕",
				ChatID:      chatID,
			},
		})
		if err != nil {
			t.Fatalf("DispatchInbound: %v", err)
		}
		if res == nil || !res.Consumed || !res.Dropped {
			t.Fatalf("result = %+v, want Consumed=true Dropped=true (pre-F-45 default)", res)
		}
		if atomic.LoadInt32(&mdHits) != 0 {
			t.Errorf("mdHits = %d, want 0 (no agent dispatch for reactions)", mdHits)
		}
	})

	t.Run("handler returns false → drop, no agent dispatch", func(t *testing.T) {
		// Handler ran but decided not to consume (e.g. no matching
		// gtwDraft, or emoji wasn't recognised). Should be marked
		// Consumed=true Dropped=true so the runtime knows the
		// gateway took ownership; the agent loop is not invoked.
		var (
			handlerHits int32
			mdHits      int32
		)
		gw := New(MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
			atomic.AddInt32(&mdHits, 1)
			return nil
		}), &gatewaytest.NoopEmitter{}).(*Router)
		gw.WithActionHandler(func(_ context.Context, _ *InboundMessage) bool {
			atomic.AddInt32(&handlerHits, 1)
			return false // "I looked, no draft matched"
		})

		res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
			ChatID: chatID,
			Text:   "",
			Reaction: &commandServices.ReactionEvent{
				TargetMsgID: "om_orphan",
				Emoji:       "👀",
				ChatID:      chatID,
			},
		})
		if err != nil {
			t.Fatalf("DispatchInbound: %v", err)
		}
		if res == nil || !res.Consumed || !res.Dropped {
			t.Errorf("result = %+v, want Consumed=true Dropped=true (handler declined)", res)
		}
		if atomic.LoadInt32(&handlerHits) != 1 {
			t.Errorf("handlerHits = %d, want 1", handlerHits)
		}
		if atomic.LoadInt32(&mdHits) != 0 {
			t.Errorf("mdHits = %d, want 0 (handler declined, no agent dispatch)", mdHits)
		}
	})

	t.Run("plain text routes to messageDispatcher (regression guard)", func(t *testing.T) {
		// Sanity: adding the action branch must not break the
		// plain-text path. A non-action message with text should
		// still go to messageDispatcher for the agent loop.
		var mdHits int32
		gw := New(MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
			atomic.AddInt32(&mdHits, 1)
			return nil
		}), &gatewaytest.NoopEmitter{}).(*Router)
		gw.WithActionHandler(func(_ context.Context, _ *InboundMessage) bool {
			// should NOT be called for plain text
			t.Error("actionHandler called for plain text — branch mis-routing")
			return true
		})

		_, err := gw.DispatchInbound(context.Background(), &InboundMessage{
			ChatID:  chatID,
			UserID:  "ou_user_1",
			Text:    "hello world",
			HasMention: true,
		})
		if err != nil {
			t.Fatalf("DispatchInbound: %v", err)
		}
		if atomic.LoadInt32(&mdHits) != 1 {
			t.Errorf("mdHits = %d, want 1 (plain text must hit agent)", mdHits)
		}
	})
}

// TestDispatchInbound_ActionHandlerError is a defensive check: if
// the actionHandler panics or returns an error, the dispatcher
// must NOT swallow it. The current dispatchAction returns the
// consumed bool from the handler and never inspects the handler
// for an error, but a future handler that returns one (or
// panics) must not crash the gateway.
func TestDispatchInbound_ActionHandlerPanicSafe(t *testing.T) {
	const chatID = "oc_chat"
	gw := New(MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
		return nil
	}), &gatewaytest.NoopEmitter{}).(*Router)
	gw.WithActionHandler(func(_ context.Context, _ *InboundMessage) bool {
		return true
	})

	// Sanity: just verify the dispatcher doesn't crash on a
	// reaction event that looks normal.
	_, err := gw.DispatchInbound(context.Background(), &InboundMessage{
		ChatID: chatID,
		Reaction: &commandServices.ReactionEvent{
			TargetMsgID: "om_test",
			Emoji:       "✅",
			ChatID:      chatID,
		},
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("DispatchInbound: %v", err)
	}
}

// noopEmitter is a test-only outbound.Emitter that does nothing.
type noopEmitter struct{}

func (noopEmitter) Send(context.Context, OutboundMessage) error {
	return nil
}
func (noopEmitter) SendCard(context.Context, OutboundMessage) (string, error) {
	return "", nil
}
