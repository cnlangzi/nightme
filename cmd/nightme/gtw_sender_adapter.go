// Package main (cmd/nightme) — chatSessionSender wires the
// per-chat `gtw.Sender` (used by gtw.RunFix and
// gtw.HandleDraftReaction) from the runtime's *chatsession.Manager
// + Channel adapter.
//
// ADR 0007 (2026-08-06): the previous SessionService indirection
// was removed; chatSessionSender now takes a *chatsession.ChatSession
// directly. The Sender surface is unchanged (gtw only reads
// ActiveCwd / writes SetActiveCwd / sends messages) so gtw itself
// is not affected.
//
// Constructed lazily by the gtw.Manager's senderFactory
// (installed in run.go). One Sender instance per chatID;
// cached by the Manager on first GetSender.
package main

import (
	"context"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/command/gtw"
)

// chatSessionSender implements gtw.Sender by delegating to:
//   - ChatSession.ActiveCwd / SetActiveCwd (per-chat workspace
//     state) — direct read / write on the chatsession concrete.
//   - Channel.Send (per-message outbound) — routed through
//     the Channel adapter, which translates command.Outbound
//     → gateway.OutboundMessage.
//
// One instance per chatID. Safe for concurrent use: the
// chatsession.Manager is RWMutex-protected and the Channel
// adapter is stateless.
type chatSessionSender struct {
	chatID  string
	cs      *chatsession.ChatSession
	channel command.Channel
}

// newChatSessionSender constructs a Sender for the given chat.
// cs is the chatsession view of the chat (not nil — the runtime
// must ensure the chat session exists before the factory is
// called); channel is the shared outbound channel (the runtime
// has one per process).
func newChatSessionSender(chatID string, cs *chatsession.ChatSession, ch command.Channel) gtw.Sender {
	if cs == nil {
		return nil
	}
	return &chatSessionSender{
		chatID:  chatID,
		cs:      cs,
		channel: ch,
	}
}

// ActiveCwd returns the chat's current workspace. Read-only;
// gtw uses this to derive the worktree root.
func (s *chatSessionSender) ActiveCwd() string { return s.cs.ActiveCwd() }

// SetActiveCwd updates the chat's active workspace. gtw uses
// this on /gtw fix success to switch the chat to the newly-
// created worktree.
func (s *chatSessionSender) SetActiveCwd(cwd string) error {
	return s.cs.SetActiveCwd(cwd)
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