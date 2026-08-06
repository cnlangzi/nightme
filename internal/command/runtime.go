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
// Commands must NEVER reach for *chatsession.Manager / *gtw /
// *gateway concrete types via this struct — only the interfaces
// below. RuntimeServices does NOT contain gateway.Channel or
// gateway.OutboundMessage; it carries command.Channel (this
// package) instead. See F-51 doc §1.2.7 for the translation
// convention.
type RuntimeServices struct {
	// Session is the per-chat state surface (activeCwd,
	// activeAgent, primaryAgent, AgentSession pool, per-chat
	// toggle modes). Implemented by *chatsession.Manager via
	// the sessionAdapter in cmd/nightme/session_adapter.go.
	Session services.SessionService
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
