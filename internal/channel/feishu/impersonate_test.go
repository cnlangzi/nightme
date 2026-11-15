package feishu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestSpeak_PostsTheMessageAndHandsItBack — what the run says is a
// real message of the chat: it is posted on the platform and comes
// back into the daemon with the platform's own id.
func TestSpeak_PostsTheMessageAndHandsItBack(t *testing.T) {
	a := testAdapter(t)

	var posted struct {
		chatID  string
		msgType string
		content string
		rootID  string
	}
	a.sendFunc = func(_ context.Context, chatID, msgType, content, rootID string, _ bool) (string, error) {
		posted.chatID = chatID
		posted.msgType = msgType
		posted.content = content
		posted.rootID = rootID
		return "om_echo", nil
	}

	msgID, err := a.Speak(context.Background(), "oc_test", "/gtw commit", channel.SpeakOptions{
		Display: "🤖 /gtw commit",
		Owner:   "ou_owner",
		RunID:   "run-1",
	})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if msgID != "om_echo" {
		t.Fatalf("msgID = %q, want om_echo", msgID)
	}
	if posted.chatID != "oc_test" {
		t.Errorf("the message went to %q, want oc_test", posted.chatID)
	}
	if posted.msgType != larkim.MsgTypeText {
		t.Errorf("msgType = %q, want %q", posted.msgType, larkim.MsgTypeText)
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(posted.content), &payload); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if payload.Text != "🤖 /gtw commit" {
		t.Errorf("the chat shows %q, want the display text", payload.Text)
	}

	inbound := <-a.Incoming()
	if inbound.MessageID != "om_echo" {
		t.Errorf("MessageID = %q, want om_echo", inbound.MessageID)
	}
	if inbound.Text != "/gtw commit" {
		t.Errorf("Text = %q, want the dispatched text", inbound.Text)
	}
	if inbound.Origin != messages.OriginMe {
		t.Errorf("Origin = %q, want %q", inbound.Origin, messages.OriginMe)
	}
	if inbound.RunID != "run-1" {
		t.Errorf("RunID = %q, want run-1", inbound.RunID)
	}
	if inbound.UserID != "ou_owner" {
		t.Errorf("UserID = %q, want ou_owner", inbound.UserID)
	}
	if !inbound.HasMention {
		t.Error("the message does not address the bot")
	}
	if inbound.ChatID != "oc_test" {
		t.Errorf("ChatID = %q, want oc_test", inbound.ChatID)
	}
}

// TestSpeak_QuotesTheMessageItAnswers — a correction quotes the
// message it corrects.
func TestSpeak_QuotesTheMessageItAnswers(t *testing.T) {
	a := testAdapter(t)

	var rootID string
	a.sendFunc = func(_ context.Context, _, _, _, id string, _ bool) (string, error) {
		rootID = id
		return "om_echo", nil
	}

	if _, err := a.Speak(context.Background(), "oc_test", "/gtw commit --amend", channel.SpeakOptions{
		ReplyTo: "om_prev",
	}); err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if rootID != "om_prev" {
		t.Errorf("the message quoted %q, want om_prev", rootID)
	}
	inbound := <-a.Incoming()
	if inbound.ReplyTo != "om_prev" {
		t.Errorf("ReplyTo = %q, want om_prev", inbound.ReplyTo)
	}
}

// TestAsk_PostsTheQuestionWithoutDispatchingIt — a question belongs
// to the program that asked it: the daemon does not take it as a
// turn of its own.
func TestAsk_PostsTheQuestionWithoutDispatchingIt(t *testing.T) {
	a := testAdapter(t)

	var posted string
	a.sendFunc = func(_ context.Context, _, _, content, _ string, _ bool) (string, error) {
		posted = content
		return "om_question", nil
	}

	id, err := a.Ask(context.Background(), "oc_test", "Which branch?", channel.SpeakOptions{
		Display: "🤖 ❓ Which branch?",
		ReplyTo: "om_card",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if id != "om_question" {
		t.Fatalf("id = %q, want om_question", id)
	}
	if posted == "" {
		t.Fatal("the question was not posted")
	}
	select {
	case msg := <-a.Incoming():
		t.Fatalf("the question was dispatched to the daemon: %+v", msg)
	default:
	}
}

// TestPostCard_RendersOneLinePerElement — Feishu's markdown element
// treats a newline as a soft break: a body with newlines in a single
// element runs together on the card.
func TestPostCard_RendersOneLinePerElement(t *testing.T) {
	a := testAdapter(t)

	var sent string
	a.sendFunc = func(_ context.Context, _, _, content, _ string, _ bool) (string, error) {
		sent = content
		return "om_card", nil
	}

	spec := channel.CardSpec{
		Title: "fix · running",
		Body:  "1. ✅ /gtw commit\n2. 🔄 /gtw push\nerror: the agent is rate limited",
	}
	if _, err := a.PostCard(context.Background(), "oc_test", spec); err != nil {
		t.Fatalf("PostCard: %v", err)
	}

	var card map[string]any
	if err := json.Unmarshal([]byte(sent), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	body, _ := card["body"].(map[string]any)
	elements, _ := body["elements"].([]any)
	lines := 0
	for _, el := range elements {
		m, _ := el.(map[string]any)
		if m["tag"] == "markdown" {
			lines++
		}
	}
	if lines != 3 {
		t.Fatalf("the card has %d markdown elements, want one per line", lines)
	}
}

// TestAct_ClicksTheCardAndShowsTheChoice — the run answers a prompt
// the way a person does: the card shows the choice and the click
// reaches the daemon.
func TestAct_ClicksTheCardAndShowsTheChoice(t *testing.T) {
	a := testAdapter(t)
	a.updateFunc = func(context.Context, string, string) error { return nil }

	choice := &messages.Choice{
		RequestID: "perm:oc_test:1",
		Kind:      messages.ChoiceKindPermission,
		Title:     "Waiting for approval",
		Body:      "Bash: git push",
		Options: []messages.ChoiceOption{
			{ID: "yes", Label: "yes"},
			{ID: "no", Label: "no"},
		},
	}
	a.rememberOptCard("oc_test", "om_card", choice)

	if err := a.Act(context.Background(), "oc_test", messages.ActionPayload{
		RequestID: "perm:oc_test:1",
		Option:    "yes",
	}); err != nil {
		t.Fatalf("Act: %v", err)
	}

	inbound := <-a.Incoming()
	if inbound.Action == nil {
		t.Fatal("the click did not reach the daemon")
	}
	if inbound.Action.RequestID != "perm:oc_test:1" {
		t.Errorf("RequestID = %q, want the card's request id", inbound.Action.RequestID)
	}
	if inbound.Action.Option != "yes" {
		t.Errorf("Option = %q, want yes", inbound.Action.Option)
	}
	if inbound.ChatID != "oc_test" {
		t.Errorf("ChatID = %q, want oc_test", inbound.ChatID)
	}
}

// TestAct_WithoutACardIsRefused — a click on a card nobody waits for
// is a mistake, not a silent no-op.
func TestAct_WithoutACardIsRefused(t *testing.T) {
	a := testAdapter(t)
	err := a.Act(context.Background(), "oc_test", messages.ActionPayload{
		RequestID: "perm:oc_test:9",
		Option:    "yes",
	})
	if err == nil {
		t.Fatal("the click was accepted although no card waits for it")
	}
}

// TestPostCard_PublishesTheCardAndUpdatesIt — the run's overview is
// one message of the chat, edited in place.
func TestPostCard_PublishesTheCardAndUpdatesIt(t *testing.T) {
	a := testAdapter(t)

	var sent, patched string
	a.sendFunc = func(_ context.Context, _, _, content, _ string, _ bool) (string, error) {
		sent = content
		return "om_card", nil
	}
	a.updateFunc = func(_ context.Context, _, content string) error {
		patched = content
		return nil
	}

	spec := channel.CardSpec{
		Title: "fix · running",
		Body:  "1. ✅ /gtw commit",
		Buttons: []channel.CardButton{
			{Label: "暂停", Action: "pause"},
		},
	}
	id, err := a.PostCard(context.Background(), "oc_test", spec)
	if err != nil {
		t.Fatalf("PostCard: %v", err)
	}
	if id != "om_card" {
		t.Fatalf("id = %q, want om_card", id)
	}
	if sent == "" {
		t.Fatal("the card was not published")
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(sent), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card["schema"] != "2.0" {
		t.Errorf("schema = %v, want 2.0", card["schema"])
	}

	spec.Body = "1. ✅ /gtw commit\n2. 🔄 /gtw push"
	if err := a.UpdateCard(context.Background(), "oc_test", id, spec); err != nil {
		t.Fatalf("UpdateCard: %v", err)
	}
	if patched == "" {
		t.Fatal("the card was not updated")
	}
	if !strings.Contains(patched, "gtw push") {
		t.Errorf("the updated card does not carry the new body: %s", patched)
	}
}
