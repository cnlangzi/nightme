package slack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
)

// callbackEvent wraps an inner event the way Slack delivers it over
// Socket Mode, including the raw JSON the adapter re-parses for
// fields slack-go's structs omit.
func callbackEvent(t *testing.T, inner any, rawJSON string) socketmode.Event {
	t.Helper()
	raw := json.RawMessage(rawJSON)
	payload := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		Data: slackevents.EventsAPICallbackEvent{
			Type:       slackevents.CallbackEvent,
			InnerEvent: &raw,
		},
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: inner},
	}
	return socketmode.Event{
		Type:    socketmode.EventTypeEventsAPI,
		Data:    payload,
		Request: &socketmode.Request{},
	}
}

func drainOne(t *testing.T, a *Adapter) (messages.InboundMessage, bool) {
	t.Helper()
	select {
	case msg := <-a.Incoming():
		return msg, true
	case <-time.After(time.Second):
		return messages.InboundMessage{}, false
	}
}

// The reason dedup exists: nightme subscribes to app_mention AND
// message.channels so /watch all is possible, and Slack then
// delivers an @-mention through both.
func TestInbound_MentionDeliveredTwiceIsProcessedOnce(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	mention := &slackevents.AppMentionEvent{
		User: "U1", Text: "<@UBOT> hello", TimeStamp: "1000.1", Channel: "C1",
	}
	message := &slackevents.MessageEvent{
		User: "U1", Text: "<@UBOT> hello", TimeStamp: "1000.1",
		Channel: "C1", ChannelType: "channel",
	}

	a.handleSocketEvent(ctx, callbackEvent(t, mention, `{"ts":"1000.1"}`))
	a.handleSocketEvent(ctx, callbackEvent(t, message, `{"ts":"1000.1"}`))

	if _, ok := drainOne(t, a); !ok {
		t.Fatal("the first delivery should be published")
	}
	select {
	case extra := <-a.Incoming():
		t.Fatalf("the duplicate delivery leaked through: %+v", extra)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestInbound_DistinctMessagesBothPass(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	for _, ts := range []string{"1000.1", "1000.2"} {
		ev := &slackevents.MessageEvent{
			User: "U1", Text: "hi", TimeStamp: ts, Channel: "D1", ChannelType: "im",
		}
		a.handleSocketEvent(ctx, callbackEvent(t, ev, `{"ts":"`+ts+`"}`))
	}
	for i := 0; i < 2; i++ {
		if _, ok := drainOne(t, a); !ok {
			t.Fatalf("message %d was dropped", i)
		}
	}
}

func TestInbound_DMStampsChatIDAndMention(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	ev := &slackevents.MessageEvent{
		User: "U1", Text: "hello", TimeStamp: "1000.1",
		Channel: "D1", ChannelType: "im",
	}
	a.handleSocketEvent(context.Background(), callbackEvent(t, ev, `{"ts":"1000.1"}`))

	msg, ok := drainOne(t, a)
	if !ok {
		t.Fatal("no inbound message")
	}
	if msg.ChatID != "sl_T1:D1" {
		t.Fatalf("chat id = %q", msg.ChatID)
	}
	if !msg.HasMention {
		t.Fatal("a DM must always be flagged as a mention")
	}
	if msg.MessageID != "1000.1" {
		t.Fatalf("message id = %q", msg.MessageID)
	}
}

func TestInbound_ThreadedMessageGetsThreadScopedChatID(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	ev := &slackevents.MessageEvent{
		User: "U1", Text: "<@UBOT> more", TimeStamp: "1000.2",
		ThreadTimeStamp: "1000.1", Channel: "C1", ChannelType: "channel",
	}
	a.handleSocketEvent(context.Background(), callbackEvent(t, ev, `{"ts":"1000.2"}`))

	msg, ok := drainOne(t, a)
	if !ok {
		t.Fatal("no inbound message")
	}
	if msg.ChatID != "sl_T1:C1:1000.1" {
		t.Fatalf("chat id = %q, want the thread-scoped form", msg.ChatID)
	}
}

func TestInbound_MentionPrefixStrippedSoCommandsParse(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	ev := &slackevents.AppMentionEvent{
		User: "U1", Text: "<@UBOT> /cwd /tmp", TimeStamp: "1000.1", Channel: "C1",
	}
	a.handleSocketEvent(context.Background(), callbackEvent(t, ev, `{"ts":"1000.1"}`))

	msg, ok := drainOne(t, a)
	if !ok {
		t.Fatal("no inbound message")
	}
	if msg.Text != "/cwd /tmp" {
		t.Fatalf("text = %q — the command parser needs a leading slash", msg.Text)
	}
}

func TestInbound_BotAndSubtypeEventsAreIgnored(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	cases := []*slackevents.MessageEvent{
		{BotID: "B1", Text: "from a bot", TimeStamp: "1", Channel: "C1"},
		{User: "U1", SubType: "message_changed", Text: "edit", TimeStamp: "2", Channel: "C1"},
		{User: "UBOT", Text: "our own message", TimeStamp: "3", Channel: "C1", ChannelType: "im"},
		{Text: "no user", TimeStamp: "4", Channel: "C1"},
	}
	for _, ev := range cases {
		a.handleSocketEvent(ctx, callbackEvent(t, ev, `{}`))
	}

	select {
	case msg := <-a.Incoming():
		t.Fatalf("expected all of these to be ignored, got %+v", msg)
	case <-time.After(150 * time.Millisecond):
	}
}

// parent_user_id is not on slack-go's MessageEvent struct, so the
// adapter digs it out of the raw payload; a reply to the bot counts
// as addressing it.
func TestInbound_ReplyToBotCountsAsMention(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	ev := &slackevents.MessageEvent{
		User: "U1", Text: "yes please", TimeStamp: "1000.2",
		ThreadTimeStamp: "1000.1", Channel: "C1", ChannelType: "channel",
	}
	a.handleSocketEvent(context.Background(),
		callbackEvent(t, ev, `{"ts":"1000.2","parent_user_id":"UBOT"}`))

	msg, ok := drainOne(t, a)
	if !ok {
		t.Fatal("no inbound message")
	}
	if !msg.HasMention {
		t.Fatal("replying to the bot should count as addressing it")
	}
}

// Files live on the raw payload only — slack-go's typed events omit
// them entirely.
func TestInbound_FilesComeFromRawPayload(t *testing.T) {
	api := newFakeAPI()
	api.downloadBody = []byte("PNGDATA")
	a := newTestAdapter(t, api, newFakeSocket())

	ev := &slackevents.MessageEvent{
		User: "U1", Text: "look", TimeStamp: "1000.1",
		Channel: "D1", ChannelType: "im", SubType: "file_share",
	}
	raw := `{"ts":"1000.1","files":[{"id":"F1","name":"shot.png","mimetype":"image/png","url_private":"https://files.slack.com/x"}]}`
	a.handleSocketEvent(context.Background(), callbackEvent(t, ev, raw))

	msg, ok := drainOne(t, a)
	if !ok {
		t.Fatal("no inbound message")
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(msg.Attachments))
	}
	att := msg.Attachments[0]
	if att.LocalPath == "" {
		t.Fatal("LocalPath must be populated before publishing (InboundMessage contract)")
	}
	if att.MimeType != "image/png" || att.Type != "image" {
		t.Fatalf("attachment = %+v", att)
	}
	if len(msg.Blocks) != 2 {
		t.Fatalf("blocks = %d, want text + image", len(msg.Blocks))
	}
}

func TestInbound_FailedDownloadKeepsAttachmentWithError(t *testing.T) {
	api := newFakeAPI()
	api.downloadErr = ErrHTMLResponse
	a := newTestAdapter(t, api, newFakeSocket())

	ev := &slackevents.MessageEvent{
		User: "U1", Text: "look", TimeStamp: "1000.1",
		Channel: "D1", ChannelType: "im", SubType: "file_share",
	}
	raw := `{"ts":"1000.1","files":[{"id":"F1","name":"a.png","mimetype":"image/png","url_private":"https://files.slack.com/x"}]}`
	a.handleSocketEvent(context.Background(), callbackEvent(t, ev, raw))

	msg, ok := drainOne(t, a)
	if !ok {
		t.Fatal("no inbound message")
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].Error == nil {
		t.Fatal("a failed download should be reported, not silently dropped")
	}
	if msg.Attachments[0].LocalPath != "" {
		t.Fatal("a failed download must not claim a local path")
	}
}

func TestSocketEvents_AreAcked(t *testing.T) {
	api := newFakeAPI()
	sock := newFakeSocket()
	a := newTestAdapter(t, api, sock)

	ev := &slackevents.MessageEvent{
		User: "U1", Text: "hi", TimeStamp: "1000.1", Channel: "D1", ChannelType: "im",
	}
	a.handleSocketEvent(context.Background(), callbackEvent(t, ev, `{"ts":"1000.1"}`))

	if sock.ackCount() != 1 {
		t.Fatalf("ack count = %d, want 1 — an unacked event gets redelivered", sock.ackCount())
	}
}

func TestSocketEvents_ConnectionLifecycleFeedsHealth(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	a.handleSocketEvent(ctx, socketmode.Event{Type: socketmode.EventTypeConnecting})
	a.handleSocketEvent(ctx, socketmode.Event{Type: socketmode.EventTypeConnected})

	name, payload, err := a.HealthSnapshot()
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if name != "slack" {
		t.Fatalf("name = %q", name)
	}
	var snap HealthSnapshot
	if err := json.Unmarshal(payload, &snap); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if !snap.Connected {
		t.Fatal("health should report the connection as up")
	}
	if snap.ConnectCount != 1 {
		t.Fatalf("connect count = %d", snap.ConnectCount)
	}

	a.handleSocketEvent(ctx, socketmode.Event{Type: socketmode.EventTypeDisconnect})
	_, payload, _ = a.HealthSnapshot()
	_ = json.Unmarshal(payload, &snap)
	if snap.Connected {
		t.Fatal("health should report the connection as down after a disconnect")
	}
}

func TestSlashCommand_BecomesInboundText(t *testing.T) {
	api := newFakeAPI()
	sock := newFakeSocket()
	a := newTestAdapter(t, api, sock)

	a.handleSocketEvent(context.Background(), socketmode.Event{
		Type:    socketmode.EventTypeSlashCommand,
		Request: &socketmode.Request{},
		Data: slackgo.SlashCommand{
			Command: "/cwd", Text: "/tmp", ChannelID: "C1",
			UserID: "U1", TeamID: "T1", TriggerID: "trig-1",
		},
	})

	msg, ok := drainOne(t, a)
	if !ok {
		t.Fatal("no inbound message")
	}
	if msg.Text != "/cwd /tmp" {
		t.Fatalf("text = %q", msg.Text)
	}
	if !msg.HasMention {
		t.Fatal("an explicit command is unambiguously for the bot")
	}
}

func TestInteractive_BlockActionBecomesActionPayload(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	cb := slackgo.InteractionCallback{Type: slackgo.InteractionTypeBlockActions}
	cb.User.ID = "U1"
	cb.Channel.ID = "C1"
	cb.Message.Timestamp = "1000.5"
	cb.ActionCallback.BlockActions = []*slackgo.BlockAction{
		{ActionID: "nightme_choice_allow", Value: encodeActionValue("req-1", "allow")},
	}

	a.handleSocketEvent(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeInteractive, Request: &socketmode.Request{}, Data: cb,
	})

	msg, ok := drainOne(t, a)
	if !ok {
		t.Fatal("no inbound message")
	}
	if msg.Action == nil {
		t.Fatal("expected an ActionPayload")
	}
	if msg.Action.RequestID != "req-1" || msg.Action.Option != "allow" {
		t.Fatalf("action = %+v", msg.Action)
	}
}

// A stream left open by a crashed process renders as forever
// in-progress; recovery closes it on the next start.
func TestStart_ClosesOrphanStreamsFromPreviousRun(t *testing.T) {
	api := newFakeAPI()
	dir := t.TempDir()
	a, err := NewAdapterWithDeps(defaultTestConfig(), api, newFakeSocket(), dir)
	if err != nil {
		t.Fatalf("NewAdapterWithDeps: %v", err)
	}
	a.state.putStream(&OpenStream{
		ChatID: "sl_T1:C1", ChannelID: "C1", TS: "old-1",
		StartedAt: time.Now().UTC(),
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop(context.Background())

	stopped := false
	for _, c := range api.snapshot() {
		if c.Method == "StopStream" && c.TS == "old-1" {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("orphan stream was not closed, calls = %v", api.methods())
	}
	if n := len(a.state.orphanStreams(time.Now().UTC())); n != 0 {
		t.Fatalf("recovered records should be forgotten, got %d", n)
	}
}

// A record too old to be meaningful is dropped rather than acted on.
func TestStart_SkipsExpiredOrphanRecords(t *testing.T) {
	api := newFakeAPI()
	a, err := NewAdapterWithDeps(defaultTestConfig(), api, newFakeSocket(), t.TempDir())
	if err != nil {
		t.Fatalf("NewAdapterWithDeps: %v", err)
	}
	a.state.putStream(&OpenStream{
		ChatID: "sl_T1:C1", ChannelID: "C1", TS: "ancient",
		StartedAt: time.Now().UTC().Add(-openStreamTTL - time.Hour),
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop(context.Background())

	for _, c := range api.snapshot() {
		if c.Method == "StopStream" {
			t.Fatal("an expired record should be dropped, not acted on")
		}
	}
}

// A persistent orphan close failure must keep the local record so
// the next start retries — evicting on failure used to leak the
// Slack stream forever whenever the close transiently failed.
func TestStart_OrphanCloseFailureKeepsRecord(t *testing.T) {
	api := newFakeAPI()
	api.failAlways("StopStream", errBoom)
	a, err := NewAdapterWithDeps(defaultTestConfig(), api, newFakeSocket(), t.TempDir())
	if err != nil {
		t.Fatalf("NewAdapterWithDeps: %v", err)
	}
	a.state.putStream(&OpenStream{
		ChatID: "sl_T1:C1", ChannelID: "C1", TS: "stuck",
		StartedAt: time.Now().UTC(),
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop(context.Background())

	if n := len(a.state.orphanStreams(time.Now().UTC())); n != 1 {
		t.Fatalf("record should be kept on close failure, got %d", n)
	}
}

// handleInteractive must fall back to cb.Team.ID when a.teamID has
// not yet been populated (early startup race or auth-not-yet-done).
func TestInteractive_FallsBackToCallbackTeamID(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	// Zero out teamID; the fallback must kick in.
	a.teamID = ""

	cb := slackgo.InteractionCallback{Type: slackgo.InteractionTypeBlockActions}
	cb.User.ID = "U1"
	cb.Team.ID = "TFALLBACK"
	cb.Channel.ID = "C1"
	cb.Message.Timestamp = "1000.5"
	cb.ActionCallback.BlockActions = []*slackgo.BlockAction{
		{ActionID: "nightme_choice_allow", Value: encodeActionValue("req-1", "allow")},
	}

	a.handleSocketEvent(context.Background(), socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Request: &socketmode.Request{},
		Data:    cb,
	})

	msg, ok := drainOne(t, a)
	if !ok {
		t.Fatal("no inbound message — fallback to cb.Team.ID should have produced a chatID")
	}
	if msg.ChatID == "" {
		t.Fatalf("chatID must be non-empty when fallback team id is present")
	}
}

// AuthTest failing with context.Canceled must surface a "startup
// cancelled" error, not the misleading "auth test" one — otherwise
// graceful shutdown looks like a token rejection in the logs.
func TestStart_AuthTestCancelledReportsStartupCancelled(t *testing.T) {
	api := newFakeAPI()
	api.authErr = context.Canceled
	a, err := NewAdapterWithDeps(defaultTestConfig(), api, newFakeSocket(), t.TempDir())
	if err != nil {
		t.Fatalf("NewAdapterWithDeps: %v", err)
	}

	err = a.Start(context.Background())
	if err == nil {
		t.Fatal("Start should propagate cancellation")
	}
	if !strings.Contains(err.Error(), "startup cancelled") {
		t.Fatalf("expected 'startup cancelled' in error, got %q", err)
	}
}

func TestStop_ClosesLiveStreams(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	if err := a.Send(ctx, outbound(messages.OutReply, "mid-turn")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if n := api.countOf("StopStream"); n != 1 {
		t.Fatalf("shutdown should close the live stream, got %d StopStream calls", n)
	}
}

func TestStart_FailsWhenAuthRejected(t *testing.T) {
	api := newFakeAPI()
	api.authErr = errBoom
	a, err := NewAdapterWithDeps(defaultTestConfig(), api, newFakeSocket(), t.TempDir())
	if err != nil {
		t.Fatalf("NewAdapterWithDeps: %v", err)
	}

	if startErr := a.Start(context.Background()); startErr == nil {
		t.Fatal("Start should fail when the token is rejected")
	}
}

func TestNewAdapter_RequiresBothTokens(t *testing.T) {
	if _, err := NewAdapter(nil); err == nil {
		t.Fatal("nil config should error")
	}
}

// defaultTestConfig is the minimal valid Slack config for tests that
// construct an Adapter directly rather than via newTestAdapter.
func defaultTestConfig() config.SlackConfig {
	return config.SlackConfig{BotToken: "xoxb-test", AppToken: "xapp-test"}
}
