// Tests for §18 StatusBar per-message trailer integration.
// Every text-emitting OutboundKind (OutReply / OutResult /
// OutThinking / OutToolStart / OutToolEnd / OutTaskCreate /
// OutTaskUpdate / OutError / OutCommandReply) must carry the
// StatusBar snapshot appended to its rendered body. The
// placeholder PATCHed by OutHeartbeat also carries the trailer
// so identity / usage / git stay visible alongside the status
// line. OutChoice / OutMessageState / OutMessageStateRemoved
// are deliberately excluded (Choice is its own InlineKeyboard
// card; reactions don't touch text).
package telegram

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// sendMessageText returns the text of the first sendMessage
// call recorded by the fake API, or "" when none was recorded.
// Used by per-Kind tests that want to assert the rendered body.
func sendMessageText(calls []fakeCall) string {
	for _, call := range calls {
		if call.Method != "sendMessage" {
			continue
		}
		if text, ok := call.Params["text"].(string); ok {
			return text
		}
	}
	return ""
}

func snapshotFromAdapter(t *testing.T, a *Adapter) []fakeCall {
	t.Helper()
	if fake, ok := a.api.(*fakeAPI); ok {
		return fake.snapshotCalls()
	}
	return nil
}

// editMessageTextOrSendMessage returns the most recent
// editMessageText body, falling back to sendMessageText.
func editMessageTextOrSendMessage(calls []fakeCall) string {
	if text, ok := editMessageText(calls); ok {
		return text
	}
	return sendMessageText(calls)
}

// editMessageText returns the text of the most recent
// editMessageText call (placeholders are repeatedly PATCHed, so
// "most recent" is the right slice).
func editMessageText(calls []fakeCall) (string, bool) {
	for index := len(calls) - 1; index >= 0; index-- {
		if calls[index].Method == "editMessageText" {
			if text, ok := calls[index].Params["text"].(string); ok {
				return text, true
			}
		}
	}
	return "", false
}

// richOut returns an OutboundMessage with non-zero statusbar
// fields so StatusBarLines renders all three lines.
func richOut(kind messages.OutboundKind, text string) messages.OutboundMessage {
	return messages.OutboundMessage{
		ChatID:    "100",
		Kind:      kind,
		Text:      text,
		AgentName: "claude",
		Model:     "opus-4-5",
		SessionID: "sess-1",
		Usage: &agent.UsageInfo{
			InputTokens: 12_300, OutputTokens: 1_500, CostUSD: 0.087,
		},
		GitStatus: &messages.GitStatus{
			Workspace: "code/nightme",
			Snapshot:  &messages.GitStatusSnapshot{Branch: "main", AheadOfRemote: 2},
		},
	}
}

func assertSendMessageParseMode(calls []fakeCall, want string) bool {
	for _, c := range calls {
		if c.Method != "sendMessage" {
			continue
		}
		got, ok := c.Params["parse_mode"].(string)
		return ok && got == want
	}
	return false
}

func TestAdapter_Send_DM_OutChoice_NoStatusBar(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	// Seed the cache so we can detect if Choice mistakenly
	// appends it.
	if err := a.Send(context.Background(), richOut(messages.OutReply, "seed")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	api.Calls = nil

	// OutChoice should NOT carry the StatusBar trailer — Choice
	// is its own self-contained UI card with InlineKeyboard
	// buttons; tacking a footer on would clutter the choice
	// surface.
	choice := messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-1",
			Title:     "Allow?",
			Body:      "Allow agent to read /etc/passwd",
			Options: []messages.ChoiceOption{
				{ID: "allow", Label: "Allow once"},
				{ID: "reject", Label: "Reject"},
			},
		},
	}
	if err := a.Send(context.Background(), choice); err != nil {
		t.Fatalf("send choice: %v", err)
	}
	for _, call := range api.snapshotCalls() {
		if call.Method != "sendMessage" {
			continue
		}
		text, _ := call.Params["text"].(string)
		if strings.Contains(text, "🤖:") || strings.Contains(text, "💰:") || strings.Contains(text, "📁:") {
			t.Errorf("OutChoice must not carry StatusBar; got text=%q", text)
		}
	}
}

func TestAdapter_Send_DM_OutMessageState_NoTextChange(t *testing.T) {
	a, api := newTestAdapter(t)

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "10",
			State:     agent.MessageSubmitted,
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// OutMessageState → setMessageReaction only. No
	// editMessageText, no sendMessage.
	for _, call := range api.snapshotCalls() {
		if call.Method == "sendMessage" || call.Method == "editMessageText" {
			t.Errorf("OutMessageState must not write text; got method=%s params=%+v", call.Method, call.Params)
		}
	}
}

// debug_flush prints the full API calls log — drop in to lastChunkText
// helper when a test fails to see what's actually being recorded.
func debug_flush(t *testing.T, a *Adapter, api *fakeAPI) {
	t.Helper()
	for i, call := range api.snapshotCalls() {
		text := ""
		if s, ok := call.Params["text"].(string); ok {
			text = s
		}
		t.Logf("call %d: method=%s text=%q", i, call.Method, text)
	}
}
