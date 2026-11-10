package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/messages"
)

// chatSender posts a text into a chat on any channel.
//
// It is the daemon-wide counterpart to channel.Channel.Send: the
// sender does not know which channel owns the chat, so the chat is
// resolved through the per-channel Managers the runtime owns.
type chatSender struct{}

// NewChatSender returns a sender that posts into any chat on any
// channel. The sender resolves the channel at send time, so it is
// safe to construct before the channels have started.
func NewChatSender() messages.ChatSender {
	return &chatSender{}
}

// Send posts text into the chat with the given id.
//
// The chat is resolved through the runtime's per-channel Managers:
// chat ids are channel-namespaced, so exactly one Manager owns a
// given id. A chat that never sent a message in this daemon's
// lifetime has no session and is reported as unknown.
func (s *chatSender) Send(ctx context.Context, chatID, text string) error {
	if strings.TrimSpace(chatID) == "" {
		return errors.New("runtime: ChatSender: empty chat id")
	}
	cs := findChatSession(chatID)
	if cs == nil {
		return fmt.Errorf("runtime: ChatSender: no chat session for %q", chatID)
	}
	em := cs.Emitter()
	if em == nil {
		return fmt.Errorf("runtime: ChatSender: chat %q has no emitter", chatID)
	}
	return em.Send(ctx, messages.OutboundMessage{
		ChatID: chatID,
		Kind:   messages.OutCommandReply,
		Text:   text,
	})
}
