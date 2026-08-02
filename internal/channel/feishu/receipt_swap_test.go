package feishu

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
)

// mockReceiptAdapter records AddReaction / UpdateMessage /
// SendMessageText calls for the append-only reaction tests.
type mockReceiptAdapter struct {
	mu sync.Mutex

	added     []receiptReactionCall
	updated   []receiptUpdateCall
	sentText  []receiptSendCall
	addErr    error
	updateErr error
	sendErr   error
}

type receiptReactionCall struct{ MessageID, Emoji string }

type receiptUpdateCall struct {
	MessageID string
	Text      string
}

type receiptSendCall struct {
	ChatID string
	Text   string
}

func (m *mockReceiptAdapter) snapshotAdded() []receiptReactionCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]receiptReactionCall, len(m.added))
	copy(out, m.added)
	return out
}

func (m *mockReceiptAdapter) snapshotSentText() []receiptSendCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]receiptSendCall, len(m.sentText))
	copy(out, m.sentText)
	return out
}

func (m *mockReceiptAdapter) AddReaction(_ context.Context, msgID, emoji string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addErr != nil {
		return "", m.addErr
	}
	m.added = append(m.added, receiptReactionCall{msgID, emoji})
	return "reaction-id", nil
}

func (m *mockReceiptAdapter) UpdateMessage(_ context.Context, msgID, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updated = append(m.updated, receiptUpdateCall{msgID, text})
	return m.updateErr
}

func (m *mockReceiptAdapter) SendMessageText(_ context.Context, chatID, text string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		return "", m.sendErr
	}
	m.sentText = append(m.sentText, receiptSendCall{chatID, text})
	return "new-message-id", nil
}

func newAppendOnlyReceipt(bot receiptBot) *MessageReceipt {
	return NewMessageReceiptForReply("chat1", "user-message-1", "reply-message-1", bot)
}

func TestReactionAppendOnly_FullLifecycleAddsEachStateOnce(t *testing.T) {
	ctx := context.Background()
	bot := &mockReceiptAdapter{}
	r := newAppendOnlyReceipt(bot)

	// Mirror the production seed: NewMessageReceipt posts the OK
	// reaction before the receipt is returned.
	if _, err := bot.AddReaction(ctx, r.userMsgID, StateWaiting.Emoji()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.currentReaction = StateWaiting.Emoji()
	r.mu.Unlock()

	if err := r.SetExecuting(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.SetCompleted(ctx); err != nil {
		t.Fatal(err)
	}

	want := []string{"OK", "OnIt", "PARTY"}
	added := bot.snapshotAdded()
	if len(added) != len(want) {
		t.Fatalf("added %d reactions, want %d", len(added), len(want))
	}
	for i, emoji := range want {
		if added[i].MessageID != r.userMsgID || added[i].Emoji != emoji {
			t.Errorf("added[%d] = %+v, want message %q emoji %q", i, added[i], r.userMsgID, emoji)
		}
	}
}

func TestReactionAppendOnly_SameStateSkipsDuplicate(t *testing.T) {
	ctx := context.Background()
	bot := &mockReceiptAdapter{}
	r := newAppendOnlyReceipt(bot)

	if err := r.SetExecuting(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}

	if got := len(bot.snapshotAdded()); got != 1 {
		t.Errorf("added %d reactions, want 1 (heartbeat must not re-add OnIt)", got)
	}
}

func TestReactionAppendOnly_AddFailureKeepsReplyShipping(t *testing.T) {
	bot := &mockReceiptAdapter{addErr: errors.New("simulated add failure")}
	r := newAppendOnlyReceipt(bot)

	if err := r.Append(context.Background(), agent.AgentEvent{
		Kind: agent.EventText,
		Text: "still ship this",
	}); err != nil {
		t.Fatal(err)
	}

	if r.currentReaction != "" {
		t.Fatalf("currentReaction = %q after failed add, want empty (retryable)", r.currentReaction)
	}
	if got := len(bot.snapshotSentText()); got != 1 {
		t.Fatalf("shipped %d replies after failed add, want 1", got)
	}
}

func TestReactionAppendOnly_ApplyStateAppendsWithoutDelete(t *testing.T) {
	ctx := context.Background()
	bot := &mockReceiptAdapter{}
	r := newAppendOnlyReceipt(bot)

	if err := r.applyState(ctx, channel.ReceiptExecuting); err != nil {
		t.Fatal(err)
	}
	if err := r.applyState(ctx, channel.ReceiptDone); err != nil {
		t.Fatal(err)
	}

	want := []string{"OnIt", "PARTY"}
	added := bot.snapshotAdded()
	if len(added) != len(want) {
		t.Fatalf("added %d reactions, want %d", len(added), len(want))
	}
	for i, emoji := range want {
		if added[i].Emoji != emoji {
			t.Errorf("added[%d].Emoji = %q, want %q", i, added[i].Emoji, emoji)
		}
	}
}

func TestReactionAppendOnly_DisposeLeavesReactions(t *testing.T) {
	ctx := context.Background()
	bot := &mockReceiptAdapter{}
	r := newAppendOnlyReceipt(bot)

	if err := r.applyState(ctx, channel.ReceiptExecuting); err != nil {
		t.Fatal(err)
	}
	before := len(bot.snapshotAdded())
	if err := r.dispose(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(bot.snapshotAdded()); got != before {
		t.Fatalf("dispose changed reaction count: before=%d after=%d", before, got)
	}
}
