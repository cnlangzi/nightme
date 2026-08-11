package shell

import (
	"context"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// ChatSessionSender implements shell.Sender on top of a
// chatsession.Manager's shared outbound.Emitter. The dispatcher
// looks up the chat's Emitter at Send time, so a single
// sender routes to any chat the manager knows about.
//
// Lives in the shell package (not in main) because the
// adapter is shell-specific: the package's whole contract is
// that a Sender routes an Outbound somewhere, and this is
// the chatsession-backed implementation of that contract.
//
// Send is best-effort: if the chat session can't be resolved
// (e.g. unloaded during shutdown) or the channel refuses, we
// silently drop. The shell dispatcher's Handle is the
// fire-and-forget reply path (the result card), not a critical
// control message.
type ChatSessionSender struct {
	mgr *chatsession.Manager
}

// NewChatSessionSender returns a Sender that routes shell
// replies through the given chatsession.Manager's per-chat
// outbound.Emitter. The shell dispatcher only needs Send
// (no SendCard) so this is a thin one-method shim.
func NewChatSessionSender(mgr *chatsession.Manager) *ChatSessionSender {
	return &ChatSessionSender{mgr: mgr}
}

// Send looks up the ChatSession for the requested chatID and
// posts the reply through its Emitter. nil-safe everywhere: a
// missing chat session, missing emitter, or missing reply
// target all silently no-op (matches the pre-extraction wrap's
// behaviour).
func (s *ChatSessionSender) Send(ctx context.Context, msg Outbound) error {
	if msg.ChatID == "" {
		return nil
	}
	if s == nil || s.mgr == nil {
		return nil
	}
	cs := s.mgr.Get(msg.ChatID)
	if cs == nil {
		return nil
	}
	em := cs.Emitter()
	if em == nil {
		return nil
	}
	return em.Send(ctx, messages.OutboundMessage{
		ChatID:  msg.ChatID,
		Kind:    messages.OutCommandReply,
		Text:    msg.Text,
		ReplyTo: msg.ReplyTo,
	})
}
