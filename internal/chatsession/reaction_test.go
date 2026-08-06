package chatsession

import (
	"context"
	"testing"
)

func TestHandleAction_NoHandler(t *testing.T) {
	cs := New("chat-1", "primary")
	consumed := cs.HandleAction(context.Background(), ReactionEvent{
		TargetMsgID: "msg-1",
		Emoji:       "✅",
		ChatID:      "chat-1",
	})
	if consumed {
		t.Errorf("consumed = true, want false (no handler installed)")
	}
}

func TestHandleAction_DispatchesToHandler(t *testing.T) {
	cs := New("chat-1", "primary")

	var (
		gotEv  ReactionEvent
		gotCtx context.Context
		calls  int
	)
	cs.SetActionHandler(func(ctx context.Context, ev ReactionEvent) bool {
		gotEv = ev
		gotCtx = ctx
		calls++
		return true
	})

	want := ReactionEvent{
		TargetMsgID: "om_abc",
		Emoji:       "🆕",
		UserID:      "ou_xyz",
		ChatID:      "chat-1",
	}
	consumed := cs.HandleAction(context.Background(), want)
	if !consumed {
		t.Fatal("consumed = false, want true (handler returned true)")
	}
	if calls != 1 {
		t.Errorf("handler calls = %d, want 1", calls)
	}
	if gotEv != want {
		t.Errorf("handler ev = %+v, want %+v", gotEv, want)
	}
	if gotCtx == nil {
		t.Error("handler received nil context")
	}
}

func TestHandleAction_HandlerFalse(t *testing.T) {
	cs := New("chat-1", "primary")
	cs.SetActionHandler(func(_ context.Context, _ ReactionEvent) bool {
		return false
	})
	if cs.HandleAction(context.Background(), ReactionEvent{TargetMsgID: "x"}) {
		t.Error("consumed = true, want false")
	}
}

func TestSetActionHandler_NilClears(t *testing.T) {
	cs := New("chat-1", "primary")
	cs.SetActionHandler(func(_ context.Context, _ ReactionEvent) bool { return true })
	cs.SetActionHandler(nil)
	if cs.HandleAction(context.Background(), ReactionEvent{TargetMsgID: "x"}) {
		t.Error("after nil-clear, handler should not be called")
	}
	if cs.ActionHandler() != nil {
		t.Error("ActionHandler() should return nil after nil-clear")
	}
}