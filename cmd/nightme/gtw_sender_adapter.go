// Package main (cmd/nightme) — chatSessionSender wires the
// per-chat `gtw.Sender` (used by gtw.RunFix and
// gtw.HandleDraftReaction) from the runtime's SessionService
// + Channel adapters.
//
// Lives in cmd/nightme for the same reason as
// session_adapter.go / channel_adapter.go: the Sender
// composition depends on both chatsession (concrete) and
// command (interface) — neither can hold it without breaking
// the F-51 dependency arrow.
//
// Constructed lazily by the gtw.Manager's senderFactory
// (installed in run.go). One Sender instance per chatID;
// cached by the Manager on first GetSender.
package main

import (
	"context"

	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/command/services"
)

// chatSessionSender implements gtw.Sender by delegating to:
//   - Session.ActiveCwd / SetActiveCwd (per-chat workspace
//     state) — read / write through the SessionService adapter.
//   - Channel.Send (per-message outbound) — routed through
//     the Channel adapter, which translates command.Outbound
//     → gateway.OutboundMessage.
//
// One instance per chatID. Safe for concurrent use: the
// SessionService adapter is concurrent-safe (chatsession.Manager
// is RWMutex-protected) and the Channel adapter is stateless.
type chatSessionSender struct {
	chatID  string
	session services.Session
	channel command.Channel
}

// newChatSessionSender constructs a Sender for the given chat.
// session is the SessionService-adapter view of the chat (not
// nil — the runtime must ensure the chat session exists before
// the factory is called); channel is the shared outbound
// channel (the runtime has one per process).
func newChatSessionSender(chatID string, sess services.Session, ch command.Channel) gtw.Sender {
	if sess == nil {
		return nil
	}
	return &chatSessionSender{
		chatID:  chatID,
		session: sess,
		channel: ch,
	}
}

// ActiveCwd returns the chat's current workspace. Read-only;
// gtw uses this to derive the worktree root.
func (s *chatSessionSender) ActiveCwd() string { return s.session.ActiveCwd() }

// SetActiveCwd updates the chat's active workspace. gtw uses
// this on /gtw fix success to switch the chat to the newly-
// created worktree.
func (s *chatSessionSender) SetActiveCwd(cwd string) error {
	return s.session.SetActiveCwd(cwd)
}

// Send posts an outbound gtw message via the channel. The
// gtw.OutMsg is translated to command.Outbound (no Card in
// this path — gtw uses Send for text replies / PATCHes; card
// sends go through the gtw.SendCardFunc in HandlerDeps, not
// through Sender).
func (s *chatSessionSender) Send(ctx context.Context, m gtw.OutMsg) error {
	if s.channel == nil {
		return nil
	}
	_, err := s.channel.Send(ctx, command.Outbound{
		ChatID:  m.ChatID,
		Text:    m.Text,
		ReplyTo: m.ReplyTo,
	})
	return err
}

// Compile-time check: chatSessionSender satisfies gtw.Sender.
var _ gtw.Sender = (*chatSessionSender)(nil)
