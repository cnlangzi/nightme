package feishu

import (
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// stringValue dereferences a *string, returning "" when nil. Used
// by the source adapters to read Message.ChatId and similar
// optional SDK fields. Lived in adapter.go before the
// SessionChatID refactor; moved here so session_chatid.go is
// self-contained (no implicit dependency on the adapter file).
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// SessionChatIDSource is the minimal interface SessionChatID needs
// from any Feishu inbound event. Three concrete implementations —
// one per inbound event family — populate exactly one of the three
// methods; the other two are no-ops that return "".
//
// Defining this as an interface (rather than a single struct with
// every field) avoids nil-pointer dereferences when an event only
// has one of the three paths populated.
type SessionChatIDSource interface {
	TypedChatID() string
	EnvelopeChatID() string
	ContextOpenChatID() string
}

// SessionChatID is the single source of truth for chatID
// extraction across all Feishu inbound events. Pure function of
// the incoming event payload — no daemon state, no config, no
// SDK version detection, no format validation, no migration.
// The function deliberately takes no Adapter receiver: it is a
// pure function, not a method. Adding a receiver would imply
// state that does not exist.
//
// Modern Feishu SDK returns oc_<hex> from every chat-field
// populator. The three fallback paths must converge on the same
// string for the same chat (TestSessionChatID_AllSourcesAgree
// locks this invariant). If they ever drift, that's a bug in the
// SDK upgrade — fix the SDK dep, not this builder.
//
// Returns "" when no source produced a chatID. Callers treat ""
// as a drop (mirrors the existing extractReactionChatID behavior).
//
// Wire (inbound → runtime):
//
//	handleMessage        → receiveV1Source  → SessionChatID
//	handleReactionCreated → reactionV3Source → SessionChatID
//	handleCardAction     → cardActionSource → SessionChatID
//
// The same chat on the same IM therefore always produces the same
// chatID, across daemon restarts, channel adapter restarts, and
// SDK upgrades. This is the contract that lets /cwd in DM persist
// and find the same ChatSession on the next message — see
// docs/CHANNEL.md §5.5 for the cross-channel stability contract.
func SessionChatID(event SessionChatIDSource) string {
	if v := event.TypedChatID(); v != "" {
		return v
	}
	if v := event.EnvelopeChatID(); v != "" {
		return v
	}
	if v := event.ContextOpenChatID(); v != "" {
		return v
	}
	return ""
}

// --- source adapters (one per event family) ---

// receiveV1Source reads Message.ChatId from the typed struct
// (im.message.receive_v1). The EnvelopeChatID and
// ContextOpenChatID fallback methods are no-ops.
//
// Field path: event.Event.Message.ChatId (im/v1/model.go ChatId).
// JSON key: event.message.chat_id.
type receiveV1Source struct {
	event *larkim.P2MessageReceiveV1
}

func (s receiveV1Source) TypedChatID() string {
	if s.event == nil || s.event.Event == nil || s.event.Event.Message == nil {
		return ""
	}
	return stringValue(s.event.Event.Message.ChatId)
}

func (s receiveV1Source) EnvelopeChatID() string    { return "" }
func (s receiveV1Source) ContextOpenChatID() string { return "" }

// reactionV3Source reads chat_id from the raw JSON envelope
// because the Feishu reaction v3 SDK typed struct
// (P2MessageReactionCreatedV1Data) does not expose chat_id — see
// extractReactionChatID for the wire-shape contract. The
// TypedChatID and ContextOpenChatID fallback methods are no-ops.
//
// Field path: event.Body → {"chat_id": "oc_xxx"}.
type reactionV3Source struct {
	event *larkim.P2MessageReactionCreatedV1
}

func (s reactionV3Source) TypedChatID() string        { return "" }
func (s reactionV3Source) ContextOpenChatID() string { return "" }

func (s reactionV3Source) EnvelopeChatID() string {
	return extractReactionChatID(s.event)
}

// cardActionSource reads Context.OpenChatID from the card action
// event struct (card.action.trigger callback). The TypedChatID
// and EnvelopeChatID fallback methods are no-ops.
//
// Field path: event.Event.Context.OpenChatID (card/model.go).
// JSON key: event.context.open_chat_id.
//
// NOTE: this is the only path that consumes the *callback* event
// (vs the WS message event). The callback envelope never carries
// chat_id in event.context.chat_id — it uses open_chat_id
// exclusively. Per the Feishu SDK v3.9.10 source, modern tenants
// deliver oc_<hex> in both fields, so this returns the same
// string as receiveV1Source.TypedChatID for the same chat (see
// TestSessionChatID_AllSourcesAgree).
type cardActionSource struct {
	event *larkcallback.CardActionTriggerEvent
}

func (s cardActionSource) TypedChatID() string     { return "" }
func (s cardActionSource) EnvelopeChatID() string { return "" }

func (s cardActionSource) ContextOpenChatID() string {
	if s.event == nil || s.event.Event == nil || s.event.Event.Context == nil {
		return ""
	}
	return s.event.Event.Context.OpenChatID
}
