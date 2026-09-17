package discord

import (
	"encoding/json"
	"time"
)

// Snowflake is Discord's 64-bit integer id encoded as a decimal
// string on the wire. We keep it as a string to avoid
// platform-specific int64 width concerns and to match the JSON
// shape Discord emits — round-tripping a Snowflake through json
// must never drop leading digits or sign bits.
type Snowflake string

// Message mirrors the subset of Discord's message object the
// adapter consumes in Phase 1. Fields we don't use (e.g. components,
// thread, poll) are deliberately omitted so we don't decode bytes
// we never read.
type Message struct {
	ID                Snowflake         `json:"id"`
	ChannelID         Snowflake         `json:"channel_id"`
	GuildID           Snowflake         `json:"guild_id,omitempty"`
	Content           string            `json:"content"`
	Timestamp         time.Time         `json:"timestamp"`
	EditedTimestamp   *time.Time        `json:"edited_timestamp,omitempty"`
	Type              int               `json:"type"`
	Author            User              `json:"author"`
	Attachments       []Attachment      `json:"attachments"`
	Embeds            []json.RawMessage `json:"embeds,omitempty"`
	Mentions          []User            `json:"mentions"`
	MentionEveryone   bool              `json:"mention_everyone"`
	ReferencedMessage *Message          `json:"referenced_message,omitempty"`
	MessageReference  *MessageReference `json:"message_reference,omitempty"`
	Member            *Member           `json:"member,omitempty"`
	ChannelType       int               `json:"channel_type,omitempty"`
}

// User mirrors Discord's user object.
type User struct {
	ID            Snowflake `json:"id"`
	Username      string    `json:"username"`
	Discriminator string    `json:"discriminator,omitempty"`
	Bot           bool      `json:"bot,omitempty"`
	GlobalName    string    `json:"global_name,omitempty"`
}

type Member struct {
	Nick     string      `json:"nick,omitempty"`
	Roles    []Snowflake `json:"roles"`
	JoinedAt time.Time   `json:"joined_at,omitempty"`
}

type Attachment struct {
	ID          Snowflake `json:"id"`
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	URL         string    `json:"url"`
	ProxyURL    string    `json:"proxy_url"`
	ContentType string    `json:"content_type,omitempty"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
}

type MessageReference struct {
	Type            *int      `json:"type,omitempty"`
	MessageID       Snowflake `json:"message_id,omitempty"`
	ChannelID       Snowflake `json:"channel_id,omitempty"`
	GuildID         Snowflake `json:"guild_id,omitempty"`
	FailIfNotExists *bool     `json:"fail_if_not_exists,omitempty"`
}

// GatewayPayload is the on-the-wire envelope for every Gateway
// frame. Op is the opcode (0=Dispatch, 1=Heartbeat, 2=Identify,
// 6=Resume, 7=Reconnect, 9=Invalid Session, 10=Hello, 11=Heartbeat
// ACK). S is the sequence number persisted on every dispatch.
// T is the event name (READY, MESSAGE_CREATE, …) populated on op=0.
type GatewayPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// Hello is the d-payload of op=10. HeartbeatInterval is in
// milliseconds.
type Hello struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}

// Identify is the d-payload of op=2. Properties.OS follows Discord's
// recommended values: "linux" / "darwin" / "windows".
type Identify struct {
	Token      string             `json:"token"`
	Intents    int                `json:"intents"`
	Properties IdentifyProperties `json:"properties"`
}

type IdentifyProperties struct {
	OS      string `json:"os"`
	Browser string `json:"browser"`
	Device  string `json:"device"`
}

// Ready is the d-payload of op=0 t=READY. Guilds is left as raw
// JSON: Phase 1 doesn't enumerate guild state, so decoding each
// guild object would burn CPU and allocations for nothing.
type Ready struct {
	V                int               `json:"v"`
	User             User              `json:"user"`
	Guilds           []json.RawMessage `json:"guilds"`
	SessionID        string            `json:"session_id"`
	ResumeGatewayURL string            `json:"resume_gateway_url"`
}

// Resume is the d-payload of op=6.
type Resume struct {
	Token     string `json:"token"`
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
}

// GatewayBotResponse is the response shape of
// GET /gateway/bot. URL is the WebSocket endpoint (without the
// query string — we append it in gateway.go).
type GatewayBotResponse struct {
	URL               string            `json:"url"`
	Shards            int               `json:"shards"`
	SessionStartLimit SessionStartLimit `json:"session_start_limit"`
}

type SessionStartLimit struct {
	Total          int `json:"total"`
	Remaining      int `json:"remaining"`
	ResetAfter     int `json:"reset_after"`
	MaxConcurrency int `json:"max_concurrency"`
}

// CreateMessagePayload is the body of POST /channels/{id}/messages.
// AllowedMentions controls whether the message pings @users /
// @roles / @everyone; default-suppressing Parse is what every
// other adapter does to avoid accidental pings.
type CreateMessagePayload struct {
	Content          string            `json:"content,omitempty"`
	Nonce            string            `json:"nonce,omitempty"`
	TTS              bool              `json:"tts,omitempty"`
	Embeds           []json.RawMessage `json:"embeds,omitempty"`
	AllowedMentions  *AllowedMentions  `json:"allowed_mentions,omitempty"`
	MessageReference *MessageReference `json:"message_reference,omitempty"`
}

type AllowedMentions struct {
	Parse       []string `json:"parse,omitempty"`
	Users       []string `json:"users,omitempty"`
	Roles       []string `json:"roles,omitempty"`
	RepliedUser bool     `json:"replied_user,omitempty"`
}

// EditMessagePayload is the body of PATCH /channels/{id}/messages/{id}.
type EditMessagePayload struct {
	Content         string            `json:"content,omitempty"`
	Embeds          []json.RawMessage `json:"embeds,omitempty"`
	AllowedMentions *AllowedMentions  `json:"allowed_mentions,omitempty"`
}

// isSupportedChannelType reports whether the adapter should
// process inbound MESSAGE_CREATE events from a channel of the
// given Discord type. Phase 1 supports guild text, DM, guild
// announcement, and the three thread flavours; voice / stage /
// forum / media channels are silent-drop (the request spec §2.7).
func isSupportedChannelType(t int) bool {
	switch t {
	case 0, // GUILD_TEXT
		1,  // DM
		5,  // GUILD_ANNOUNCEMENT
		10, // ANNOUNCEMENT_THREAD (public)
		11, // PUBLIC_THREAD
		12: // PRIVATE_THREAD
		return true
	}
	return false
}
