// Tests for the operator that publishes the run's events.
//
// The run's thinking and the run's tool calls travel the outbound
// path: the chat renders them the way it renders a coding agent's.
// The chat's own settings gate them the way they gate the agent's.

package runtime

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// recordingEmitter records the outbound messages it is given.
type recordingEmitter struct {
	mu   sync.Mutex
	sent []messages.OutboundMessage
}

func (e *recordingEmitter) Send(_ context.Context, msg messages.OutboundMessage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, msg)
	return nil
}

func (e *recordingEmitter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sent)
}

// chatSessionFor returns a chat session the operator's chat resolves
// to, and registers its manager so the operator finds it.
func chatSessionFor(t *testing.T, chatID string, em messages.Emitter) *chatsession.ChatSession {
	t.Helper()
	mgr := chatsession.NewManager()
	if em != nil {
		mgr.WithEmitter(em)
	}
	cs, err := mgr.GetOrCreate(chatID, "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	registerMgrInAllMgrs(mgr)
	t.Cleanup(func() { unregisterMgrFromAllMgrs(mgr) })
	return cs
}

// TestOperator_Emit_ReachesTheChannelWithoutAChatSession — a run that
// was resumed after a restart finds no chat session, and its events
// still reach the chat.
func TestOperator_Emit_ReachesTheChannelWithoutAChatSession(t *testing.T) {
	sends := &capturedSends{}
	ch := newStubChannel("me_stub", sends)
	op := newOperator(nil, []channel.Channel{ch}, slog.Default())

	err := op.Emit(context.Background(), "ms_test", messages.OutboundMessage{
		Kind: messages.OutThinking,
		Text: "the reasoning",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := sends.messages()
	if len(got) != 1 {
		t.Fatalf("the channel got %d messages, want 1", len(got))
	}
	if got[0].Kind != messages.OutThinking {
		t.Fatalf("the channel got a %s, want the thinking", got[0].Kind)
	}
	if got[0].ChatID != "ms_test" {
		t.Fatalf("the message names the chat %q, want ms_test", got[0].ChatID)
	}
}

// TestOperator_Emit_PublishesThroughTheChatSessionsEmitter — the
// events travel the same wire the coding agent's events travel.
func TestOperator_Emit_PublishesThroughTheChatSessionsEmitter(t *testing.T) {
	sends := &capturedSends{}
	ch := newStubChannel("me_stub", sends)
	em := &recordingEmitter{}
	cs := chatSessionFor(t, "ms_test", em)
	if err := cs.SetThinkMode(chatsession.ThinkModeShow); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	op := newOperator(nil, []channel.Channel{ch}, slog.Default())

	err := op.Emit(context.Background(), "ms_test", messages.OutboundMessage{
		Kind: messages.OutThinking,
		Text: "the reasoning",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if em.count() != 1 {
		t.Fatalf("the emitter got %d messages, want 1", em.count())
	}
	if sends.count() != 0 {
		t.Fatalf("the channel got %d messages, want the emitter to carry them", sends.count())
	}
}

// TestOperator_Emit_DropsTheThinkingWhenTheChatHidesIt — /think off
// hides the run's reasoning the way it hides the agent's.
func TestOperator_Emit_DropsTheThinkingWhenTheChatHidesIt(t *testing.T) {
	sends := &capturedSends{}
	ch := newStubChannel("me_stub", sends)
	em := &recordingEmitter{}
	cs := chatSessionFor(t, "ms_test", em)
	if err := cs.SetThinkMode(chatsession.ThinkModeHide); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	op := newOperator(nil, []channel.Channel{ch}, slog.Default())

	err := op.Emit(context.Background(), "ms_test", messages.OutboundMessage{
		Kind: messages.OutThinking,
		Text: "the reasoning",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if em.count() != 0 {
		t.Fatalf("the emitter got %d messages, want none", em.count())
	}
	if sends.count() != 0 {
		t.Fatalf("the channel got %d messages, want none", sends.count())
	}
}

// TestOperator_Emit_DropsTheToolEventsWhenTheChatHidesThem — /tools
// off hides the run's tool calls the way it hides the agent's.
func TestOperator_Emit_DropsTheToolEventsWhenTheChatHidesThem(t *testing.T) {
	sends := &capturedSends{}
	ch := newStubChannel("me_stub", sends)
	em := &recordingEmitter{}
	cs := chatSessionFor(t, "ms_test", em)
	if err := cs.SetThinkMode(chatsession.ThinkModeShow); err != nil {
		t.Fatalf("SetThinkMode: %v", err)
	}
	if err := cs.SetToolsMode(chatsession.ToolsModeHide); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	op := newOperator(nil, []channel.Channel{ch}, slog.Default())

	for _, kind := range []messages.OutboundKind{messages.OutToolStart, messages.OutToolEnd} {
		err := op.Emit(context.Background(), "ms_test", messages.OutboundMessage{
			Kind: kind,
			Tool: &messages.ToolInfo{Name: "run_step", Args: "commit"},
		})
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	if em.count() != 0 {
		t.Fatalf("the emitter got %d messages, want none", em.count())
	}
	if sends.count() != 0 {
		t.Fatalf("the channel got %d messages, want none", sends.count())
	}
}

// TestOperator_Emit_KeepsTheToolEventsWhenTheChatShowsThem — /tools
// on lets the run's tool calls through.
func TestOperator_Emit_KeepsTheToolEventsWhenTheChatShowsThem(t *testing.T) {
	sends := &capturedSends{}
	ch := newStubChannel("me_stub", sends)
	em := &recordingEmitter{}
	cs := chatSessionFor(t, "ms_test", em)
	if err := cs.SetToolsMode(chatsession.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode: %v", err)
	}
	op := newOperator(nil, []channel.Channel{ch}, slog.Default())

	err := op.Emit(context.Background(), "ms_test", messages.OutboundMessage{
		Kind: messages.OutToolStart,
		Tool: &messages.ToolInfo{Name: "run_step", Args: "commit"},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if em.count() != 1 {
		t.Fatalf("the emitter got %d messages, want 1", em.count())
	}
}

// TestOperator_Emit_RefusesAChatNoChannelOwns — a chat of a channel
// the daemon does not run has nowhere to publish.
func TestOperator_Emit_RefusesAChatNoChannelOwns(t *testing.T) {
	ch := newStubChannel("me_stub", nil)
	op := newOperator(nil, []channel.Channel{ch}, slog.Default())

	if err := op.Emit(context.Background(), "tg_test", messages.OutboundMessage{
		Kind: messages.OutThinking,
	}); err == nil {
		t.Fatal("the message was accepted")
	}
}
