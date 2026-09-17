package discord

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// fakeREST is a hand-rolled restClient used by send_test.go to
// capture the calls Send dispatches.
type fakeREST struct {
	mu                sync.Mutex
	creates           []createCall
	edits             []editCall
	reactions         []reactionCall
	removeReactions   []reactionCall
	downloads         []downloadCall
	acks              []ackCall
	getMeCalled       bool
	gatewayBotCalled  bool
	createErr         error
	addReactionErr    error
	downloadBody      []byte
	downloadErr       error
	ackErr            error
	ackDeadlineHitErr error
}

type createCall struct {
	ChannelID  string
	Content    string
	Components []Component
}

type editCall struct {
	ChannelID  string
	MessageID  string
	Content    string
	Components []Component
}

type reactionCall struct {
	ChannelID string
	MessageID string
	Emoji     string
}

type downloadCall struct {
	URL string
}

type ackCall struct {
	InteractionID string
	Body          InteractionResponse
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
	f.creates = append(f.creates, createCall{
		ChannelID:  channelID,
		Content:    payload.Content,
		Components: payload.Components,
	})
	return Message{ID: "msg-1", ChannelID: Snowflake(channelID)}, nil
}

func (f *fakeREST) EditMessage(_ context.Context, channelID, messageID string, payload EditMessagePayload) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, editCall{
		ChannelID:  channelID,
		MessageID:  messageID,
		Content:    payload.Content,
		Components: payload.Components,
	})
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

func (f *fakeREST) Download(_ context.Context, url string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads = append(f.downloads, downloadCall{URL: url})
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	if f.downloadBody != nil {
		return f.downloadBody, nil
	}
	return []byte("downloaded"), nil
}

// AcknowledgeInteraction captures the call AND enforces the ctx
// deadline so tests can assert the 2.5 s budget. Set
// ackDeadlineHitErr to simulate a stranded ACK (caller cancelled
// the gateway ctx mid-flight); the fake then returns the supplied
// error so the test can verify the adapter honoured the
// fresh context.Background() deadline.
func (f *fakeREST) AcknowledgeInteraction(ctx context.Context, id, _ string, body InteractionResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.ackDeadlineHitErr != nil {
		return f.ackDeadlineHitErr
	}
	f.acks = append(f.acks, ackCall{InteractionID: id, Body: body})
	if f.ackErr != nil {
		return f.ackErr
	}
	return nil
}

// newTestAdapter returns an Adapter wired against the supplied
// fake rest client. Skips NewAdapter's token-validation so the
// tests don't need a real bot token.
func newTestAdapter(rest restClient) *Adapter {
	return &Adapter{
		name:                  "discord",
		cfg:                   configForTest(),
		api:                   rest,
		state:                 newTestStateStore(),
		incoming:              make(chan messages.InboundMessage, 4),
		logger:                testLogger(),
		choiceStore:           newChoiceStore(),
		lastMessageStateDB:    make(map[string]string),
		lastResultMessageIDDB: make(map[string]string),
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

// TestSend_OutThinking_EmitsBubble verifies Phase 2 emits the
// thinking line as a 💭 -prefixed Discord bubble.
func TestSend_OutThinking_EmitsBubble(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutThinking, Text: "thinking…",
	}); err != nil {
		t.Errorf("Send: %v", err)
	}
	if len(rest.creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(rest.creates))
	}
	if rest.creates[0].Content != "💭 thinking…" {
		t.Errorf("content = %q", rest.creates[0].Content)
	}
}

// TestSend_OutToolStart_EmitsCallLine verifies Phase 2 emits the
// "● Tool(args)" call line. Args are compacted when they're a
// JSON object with file_path (Read/Edit/Write).
func TestSend_OutToolStart_EmitsCallLine(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutToolStart,
		Tool: &messages.ToolInfo{Name: "Read", Args: `{"file_path": "/tmp/foo.go"}`},
	}); err != nil {
		t.Errorf("Send: %v", err)
	}
	if len(rest.creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(rest.creates))
	}
	if got := rest.creates[0].Content; got != "● Read(foo.go)" {
		t.Errorf("content = %q", got)
	}
}

// TestSend_OutToolEnd_EmitsResultLine verifies Phase 2 emits the
// "⎿ result" line and is PII-safe (no raw output for unclassified
// tools).
func TestSend_OutToolEnd_EmitsResultLine(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	raw := "secret-token-very-private-content"
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutToolEnd,
		Tool: &messages.ToolInfo{Name: "mcp_unknown", Output: raw},
	}); err != nil {
		t.Errorf("Send: %v", err)
	}
	if len(rest.creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(rest.creates))
	}
	if got := rest.creates[0].Content; got != "⎿  🔧 mcp_unknown → "+itoa(len(raw))+" bytes" {
		t.Errorf("content = %q", got)
	}
	if strings.Contains(rest.creates[0].Content, raw) {
		t.Errorf("PII leak: raw output %q in result line", raw)
	}
}

// TestSend_OutTaskCreate_EmitsChecklist verifies Phase 2 emits a
// 📋 Tasks header followed by the markdown checklist.
func TestSend_OutTaskCreate_EmitsChecklist(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutTaskCreate,
		TaskList: &agent.AgentTaskListEvent{
			Items: []agent.AgentTaskItem{
				{ID: "1", Subject: "first", Status: agent.TaskPending},
				{ID: "2", Subject: "second", Status: agent.TaskCompleted},
			},
		},
	}); err != nil {
		t.Errorf("Send: %v", err)
	}
	if len(rest.creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(rest.creates))
	}
	got := rest.creates[0].Content
	if !strings.HasPrefix(got, "📋 Tasks") {
		t.Errorf("missing header in %q", got)
	}
	if !strings.Contains(got, "- [ ] first") {
		t.Errorf("missing pending row in %q", got)
	}
	if !strings.Contains(got, "- [x] second") {
		t.Errorf("missing completed row in %q", got)
	}
}

// TestSend_OutTaskUpdate_NilSilentDrops verifies the nil-TaskList
// guard (Phase 2 silent-drop contract).
func TestSend_OutTaskUpdate_NilSilentDrops(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutTaskUpdate,
	}); err != nil {
		t.Errorf("Send: %v", err)
	}
	if len(rest.creates) != 0 {
		t.Errorf("creates = %d, want 0", len(rest.creates))
	}
}

// TestSend_OutChoice_RecordsCardAndStore verifies Phase 2 emits
// the V1 component card via CreateMessage and records the choice
// in the in-memory store so the INTERACTION_CREATE handler can
// resolve the click.
func TestSend_OutChoice_RecordsCardAndStore(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-1",
			Title:     "Waiting for approval",
			Body:      "run go test?",
			Options:   messages.ChoiceOptionsFromLabels([]string{"yes", "no"}),
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(rest.creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(rest.creates))
	}
	if len(rest.creates[0].Components) == 0 {
		t.Fatalf("expected components on choice card; got none")
	}
	state, ok := a.choiceStore.Get("req-1")
	if !ok {
		t.Fatalf("choice state not recorded in store")
	}
	if state.MessageID != "msg-1" {
		t.Errorf("MessageID = %q, want msg-1", state.MessageID)
	}
}

// TestSend_OutChoicePatch_UpdatesOriginal verifies Phase 2 edits
// the existing choice card via REST (not interaction callback).
func TestSend_OutChoicePatch_UpdatesOriginal(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	_ = a.choiceStore.Put(&choiceState{
		RequestID: "req-1",
		ChannelID: "chan1",
		MessageID: "card-msg-1",
		Choice:    &messages.Choice{RequestID: "req-1"},
	})
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutChoicePatch,
		Choice: &messages.Choice{
			RequestID:  "req-1",
			Settled:    true,
			SelectedID: "yes",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(rest.edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(rest.edits))
	}
	if rest.edits[0].MessageID != "card-msg-1" {
		t.Errorf("edit message_id = %q, want card-msg-1", rest.edits[0].MessageID)
	}
	if len(rest.edits[0].Components) != 0 {
		t.Errorf("settled patch should clear components; got %d", len(rest.edits[0].Components))
	}
}

// TestSend_OutChoicePatch_OrphanSilentSkips verifies that a patch
// for an unknown RequestID is a no-op (matches feishu / telegram).
func TestSend_OutChoicePatch_OrphanSilentSkips(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "dc_chan1", Kind: messages.OutChoicePatch,
		Choice: &messages.Choice{RequestID: "req-missing"},
	})
	if err != nil {
		t.Errorf("Send: %v", err)
	}
	if len(rest.edits) != 0 {
		t.Errorf("edits = %d, want 0 (orphan skip)", len(rest.edits))
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

// TestOnPromptEnded_CleanPath verifies 🎉 on clean end (no
// recorded result message id → falls back to user message id).
func TestOnPromptEnded_CleanPath(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"
	a.OnPromptEnded(context.Background(), "dc_chan1", "user-msg", 0)
	if len(rest.reactions) != 1 || rest.reactions[0].Emoji != "🎉" {
		t.Errorf("expected one 🎉 reaction; got %+v", rest.reactions)
	}
	if rest.reactions[0].MessageID != "user-msg" {
		t.Errorf("target = %q, want user-msg (fallback)", rest.reactions[0].MessageID)
	}
}

// TestOnPromptEnded_ReactionsOnResultMessage verifies that when
// sendResult recorded a result message id, OnPromptEnded stamps
// the result message id (not the user message id).
func TestOnPromptEnded_ReactionsOnResultMessage(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"
	a.rememberResultMessageID("dc_chan1", "user-msg", "result-msg-1")
	a.OnPromptEnded(context.Background(), "dc_chan1", "user-msg", 0)
	if len(rest.reactions) != 1 || rest.reactions[0].Emoji != "🎉" {
		t.Fatalf("expected one 🎉 reaction; got %+v", rest.reactions)
	}
	if rest.reactions[0].MessageID != "result-msg-1" {
		t.Errorf("target = %q, want result-msg-1 (recorded by sendResult)", rest.reactions[0].MessageID)
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
