package telegram

import (
	"context"
	"testing"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/messages"
)

// newImpersonateAdapter builds a started adapter backed by a fake
// API.
//
// The adapter is started because publish selects against the
// adapter's context: an adapter that never started drops the
// message.
func newImpersonateAdapter(t *testing.T) (*Adapter, *fakeAPI) {
	t.Helper()
	resetSendMessageCounter()
	api := &fakeAPI{GetMeResult: UserInfo{ID: 999, Username: "testbot"}}
	a := NewAdapterWithClient(nil, api, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, api
}

// TestSpeak_PostsTheMessageAndHandsItBack — what the run says is a
// real message of the chat: it is posted on the platform and comes
// back into the daemon with the platform's own id.
func TestSpeak_PostsTheMessageAndHandsItBack(t *testing.T) {
	a, api := newImpersonateAdapter(t)

	msgID, err := a.Speak(context.Background(), "tg_100", "/gtw commit", channel.SpeakOptions{
		Display: "🤖 /gtw commit",
		Owner:   "42",
		RunID:   "run-1",
	})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if msgID == "" {
		t.Fatal("the message got no id")
	}

	send := findCall(api.snapshotCalls(), "sendMessage")
	if send == nil {
		t.Fatal("the message was not posted")
	}
	if got, _ := send.Params["chat_id"].(string); got != "100" {
		t.Errorf("chat_id = %q, want 100", got)
	}
	if got, _ := send.Params["text"].(string); got != "🤖 /gtw commit" {
		t.Errorf("the chat shows %q, want the display text", got)
	}

	inbound := <-a.Incoming()
	if inbound.MessageID != msgID {
		t.Errorf("MessageID = %q, want %q", inbound.MessageID, msgID)
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
	if inbound.UserID != "42" {
		t.Errorf("UserID = %q, want 42", inbound.UserID)
	}
	if inbound.ChatID != "tg_100" {
		t.Errorf("ChatID = %q, want tg_100", inbound.ChatID)
	}
	if !inbound.HasMention {
		t.Error("the message does not address the bot")
	}
}

// TestSpeak_BuildsTheTopic — a message in a forum topic lands in that
// topic, the way a person's message does.
func TestSpeak_BuildsTheTopic(t *testing.T) {
	a, api := newImpersonateAdapter(t)

	if _, err := a.Speak(context.Background(), "tg_-100123:42", "/gtw commit", channel.SpeakOptions{
		Owner: "42",
	}); err != nil {
		t.Fatalf("Speak: %v", err)
	}
	send := findCall(api.snapshotCalls(), "sendMessage")
	if send == nil {
		t.Fatal("the message was not posted")
	}
	if got, _ := send.Params["message_thread_id"].(int); got != 42 {
		t.Errorf("message_thread_id = %v, want 42", send.Params["message_thread_id"])
	}
	inbound := <-a.Incoming()
	if inbound.ChatID != "tg_-100123:42" {
		t.Errorf("ChatID = %q, want tg_-100123:42", inbound.ChatID)
	}
}

// TestSpeak_IgnoresTheBotsOwnEcho — Telegram sends the bot's own
// messages back through getUpdates. The adapter drops them, so the
// message the run posted reaches the daemon once.
func TestSpeak_IgnoresTheBotsOwnEcho(t *testing.T) {
	a, _ := newImpersonateAdapter(t)

	if _, err := a.Speak(context.Background(), "tg_100", "/gtw commit", channel.SpeakOptions{
		Owner: "42",
	}); err != nil {
		t.Fatalf("Speak: %v", err)
	}
	first := <-a.Incoming()
	if first.Text != "/gtw commit" {
		t.Fatalf("Text = %q, want the dispatched text", first.Text)
	}

	// The platform hands the bot its own message back.
	a.mu.Lock()
	a.botID = 42
	a.mu.Unlock()
	a.handleMessage(context.Background(), &Message{
		MessageID: 7,
		Chat:      Chat{ID: 100, Type: "private"},
		From:      &User{ID: 42},
		Text:      "/gtw commit",
	})
	select {
	case second := <-a.Incoming():
		t.Fatalf("the bot's own echo came back in: %+v", second)
	default:
	}
}

// TestAsk_PostsTheQuestionWithoutDispatchingIt — a question belongs
// to the program that asked it: the daemon does not take it as a
// turn of its own.
func TestAsk_PostsTheQuestionWithoutDispatchingIt(t *testing.T) {
	a, api := newImpersonateAdapter(t)

	id, err := a.Ask(context.Background(), "tg_100", "Which branch?", channel.SpeakOptions{
		Display: "🤖 ❓ Which branch?",
		ReplyTo: "55",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if id == "" {
		t.Fatal("the question got no id")
	}
	send := findCall(api.snapshotCalls(), "sendMessage")
	if send == nil {
		t.Fatal("the question was not posted")
	}
	if got, _ := send.Params["reply_to_message_id"].(int); got != 55 {
		t.Errorf("reply_to_message_id = %v, want 55", send.Params["reply_to_message_id"])
	}
	select {
	case msg := <-a.Incoming():
		t.Fatalf("the question was dispatched to the daemon: %+v", msg)
	default:
	}
}

// TestAct_ClicksThePromptAndShowsTheChoice — the run answers a
// prompt the way a person does: the prompt shows the choice and the
// click reaches the daemon.
func TestAct_ClicksThePromptAndShowsTheChoice(t *testing.T) {
	a, _ := newImpersonateAdapter(t)

	choice := &messages.Choice{
		RequestID: "perm:tg_100:1",
		Kind:      messages.ChoiceKindPermission,
		Title:     "Waiting for approval",
		Body:      "Bash: git push",
		Options: []messages.ChoiceOption{
			{ID: "yes", Label: "yes"},
			{ID: "no", Label: "no"},
		},
	}
	state := &ChoiceState{
		RequestID: choice.RequestID,
		MessageID: 55,
		ChatID:    "tg_100",
		Choice:    choice,
	}
	if err := a.state.putChoice(state); err != nil {
		t.Fatalf("putChoice: %v", err)
	}

	if err := a.Act(context.Background(), "tg_100", messages.ActionPayload{
		RequestID: choice.RequestID,
		Option:    "yes",
	}); err != nil {
		t.Fatalf("Act: %v", err)
	}

	inbound := <-a.Incoming()
	if inbound.Action == nil {
		t.Fatal("the click did not reach the daemon")
	}
	if inbound.Action.RequestID != choice.RequestID {
		t.Errorf("RequestID = %q, want the prompt's request id", inbound.Action.RequestID)
	}
	if inbound.Action.Option != "yes" {
		t.Errorf("Option = %q, want yes", inbound.Action.Option)
	}
	if inbound.ChatID != "tg_100" {
		t.Errorf("ChatID = %q, want tg_100", inbound.ChatID)
	}
}

// TestAct_WithoutAPromptIsRefused — a click on a prompt nobody waits
// for is a mistake, not a silent no-op.
func TestAct_WithoutAPromptIsRefused(t *testing.T) {
	a, _ := newImpersonateAdapter(t)
	err := a.Act(context.Background(), "tg_100", messages.ActionPayload{
		RequestID: "perm:tg_100:9",
		Option:    "yes",
	})
	if err == nil {
		t.Fatal("the click was accepted although no prompt waits for it")
	}
}
