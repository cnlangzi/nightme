package chatsession

// ReactionEvent is the chatsession-package view of a user-emoji
// reaction on a previously-sent message. Defined here (not in
// internal/gateway) so that ChatSession's onReaction callback can
// reference it without creating a gateway → chatsession → gateway
// cycle. The gateway package re-exports the same type via a
// type alias (gateway.ReactionEvent = chatsession.ReactionEvent).
//
// F-45 §3.2.
type ReactionEvent struct {
	// TargetMsgID is the channel-native message id of the bot's
	// message that the user reacted to. Used as the lookup key
	// into ChatSession.gtwDrafts.
	TargetMsgID string
	// Emoji is the raw reaction emoji the user picked.
	Emoji string
	// UserID is the channel-native user id of the reactor.
	UserID string
	// ChatID is the chat in which the reaction was made. Set
	// by the channel adapter so the reaction handler can render
	// a follow-up reply without re-discovering the chat.
	ChatID string
}