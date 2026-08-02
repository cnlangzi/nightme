package feishu

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
)

// mockReceiptAdapter records AddReaction / UpdateMessage /
// SendMessageText / SendCard / PatchMessage calls for the
// append-only reaction tests.
type mockReceiptAdapter struct {
	mu sync.Mutex

	added     []receiptReactionCall
	updated   []receiptUpdateCall
	sentText  []receiptSendCall
	sentCard  []receiptCardSendCall
	patched   []receiptCardPatchCall
	addErr    error
	updateErr error
	sendErr   error
	cardErr   error
	patchErr  error
	// nextCardID is the synthetic id returned by SendCard so the
	// receipt's renderLocked can store it as r.cardMsgID. Bump per
	// call to keep ids distinct across receipts in the same test.
	nextCardID int
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

// receiptCardSendCall records a SendCard call. Body is the full
// card JSON envelope; CardMessageID is the id the adapter returns
// (synthesized by the mock).
type receiptCardSendCall struct {
	ChatID        string
	Body          string
	CardMessageID string
}

// receiptCardPatchCall records a PatchMessage call. CardMessageID
// is the addressee; Body is the replacement card JSON envelope.
type receiptCardPatchCall struct {
	CardMessageID string
	Body          string
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

func (m *mockReceiptAdapter) snapshotSentCards() []receiptCardSendCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]receiptCardSendCall, len(m.sentCard))
	copy(out, m.sentCard)
	return out
}

func (m *mockReceiptAdapter) snapshotPatched() []receiptCardPatchCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]receiptCardPatchCall, len(m.patched))
	copy(out, m.patched)
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

func (m *mockReceiptAdapter) SendCard(_ context.Context, chatID, body string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cardErr != nil {
		return "", m.cardErr
	}
	if m.nextCardID == 0 {
		m.nextCardID = 1
	}
	id := fmt.Sprintf("card-msg-%d", m.nextCardID)
	m.nextCardID++
	m.sentCard = append(m.sentCard, receiptCardSendCall{
		ChatID:        chatID,
		Body:          body,
		CardMessageID: id,
	})
	return id, nil
}

func (m *mockReceiptAdapter) PatchMessage(_ context.Context, messageID, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.patched = append(m.patched, receiptCardPatchCall{
		CardMessageID: messageID,
		Body:          body,
	})
	return m.patchErr
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

	want := []string{"OneSecond", "OnIt", "DONE"}
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

// TestReactionAppendOnly_AddFailureKeepsReplyShipping verifies that
// the card path still proceeds when the reaction add fails. The
// failure is non-fatal: appendReactionLocked logs and returns;
// renderLocked then builds + ships the card (SendCard on the first
// render, PatchMessage on subsequent). The currentReaction field
// stays empty so a later successful render can retry the emoji.
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
	// The card path replaces the legacy SendMessageText "reply".
	// After the migration (docs/channel/feishu.md §5) the rolling
	// log surface is an interactive card sent via SendCard on the
	// first render, so assert that the card went out — NOT that
	// a text message was shipped.
	if got := len(bot.snapshotSentCards()); got != 1 {
		t.Fatalf("shipped %d cards after failed add, want 1 (reaction failure is non-fatal)", got)
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

	want := []string{"OnIt", "DONE"}
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
