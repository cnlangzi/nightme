package discord

import (
	"encoding/json"
	"testing"
	"time"
)

// TestMessage_DecodeFixture exercises the on-the-wire shape
// Discord returns for MESSAGE_CREATE. Keeping the fixture aligned
// with the API docs (the request spec §2.7) means a Discord JSON
// change surfaces here first.
func TestMessage_DecodeFixture(t *testing.T) {
	const fixture = `{
		"id": "1234567890123456789",
		"channel_id": "987654321098765432",
		"guild_id": "111111111111111111",
		"content": "hello nightme",
		"timestamp": "2026-01-15T12:34:56.789000+00:00",
		"type": 0,
		"author": {
			"id": "222222222222222222",
			"username": "alice",
			"discriminator": "0",
			"bot": false
		},
		"attachments": [],
		"mentions": [],
		"mention_everyone": false,
		"channel_type": 0
	}`
	var msg Message
	if err := json.Unmarshal([]byte(fixture), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(msg.ID) != "1234567890123456789" {
		t.Errorf("ID = %q", msg.ID)
	}
	if string(msg.ChannelID) != "987654321098765432" {
		t.Errorf("ChannelID = %q", msg.ChannelID)
	}
	if msg.Content != "hello nightme" {
		t.Errorf("Content = %q", msg.Content)
	}
	if msg.Author.Username != "alice" {
		t.Errorf("Author.Username = %q", msg.Author.Username)
	}
	if msg.Author.Bot {
		t.Error("Author.Bot = true, want false")
	}
	if msg.ChannelType != 0 {
		t.Errorf("ChannelType = %d, want 0 (GUILD_TEXT)", msg.ChannelType)
	}
	if msg.Timestamp.IsZero() {
		t.Error("Timestamp not parsed")
	}
}

func TestIsSupportedChannelType(t *testing.T) {
	supported := []int{0, 1, 5, 10, 11, 12}
	for _, c := range supported {
		if !isSupportedChannelType(c) {
			t.Errorf("isSupportedChannelType(%d) = false, want true", c)
		}
	}
	unsupported := []int{2, 3, 4, 13, 14, 15}
	for _, c := range unsupported {
		if isSupportedChannelType(c) {
			t.Errorf("isSupportedChannelType(%d) = true, want false", c)
		}
	}
}

// TestSnowflake_StringRoundTrip ensures snowflakes survive a
// JSON round trip without precision loss.
func TestSnowflake_StringRoundTrip(t *testing.T) {
	const id = "1234567890123456789"
	s := Snowflake(id)
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Snowflake
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != s {
		t.Errorf("round trip: got %q, want %q", back, s)
	}
}

// TestReady_DecodeFixture exercises the op=0 t=READY payload
// the gateway emits on connection.
func TestReady_DecodeFixture(t *testing.T) {
	const fixture = `{
		"v": 10,
		"user": {"id": "2222", "username": "bot", "bot": true},
		"guilds": [],
		"session_id": "abcdef0123456789",
		"resume_gateway_url": "wss://gateway.example/discord"
	}`
	var ready Ready
	if err := json.Unmarshal([]byte(fixture), &ready); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ready.SessionID != "abcdef0123456789" {
		t.Errorf("SessionID = %q", ready.SessionID)
	}
	if ready.ResumeGatewayURL != "wss://gateway.example/discord" {
		t.Errorf("ResumeGatewayURL = %q", ready.ResumeGatewayURL)
	}
	if !ready.User.Bot {
		t.Error("Ready.User.Bot = false, want true")
	}
}

func TestReplyTargetOf(t *testing.T) {
	msgID := Snowflake("1111")
	refID := Snowflake("2222")
	// MessageReference takes priority over ReferencedMessage.
	m := &Message{
		MessageReference:  &MessageReference{MessageID: refID},
		ReferencedMessage: &Message{ID: msgID},
	}
	if got := replyTargetOf(m); got != "2222" {
		t.Errorf("replyTargetOf with both = %q, want 2222", got)
	}
	// Only ReferencedMessage.
	m = &Message{ReferencedMessage: &Message{ID: msgID}}
	if got := replyTargetOf(m); got != "1111" {
		t.Errorf("replyTargetOf with ref-only = %q, want 1111", got)
	}
	// Neither.
	if got := replyTargetOf(&Message{}); got != "" {
		t.Errorf("replyTargetOf with neither = %q, want empty", got)
	}
}

// TestTimestampParse verifies Discord's microsecond-precision
// timestamps decode correctly (relevant for HasMention timing
// comparisons).
func TestTimestampParse(t *testing.T) {
	const stamp = "2026-01-15T12:34:56.789000+00:00"
	m := Message{}
	if err := json.Unmarshal([]byte(`{"timestamp":"`+stamp+`"}`), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	expected := time.Date(2026, 1, 15, 12, 34, 56, 789000000, time.UTC)
	if !m.Timestamp.Equal(expected) {
		t.Errorf("Timestamp = %v, want %v", m.Timestamp, expected)
	}
}
