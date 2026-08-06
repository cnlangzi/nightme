// Package command hosts the F-51 slash command abstraction layer.
// It provides the Commander / SlashCommandFactory / RuntimeServices
// interfaces, the canonical inbound/outbound types
// (SlashInput / SlashOutput / Outbound / Card / CardChoice /
// ReactionEvent), and the ReactionRouter service interface that
// command implementations depend on.
//
// This package is the bottom of the command stack — it does NOT
// import internal/gateway, internal/chatsession, internal/gtw, or
// internal/channel. The runtime (cmd/nightme/) is the only place
// that bridges the gap. See docs/feat/F-51-slash-command-service-
// separation.md §1.2.7 for the translation convention.
//
// ADR 0007 (2026-08-06): command packages may import
// internal/chatsession directly. The previous SessionService
// indirection was removed; this package no longer declares any
// chat-session interface (services/session.go was deleted).
package command

import "github.com/cnlangzi/nightme/internal/command/services"

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
// gateway.WithCommander receives *gateway.InboundMessage and
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
	// group chats when /watch is off).
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
// result. gateway translates back to *gateway.CommandResult.
type SlashOutput struct {
	// Reply is the human-readable reply text. When Outbound is
	// empty, gateway emits this as a single OutReply.
	Reply string
	// Consumed=true means the message was handled; gateway will
	// NOT forward to the agent loop. false → fall through.
	Consumed bool
	// Dropped=true means gateway should silently drop the
	// message (e.g. /watch off + not @-mentioned). Distinct
	// from Consumed for log clarity.
	Dropped bool
	// Outbound is an explicit list of outbound messages to send
	// in order. When non-empty, gateway uses these instead of
	// building one from Reply. Allows commands to send cards +
	// replies atomically (e.g. /gtw test seed-card → reaction
	// → PATCH).
	Outbound []Outbound
}

// Outbound is the command-package's view of one outbound message.
// Mirrors a subset of gateway.OutboundMessage that commands need.
type Outbound struct {
	// ChatID is the destination chat id.
	ChatID string
	// Text is the message body (markdown; for card messages,
	// the body becomes the card's main content).
	Text string
	// ReplyTo is the channel-native userMsgID to thread under.
	// Empty for top-level messages.
	ReplyTo string
	// Card is non-nil for interactive card messages. When set,
	// Channel.SendCard is called instead of Channel.Send.
	Card *Card
}

// Card is the command-package's view of one interactive card.
// gtw.Card translates to this at the action boundary.
type Card struct {
	// Kind is the card variant: "decision" (interactive button
	// card) | "info" (read-only). Matches gateway.CardKind*.
	Kind string
	// Title is the card header.
	Title string
	// Body is the card's main markdown content.
	Body string
	// Choices is the list of buttons (decision cards only).
	Choices []CardChoice
	// RequestID is the per-card idempotency key. Channel
	// adapters use it for de-dup on PATCH.
	RequestID string
	// Disabled=true means all buttons are non-interactive
	// (used when PATCHing after a user has clicked).
	Disabled bool
	// ChosenEmoji marks one button as "✅ 已选" when PATCHing.
	// Empty when no specific choice should be highlighted.
	ChosenEmoji string
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
