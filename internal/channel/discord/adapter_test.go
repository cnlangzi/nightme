package discord

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestOnMessage_DM_HasMention verifies DMs always pass HasMention.
func TestOnMessage_DM_HasMention(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"

	msg := &Message{
		ID:          "100",
		ChannelID:   "12345",
		ChannelType: 1, // DM
		Author:      User{ID: "111", Username: "alice"},
		Content:     "hello",
		Timestamp:   time.Now(),
	}
	a.onMessage(msg)

	select {
	case in := <-a.incoming:
		if in.ChatID != "dc_12345" {
			t.Errorf("ChatID = %q", in.ChatID)
		}
		if in.UserID != "111" {
			t.Errorf("UserID = %q", in.UserID)
		}
		if in.Text != "hello" {
			t.Errorf("Text = %q", in.Text)
		}
		if !in.HasMention {
			t.Error("HasMention = false on DM, want true")
		}
		if in.MessageID != "100" {
			t.Errorf("MessageID = %q", in.MessageID)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no inbound message published")
	}
}

// TestOnMessage_GuildText_RequiresMention verifies non-DM
// messages require an explicit mention to pass HasMention.
func TestOnMessage_GuildText_RequiresMention(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"

	msg := &Message{
		ID:          "100",
		ChannelID:   "12345",
		ChannelType: 0, // GUILD_TEXT
		Author:      User{ID: "111", Username: "alice"},
		Content:     "hello",
		Timestamp:   time.Now(),
	}
	a.onMessage(msg)

	select {
	case in := <-a.incoming:
		if in.HasMention {
			t.Error("HasMention = true on non-mention guild message, want false")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no inbound message published")
	}
}

// TestOnMessage_GuildText_Mentioned verifies a bot mention sets HasMention.
func TestOnMessage_GuildText_Mentioned(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"

	msg := &Message{
		ID:          "100",
		ChannelID:   "12345",
		ChannelType: 0,
		Author:      User{ID: "111", Username: "alice"},
		Content:     "<@999> hi",
		Timestamp:   time.Now(),
		Mentions:    []User{{ID: "999"}},
	}
	a.onMessage(msg)

	select {
	case in := <-a.incoming:
		if !in.HasMention {
			t.Error("HasMention = false when bot was mentioned, want true")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no inbound message")
	}
}

// TestOnMessage_SelfMessageDropped verifies messages authored by
// the bot itself are silently dropped (avoid the feedback loop).
func TestOnMessage_SelfMessageDropped(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"

	msg := &Message{
		ID:          "100",
		ChannelID:   "12345",
		ChannelType: 1,
		Author:      User{ID: "999", Username: "self", Bot: true},
		Content:     "self reply",
		Timestamp:   time.Now(),
	}
	a.onMessage(msg)
	select {
	case in := <-a.incoming:
		t.Errorf("self message leaked: %+v", in)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestOnMessage_UnsupportedChannelSilentDropped verifies messages
// from voice / forum / etc. are dropped silently.
func TestOnMessage_UnsupportedChannelSilentDropped(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"

	msg := &Message{
		ID:          "100",
		ChannelID:   "12345",
		ChannelType: 2, // GUILD_VOICE — unsupported in Phase 1
		Author:      User{ID: "111", Username: "alice"},
		Content:     "voice",
		Timestamp:   time.Now(),
	}
	a.onMessage(msg)
	select {
	case in := <-a.incoming:
		t.Errorf("unsupported channel leaked: %+v", in)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestOnMessage_SlashCommand verifies a leading slash forces
// HasMention even without an explicit @mention.
func TestOnMessage_SlashCommand(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"

	msg := &Message{
		ID:          "100",
		ChannelID:   "12345",
		ChannelType: 0,
		Author:      User{ID: "111", Username: "alice"},
		Content:     "/cwd /tmp/project",
		Timestamp:   time.Now(),
	}
	a.onMessage(msg)

	select {
	case in := <-a.incoming:
		if !in.HasMention {
			t.Error("slash command without @mention should set HasMention")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no inbound message")
	}
}

// TestOnMessage_ReplyLink verifies reply linkage is preserved on
// the InboundMessage.ReplyTo field.
func TestOnMessage_ReplyLink(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"

	msg := &Message{
		ID:               "100",
		ChannelID:        "12345",
		ChannelType:      1,
		Author:           User{ID: "111", Username: "alice"},
		Content:          "threaded reply",
		Timestamp:        time.Now(),
		MessageReference: &MessageReference{MessageID: "9999"},
	}
	a.onMessage(msg)

	select {
	case in := <-a.incoming:
		if in.ReplyTo != "9999" {
			t.Errorf("ReplyTo = %q, want 9999", in.ReplyTo)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no inbound message")
	}
}

// TestHealthSnapshot_Shape verifies the JSON envelope the
// daemoncontrol server returns.
func TestHealthSnapshot_Shape(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.botUserID = "999"
	a.botName = "nightme-bot"

	name, payload, err := a.HealthSnapshot()
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if name != "discord" {
		t.Errorf("name = %q", name)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["bot_id"] != "999" {
		t.Errorf("bot_id = %v", decoded["bot_id"])
	}
	if decoded["username"] != "nightme-bot" {
		t.Errorf("username = %v", decoded["username"])
	}
	if connected, _ := decoded["connected"].(bool); connected {
		t.Error("connected = true before Start")
	}
}

// TestBuildBlocks_TextOnly covers the common path: a text-only
// inbound message collapses to a single ContentText block.
func TestBuildBlocks_TextOnly(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	blocks := a.BuildBlocks("hello world", nil)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	if blocks[0].Type != agent.ContentText {
		t.Errorf("type = %v, want ContentText", blocks[0].Type)
	}
	if blocks[0].Text != "hello world" {
		t.Errorf("text = %q", blocks[0].Text)
	}
}

// TestBuildBlocks_AttachmentImageVsFile verifies the MIME-based
// split between ContentImage and ContentFile.
func TestBuildBlocks_AttachmentImageVsFile(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	attachments := []messages.Attachment{
		{LocalPath: "/tmp/p.png", MimeType: "image/png"},
		{LocalPath: "/tmp/d.pdf", MimeType: "application/pdf"},
	}
	blocks := a.BuildBlocks("see attached", attachments)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	if blocks[0].Type != agent.ContentText {
		t.Errorf("blocks[0] = %v, want ContentText", blocks[0].Type)
	}
	if blocks[1].Type != agent.ContentImage {
		t.Errorf("blocks[1] = %v, want ContentImage", blocks[1].Type)
	}
	if blocks[2].Type != agent.ContentFile {
		t.Errorf("blocks[2] = %v, want ContentFile", blocks[2].Type)
	}
}

// TestStartStop_BotVerification calls GetMe during Start and
// surfaces failure. Builds a minimal gatewayClient (no live
// connection) so Start can launch the run goroutine without
// panicking.
func TestStartStop_BotVerification(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	// Wire a no-op gateway so Start's goroutine spawn is safe;
	// the goroutine will block on gw.run which we never call.
	a.gw = &gatewayClient{
		api:   rest,
		state: a.state,
		cfg:   gatewaySnapshot{Token: "x", Intents: 46593, UserAgent: "test"},
	}
	a.SetLogger(testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Cancel the gateway run loop immediately so the deferred
	// Stop returns promptly.
	cancel()

	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if !rest.getMeCalled {
		t.Error("GetMe not called during Start")
	}
	if a.botUserID != "999" {
		t.Errorf("botUserID = %q, want 999", a.botUserID)
	}
}
