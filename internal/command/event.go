// Package command hosts the F-51 slash command abstraction layer.
// It provides the Commander / SlashCommandFactory / RuntimeServices
// interfaces, the canonical inbound/outbound types
// (SlashInput / SlashOutput / Outbound / Card / CardChoice /
// ReactionEvent), and the ReactionRouter service interface that
// command implementations depend on.
//
// This package is the bottom of the command stack — it does NOT
// import internal/gateway, internal/chatsession, internal/gtw, or
// internal/channel. The runtime (cmd/nightme/) owns the boundary
// translation between gateway messages and command inputs/outputs.
//
// The Commander refactor proposed deleting command.Outbound /
// Card / CardChoice in favour of using messages.OutboundMessage
// directly. That would require command to import gateway, which
// creates an import cycle (gateway → command/gtw → command). So
// we keep the command-side mirror types; the runtime shim
// translates at the boundary as before.
package command

import (
	"github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/messages"
)

// ReactionEvent is the inbound reaction / action payload.
//
// Canonical location: services.ReactionEvent (services/reaction.go).
// This alias lets callers in the command package use
// `command.ReactionEvent` without importing the services
// subpackage twice. The underlying type lives in services
// because ReactionRouter (which lives in services) takes /
// returns ReactionEvent in its signatures — placing the type
// in services avoids a `command <-> services` import cycle
// (command.RuntimeServices already depends on services for
// ReactionRouter).
type ReactionEvent = services.ReactionEvent

// SlashInput is the command-package's view of one inbound message.
//
// gateway.WithCommander receives *messages.InboundMessage and
// translates to this struct before calling Commander.Dispatch.
// Likewise channel adapters (e.g. feishu/adapter.go) construct
// SlashInput.Reaction from the channel's native reaction event.
type SlashInput struct {
	// ChatID is the IM-side chat id (D1 model; see SPEC §3.1).
	ChatID string
	// UserID is the sender's IM-side id.
	UserID string
	// Text is the full message text, including any "/cmd args..."
	// prefix. gateway's parser may have already pre-parsed this
	// into Args, but the raw text is preserved for commands that
	// want to re-parse (e.g. /gtw with subcommands).
	Text string
	// MessageID is the channel-native message id; used as
	// ReplyTo for outbound threading.
	MessageID string
	// HasMention indicates whether the bot was @-mentioned.
	// Used by the WatchMode gate (silent-drop non-mentions in
	// group chats when /watch off).
	HasMention bool
	// Reaction is non-nil for reaction / action events.
	// Slash commands ignore this; ReactionRouter consumers use it.
	Reaction *services.ReactionEvent
	// Args is the pre-parsed argv (gateway's parser fills this).
	// Element 0 is the command name; elements 1+ are the args.
	// Empty for reaction events.
	Args []string
}

// SlashOutput is the command-package's view of one command's
// result. The runtime shim consumes Reply / Outbound and routes
// them through cs.Emitter().Send / SendCard (PATCH semantics
// fold into Send with Kind=OutCardPatch). Consumed + Dropped
// flow back to the gateway for legacy fall-through handling.
type SlashOutput struct {
	// Reply is the human-readable reply text. When Outbound is
	// empty, the runtime shim emits this as a single Send.
	Reply string
	// Consumed=true means the message was handled; gateway will
	// NOT forward to the agent loop. false → fall through.
	Consumed bool
	// Dropped=true means the runtime shim should silently drop
	// the message (e.g. /watch off + not @-mentioned). Distinct
	// from Consumed for log clarity.
	Dropped bool
	// Outbound is an explicit list of outbound messages to send
	// in order. When non-empty, the runtime shim forwards each
	// via the chat session's Emitter. Uses the canonical
	// messages.OutboundMessage so commands build messages with
	// the same type the Emitter accepts (no mirror types in
	// this package).
	Outbound []messages.OutboundMessage
}

// CardChoice is the command-package's view of one button on a
// decision card.
type CardChoice struct {
	// Emoji is the button's leading emoji ("✅" / "🆕" / ...).
	// Also the action's key — when the user clicks, the channel
	// emits a reaction with this emoji on the card's bot msg.
	Emoji string
	// Label is the button's text label.
	Label string
	// Action is the action tag ("act:/xxx" form). Channel
	// adapters route it to the action dispatcher.
	Action string
}