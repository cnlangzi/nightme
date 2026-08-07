package command

import (
	"context"

	"github.com/cnlangzi/nightme/internal/command/services"
)

// RuntimeServices aggregates the dependencies a slash command
// receives at Handle() time. The runtime (cmd/nightme/run.go)
// builds this once at startup; the Commander passes it to every
// dispatched Handle() call.
//
// Commands that need per-chat state hold *chatsession.Manager
// directly in their Factory. The remaining fields are shared
// interfaces with multiple implementations or cross-cutting concerns.
type RuntimeServices struct {
	// ReactionRouter dispatches reaction events to the right
	// handler. Implemented by services.reactionRouter —
	// held as a singleton by the runtime.
	ReactionRouter services.ReactionRouter
	// Channel is the outbound channel surface. *gateway.Channel
	// satisfies this interface (compile-time asserted in
	// cmd/nightme/run.go via
	// `var _ command.Channel = (*gateway.Channel)(nil)`).
	Channel Channel
	// Reserved: future Logger / Metrics / Config once they
	// become part of the command contract.
}

// Channel is the command-package's view of an outbound channel.
// *gateway.Channel satisfies this interface; channel adapters
// that satisfy it (via wrapper if needed) can be wired in.
//
// The interface intentionally mirrors only the surface commands
// need — Send and SendCard. Inbound (Receive) is NOT part of
// this interface because the runtime owns the inbound pump;
// commands only push back.
type Channel interface {
	// Send posts a text message (or a card, if m.Card is non-nil
	// — the adapter routes to SendCard internally) and returns
	// the bot-side message id assigned by the channel. Empty
	// string + nil error on transient errors the adapter
	// returns without an id.
	Send(ctx context.Context, m Outbound) (msgID string, err error)
	// SendCard posts an interactive card and returns the
	// created message id. m.Card must be non-nil.
	SendCard(ctx context.Context, m Outbound) (msgID string, err error)
}
