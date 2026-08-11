// Package inbound — tryMessageDispatch: the universal fallback.
// Always claims (handled=true) so the chain always terminates
// with a non-nil result.
//
// The "default branch" of the inboundDispatcher: plain text,
// attachments, unrecognised "/foo" — anything not consumed
// by action / command / shell. Hands the message to
// chatsession.Manager.HandleInbound (PR3b), which owns the
// WatchMode gate + per-chat GetOrCreate + AgentSession
// resolution + InputBuffer queue. The full message-handling
// pipeline lives next to its state; the dispatcher just
// points at it.
package inbound

import (
	"context"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/messages"
)

// tryMessageDispatch is the universal fallback. Always
// claims (handled=true) so the chain always terminates with a
// non-nil result, but returns Consumed=false (the v0.x
// dispatchMessage contract) — the message handler is a "fire
// and forget" for the agent loop; it doesn't claim the input
// the way a slash command reply does.
func (r *Router) tryMessageDispatch(ctx context.Context, msg *messages.InboundMessage) (bool, *CommandResult, error) {
	if err := r.csMgr.HandleInbound(ctx, msg); err != nil {
		slog.Default().Warn("inbound: HandleInbound failed",
			"chat_id", msg.ChatID, "err", err)
		return true, &CommandResult{}, err
	}
	return true, &CommandResult{}, nil
}
