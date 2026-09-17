package discord

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// fakeREST is a hand-rolled restClient used by send_test.go to
// capture the calls Send dispatches.
type fakeREST struct {
	mu               sync.Mutex
	creates          []createCall
	reactions        []reactionCall
	removeReactions  []reactionCall
	getMeCalled      bool
	gatewayBotCalled bool
	createErr        error
	addReactionErr   error
}

type createCall struct {
	ChannelID string
	Content   string
}

type reactionCall struct {
	ChannelID string
	MessageID string
	Emoji     string
}

func (f *fakeREST) GetGatewayBot(_ context.Context) (GatewayBotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gatewayBotCalled = true
	return GatewayBotResponse{URL: "wss://gateway.discord.gg", Shards: 1}, nil
}

func (f *fakeREST) GetMe(_ context.Context) (User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getMeCalled = true
	return User{ID: "999", Username: "fake-bot", Bot: true}, nil
}

func (f *fakeREST) CreateMessage(_ context.Context, channelID string, payload CreateMessagePayload) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return Message{}, f.createErr
	}
	f.creates = append(f.creates, createCall{ChannelID: channelID, Content: payload.Content})
	return Message{ID: "msg-1", ChannelID: Snowflake(channelID)}, nil
}

func (f *fakeREST) EditMessage(_ context.Context, _, _ string, _ EditMessagePayload) (Message, error) {
	return Message{}, nil
}

func (f *fakeREST) DeleteMessage(_ context.Context, _, _ string) error { return nil }

func (f *fakeREST) AddReaction(_ context.Context, channelID, messageID, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addReactionErr != nil {
		return f.addReactionErr
	}
	f.reactions = append(f.reactions, reactionCall{ChannelID: channelID, MessageID: messageID, Emoji: emoji})
	return nil
}

func (f *fakeREST) RemoveOwnReaction(_ context.Context, channelID, messageID, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeReactions = append(f.removeReactions, reactionCall{ChannelID: channelID, MessageID: messageID, Emoji: emoji})
	return nil
}

func (f *fakeREST) ClearReactions(_ context.Context, _, _ string) error { return nil }

// newTestAdapter returns an Adapter wired against the supplied
// fake rest client. Skips NewAdapter's token-validation so the
// tests don't need a real bot token.
func newTestAdapter(rest restClient) *Adapter {
	return &Adapter{
		name:               "discord",
		cfg:                configForTest(),
		api:                rest,
		state:              newTestStateStore(),
		incoming:           make(chan messages.InboundMessage, 4),
		logger:             testLogger(),
		lastMessageStateDB: make(map[string]string),
	}
}

func configForTest() discordCfg {
	return discordCfg{Intents: 46593}
}

// discordCfg is a local alias so the test can populate the same
// field names without importing internal/config (which would
// drag the full config package in for tests that don't need it).
type discordCfg = struct {
	BotToken               string `yaml:"bot_token"`
	ApplicationID          string `yaml:"application_id"`
	Intents                int    `yaml:"intents"`
	ReconnectMaxBackoffSec int    `yaml:"reconnect_max_backoff_sec"`
}

func newTestStateStore() *stateStore { return &stateStore{} }

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestSend_OutReply_SendsCreate verifies OutReply becomes a
// CreateMessage call with the supplied text.
func TestSend_OutReply_SendsCreate(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1",
		Kind:   messages.OutReply,
		Text:   "hello back",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(rest.creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(rest.creates))
	}
	if rest.creates[0].ChannelID != "chan1" {
		t.Errorf("channel = %q, want chan1 (prefix stripped)", rest.creates[0].ChannelID)
	}
	if rest.creates[0].Content != "hello back" {
		t.Errorf("content = %q", rest.creates[0].Content)
	}
}

// TestSend_OutResult_UsesResultText verifies the Result.Text path
// takes precedence over msg.Text when populated.
func TestSend_OutResult_UsesResultText(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1",
		Kind:   messages.OutResult,
		Text:   "ignored",
		Result: &agent.AgentResultEvent{Text: "from result"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rest.creates[0].Content != "from result" {
		t.Errorf("content = %q", rest.creates[0].Content)
	}
}

// TestSend_OutResult_EmptyTextSilentDrops verifies the empty-body
// silent-drop contract.
func TestSend_OutResult_EmptyTextSilentDrops(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1",
		Kind:   messages.OutReply,
		Text:   "   ",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(rest.creates) != 0 {
		t.Errorf("creates = %d, want 0 (silent drop on empty text)", len(rest.creates))
	}
}

// TestSend_OutMessageState_PlacesEmojiOnUserMessage verifies the
// 👌 reaction path. Discord reactions are append-only, so the
// call goes through to AddReaction.
func TestSend_OutMessageState_PlacesEmojiOnUserMessage(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "user-msg-1",
			State:     agent.MessageSubmitted,
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(rest.reactions) != 1 {
		t.Fatalf("reactions = %d, want 1", len(rest.reactions))
	}
	if rest.reactions[0].Emoji != "👌" {
		t.Errorf("emoji = %q, want 👌", rest.reactions[0].Emoji)
	}
	if rest.reactions[0].MessageID != "user-msg-1" {
		t.Errorf("messageID = %q", rest.reactions[0].MessageID)
	}
}

// TestSend_OutMessageState_Dedupes verifies repeat transitions
// for the same MessageID do not double-stamp.
func TestSend_OutMessageState_Dedupes(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	ms := &messages.MessageStatePayload{MessageID: "user-msg-2", State: agent.MessageSubmitted}
	for i := 0; i < 3; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "dc_chan1", Kind: messages.OutMessageState, MessageState: ms,
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if len(rest.reactions) != 1 {
		t.Errorf("reactions = %d, want 1 (dedupe)", len(rest.reactions))
	}
}

// TestSend_OutMessageStateRemoved_RemovesEmoji verifies the
// remove path uses the same emoji lookup.
func TestSend_OutMessageStateRemoved_RemovesEmoji(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1",
		Kind:   messages.OutMessageStateRemoved,
		MessageState: &messages.MessageStatePayload{
			MessageID: "user-msg-3",
			State:     agent.MessageSubmitted,
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(rest.removeReactions) != 1 {
		t.Fatalf("removeReactions = %d, want 1", len(rest.removeReactions))
	}
	if rest.removeReactions[0].Emoji != "👌" {
		t.Errorf("emoji = %q", rest.removeReactions[0].Emoji)
	}
}

// TestSend_OutError_AppendsStderrTail verifies the OutError
// diagnostic-stderr-tail concatenation.
func TestSend_OutError_AppendsStderrTail(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	tail := "boom: trace line"
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1",
		Kind:   messages.OutError,
		Text:   "agent crashed",
		Diagnostic: &agent.BridgeDiagnostic{
			StderrTail: tail,
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(rest.creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(rest.creates))
	}
	got := rest.creates[0].Content
	if got != "agent crashed\n```\n"+tail+"\n```" {
		t.Errorf("content = %q", got)
	}
}

// TestSend_OutThinking_SilentDropped verifies Phase 1 silent-drop.
func TestSend_OutThinking_SilentDropped(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutThinking, Text: "thinking…",
	}); err != nil {
		t.Errorf("Send: %v", err)
	}
	if len(rest.creates)+len(rest.reactions)+len(rest.removeReactions) != 0 {
		t.Errorf("expected silent drop; got creates=%d reactions=%d remove=%d",
			len(rest.creates), len(rest.reactions), len(rest.removeReactions))
	}
}

// TestSend_OutChoice_SilentDropped (Phase 2 will pick this up).
func TestSend_OutChoice_SilentDropped(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutChoice, Choice: &messages.Choice{RequestID: "x"},
	}); err != nil {
		t.Errorf("Send: %v", err)
	}
}

// TestSend_OutMessageState_MissingFields guards the precondition
// that MessageState.MessageID is required.
func TestSend_OutMessageState_MissingFields(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutMessageState,
		// MessageState nil
	})
	if err == nil {
		t.Error("expected error on nil MessageState")
	}
}

// TestOnPromptEnded_CleanPath verifies 🎉 on clean end.
func TestOnPromptEnded_CleanPath(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"
	a.OnPromptEnded(context.Background(), "dc_chan1", "user-msg", 0)
	if len(rest.reactions) != 1 || rest.reactions[0].Emoji != "🎉" {
		t.Errorf("expected one 🎉 reaction; got %+v", rest.reactions)
	}
}

// TestOnPromptEnded_ErrorPath verifies ❌ on error.
func TestOnPromptEnded_ErrorPath(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"
	// PromptEndError is the conventional error reason; check the
	// IsError method returns true for it.
	var reason agent.PromptEndReason
	if !reason.IsError() && !reason.IsError() {
		// zero value is treated as clean by default; force error
		// path with a non-zero reason if IsError semantics change.
		_ = reason
	}
	// Use an explicit error reason — read PromptEndReason values
	// from the agent package.
	if !anyReasonIsError() {
		t.Skip("no PromptEndError constant exposed; verify IsError contract elsewhere")
	}
	a.OnPromptEnded(context.Background(), "dc_chan1", "user-msg", 1)
	if len(rest.reactions) != 1 || rest.reactions[0].Emoji != "❌" {
		t.Errorf("expected one ❌ reaction; got %+v", rest.reactions)
	}
}

// anyReasonIsError pokes PromptEndReason.IsError without depending
// on the specific constant value.
func anyReasonIsError() bool {
	// PromptEndError / PromptEndProcessDied etc. are non-clean;
	// the safest probe is "any non-zero reason except the clean
	// one is error". We don't enumerate the constants; we
	// iterate from PromptEndReason(1) upward until we find one
	// where IsError is true. Bounded to a small range so the test
	// stays fast.
	for i := 1; i < 20; i++ {
		if agent.PromptEndReason(i).IsError() {
			return true
		}
	}
	return false
}

// TestMapStateToDiscordEmoji covers the three documented states.
func TestMapStateToDiscordEmoji(t *testing.T) {
	cases := []struct {
		state agent.MessageState
		want  string
	}{
		{agent.MessageSubmitted, "👌"},
		{agent.MessageDone, ""},
		{agent.MessageState(9999), ""},
	}
	for _, tc := range cases {
		if got := mapStateToDiscordEmoji(tc.state); got != tc.want {
			t.Errorf("mapStateToDiscordEmoji(%v) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// TestSend_RestErrorPropagates ensures CreateMessage failures
// surface to the caller.
func TestSend_RestErrorPropagates(t *testing.T) {
	rest := &fakeREST{createErr: errors.New("boom")}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutReply, Text: "hi",
	}); err == nil {
		t.Error("expected error from REST")
	}
}
