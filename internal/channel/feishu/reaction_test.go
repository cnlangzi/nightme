package feishu

import (
	"context"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/cnlangzi/nightme/internal/gateway"
)

func TestHandleActionCreated_TranslatesToInboundMessage(t *testing.T) {
	a := testAdapter(t)

	const (
		wantChatID    = "oc_chat_abc"
		wantMessageID = "om_msg_xyz"
		wantUserID    = "ou_user_123"
		wantEmoji     = "DONE"
	)

	openID := wantUserID
	userID := "different_user_id"
	bodyJSON := []byte(`{
		"event": {
			"message_id": "` + wantMessageID + `",
			"reaction_type": {"emoji_type": "` + wantEmoji + `"},
			"user_id": {"open_id": "` + wantUserID + `", "user_id": "` + userID + `"}
		},
		"chat_id": "` + wantChatID + `"
	}`)

	ev := &larkim.P2MessageReactionCreatedV1{
		EventReq: &larkevent.EventReq{Body: bodyJSON},
		Event: &larkim.P2MessageReactionCreatedV1Data{
			MessageId:    stringPtr(wantMessageID),
			ReactionType: &larkim.Emoji{EmojiType: stringPtr(wantEmoji)},
			UserId:       &larkim.UserId{OpenId: &openID, UserId: &userID},
		},
	}
	if err := a.handleReactionCreated(context.Background(), ev); err != nil {
		t.Fatalf("handleReactionCreated: %v", err)
	}

	var got gateway.InboundMessage
	select {
	case got = <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reaction event on incoming channel")
	}

	if got.ChatID != wantChatID {
		t.Errorf("ChatID = %q, want %q", got.ChatID, wantChatID)
	}
	if !got.HasMention {
		t.Error("HasMention should be true on reactions")
	}
	if got.Text != "" {
		t.Errorf("Text = %q, want empty", got.Text)
	}
	if got.Reaction == nil {
		t.Fatal("Reaction is nil, want non-nil")
	}
	if got.Reaction.TargetMsgID != wantMessageID {
		t.Errorf("Reaction.TargetMsgID = %q, want %q", got.Reaction.TargetMsgID, wantMessageID)
	}
	if got.Reaction.Emoji != wantEmoji {
		t.Errorf("Reaction.Emoji = %q, want %q", got.Reaction.Emoji, wantEmoji)
	}
	if got.Reaction.UserID != wantUserID {
		t.Errorf("Reaction.UserID = %q, want %q (OpenId preferred over UserId)", got.Reaction.UserID, wantUserID)
	}
	if got.Reaction.ChatID != wantChatID {
		t.Errorf("Reaction.ChatID = %q, want %q", got.Reaction.ChatID, wantChatID)
	}
}

func TestHandleActionCreated_MissingFieldsDropsQuietly(t *testing.T) {
	a := testAdapter(t)
	ev := &larkim.P2MessageReactionCreatedV1{
		EventReq: &larkevent.EventReq{Body: []byte(`{"event":{}}`)},
		Event:    &larkim.P2MessageReactionCreatedV1Data{},
	}
	if err := a.handleReactionCreated(context.Background(), ev); err != nil {
		t.Fatalf("handleReactionCreated: %v", err)
	}
	select {
	case got := <-a.Incoming():
		t.Errorf("incoming should be empty, got %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHandleActionCreated_NilEvent(t *testing.T) {
	a := testAdapter(t)
	if err := a.handleReactionCreated(context.Background(), nil); err != nil {
		t.Errorf("nil event should be no-op, got error: %v", err)
	}
	if err := a.handleReactionCreated(context.Background(), &larkim.P2MessageReactionCreatedV1{}); err != nil {
		t.Errorf("empty event should be no-op, got error: %v", err)
	}
}

func stringPtr(s string) *string { return &s }