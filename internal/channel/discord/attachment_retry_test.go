package discord

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestDownloadRetryConfig_LadderShape pins the F-61 ladder shape
// against silent drift: MaxAttempts=3, Backoffs[0]=0 (immediate),
// and the second backoff strictly less than the third.
func TestDownloadRetryConfig_LadderShape(t *testing.T) {
	if got, want := downloadRetryConfig.MaxAttempts, 3; got != want {
		t.Errorf("MaxAttempts = %d, want %d", got, want)
	}
	if len(downloadRetryConfig.Backoffs) != 3 {
		t.Fatalf("Backoffs len = %d, want 3", len(downloadRetryConfig.Backoffs))
	}
	if downloadRetryConfig.Backoffs[0] != 0 {
		t.Errorf("Backoffs[0] = %v, want 0 (immediate)", downloadRetryConfig.Backoffs[0])
	}
	if downloadRetryConfig.Backoffs[1] <= 0 {
		t.Errorf("Backoffs[1] = %v, want > 0", downloadRetryConfig.Backoffs[1])
	}
	if downloadRetryConfig.Backoffs[2] <= downloadRetryConfig.Backoffs[1] {
		t.Errorf("Backoffs[2] %v must exceed Backoffs[1] %v",
			downloadRetryConfig.Backoffs[2], downloadRetryConfig.Backoffs[1])
	}
}

// flakyREST is a minimal restClient that fails the first
// n.DownloadErrors download calls, then succeeds. Drives the
// "success on attempt N" branch of the F-61 ladder.
type flakyREST struct {
	mu             atomic.Int64
	downloadErrors int
	downloadBody   []byte
}

func (f *flakyREST) GetGatewayBot(context.Context) (GatewayBotResponse, error) {
	return GatewayBotResponse{URL: "wss://gateway.discord.gg"}, nil
}
func (f *flakyREST) GetMe(context.Context) (User, error) { return User{ID: "999"}, nil }
func (f *flakyREST) CreateMessage(context.Context, string, CreateMessagePayload) (Message, error) {
	return Message{ID: "out"}, nil
}
func (f *flakyREST) EditMessage(context.Context, string, string, EditMessagePayload) (Message, error) {
	return Message{}, nil
}
func (f *flakyREST) DeleteMessage(context.Context, string, string) error { return nil }
func (f *flakyREST) AddReaction(context.Context, string, string, string) error {
	return nil
}
func (f *flakyREST) RemoveOwnReaction(context.Context, string, string, string) error {
	return nil
}
func (f *flakyREST) ClearReactions(context.Context, string, string) error { return nil }
func (f *flakyREST) AcknowledgeInteraction(context.Context, string, string, InteractionResponse) error {
	return nil
}
func (f *flakyREST) Download(_ context.Context, _ string) ([]byte, error) {
	n := f.mu.Add(1)
	if n <= int64(f.downloadErrors) {
		return nil, errors.New("flaky: simulated CDN 503")
	}
	return f.downloadBody, nil
}

// TestDownloadAttachmentsWithRetry_SuccessOnFirstAttempt verifies
// the happy path: a successful first attempt returns immediately
// with AllFailed=false and the attachment LocalPath populated.
func TestDownloadAttachmentsWithRetry_SuccessOnFirstAttempt(t *testing.T) {
	tmp := t.TempDir()
	rest := &flakyREST{downloadBody: []byte("ok")}
	a := newTestAdapter(rest)
	a.dataDir = tmp

	msg := &Message{
		ID:        "msg-1",
		ChannelID: "chan1",
		Attachments: []Attachment{
			{ID: "a1", Filename: "f.txt", URL: "https://cdn/x", ContentType: "text/plain", Size: 2},
		},
	}
	res := a.downloadAttachmentsWithRetry(context.Background(), msg, "dc_chan1")
	if res.AllFailed {
		t.Errorf("AllFailed = true, want false on success")
	}
	if res.FailureCount != 0 {
		t.Errorf("FailureCount = %d, want 0", res.FailureCount)
	}
	if len(res.Atts) != 1 || res.Atts[0].LocalPath == "" {
		t.Errorf("Atts = %+v, want single LocalPath-populated entry", res.Atts)
	}
	if got := rest.mu.Load(); got != 1 {
		t.Errorf("downloads = %d, want 1 (early exit on success)", got)
	}
}

// TestDownloadAttachmentsWithRetry_SuccessOnSecondAttempt
// exercises the first retry backoff: one failure, then success.
// Backoff is shortened via a deferred package-var override so the
// test stays under a second.
func TestDownloadAttachmentsWithRetry_SuccessOnSecondAttempt(t *testing.T) {
	orig := downloadRetryConfig.Backoffs
	downloadRetryConfig.Backoffs = []time.Duration{0, 10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { downloadRetryConfig.Backoffs = orig })

	tmp := t.TempDir()
	rest := &flakyREST{downloadErrors: 1, downloadBody: []byte("ok")}
	a := newTestAdapter(rest)
	a.dataDir = tmp

	msg := &Message{
		ID:        "msg-2",
		ChannelID: "chan1",
		Attachments: []Attachment{
			{ID: "a1", Filename: "f.txt", URL: "https://cdn/x", ContentType: "text/plain", Size: 2},
		},
	}
	res := a.downloadAttachmentsWithRetry(context.Background(), msg, "dc_chan1")
	if res.AllFailed {
		t.Errorf("AllFailed = true, want false on attempt-2 success")
	}
	if got := rest.mu.Load(); got != 2 {
		t.Errorf("downloads = %d, want 2", got)
	}
}

// TestDownloadAttachmentsWithRetry_AllFailedAfterMaxAttempts
// verifies the outer ladder exhausts exactly MaxAttempts × N
// attachments, then declares AllFailed with the right
// FailureCount.
func TestDownloadAttachmentsWithRetry_AllFailedAfterMaxAttempts(t *testing.T) {
	orig := downloadRetryConfig.Backoffs
	downloadRetryConfig.Backoffs = []time.Duration{0, 5 * time.Millisecond, 5 * time.Millisecond}
	t.Cleanup(func() { downloadRetryConfig.Backoffs = orig })

	tmp := t.TempDir()
	rest := &flakyREST{downloadErrors: 100}
	a := newTestAdapter(rest)
	a.dataDir = tmp

	msg := &Message{
		ID:        "msg-3",
		ChannelID: "chan1",
		Attachments: []Attachment{
			{ID: "a1", Filename: "x.png", URL: "https://cdn/x", ContentType: "image/png"},
			{ID: "a2", Filename: "y.png", URL: "https://cdn/y", ContentType: "image/png"},
		},
	}
	res := a.downloadAttachmentsWithRetry(context.Background(), msg, "dc_chan1")
	if !res.AllFailed {
		t.Errorf("AllFailed = false, want true")
	}
	if res.FailureCount != 2 {
		t.Errorf("FailureCount = %d, want 2", res.FailureCount)
	}
	wantCalls := int64(downloadRetryConfig.MaxAttempts * 2)
	if got := rest.mu.Load(); got != wantCalls {
		t.Errorf("downloads = %d, want %d (MaxAttempts × N_attachments)", got, wantCalls)
	}
}

// TestNotifyDownloadFailure_EmitsOutError covers the
// user-visible failure notice path. pureImage=false produces
// the "sending text only" wording; the notice lands on
// fakeREST.creates with Kind=OutError.
func TestNotifyDownloadFailure_EmitsOutError(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.metrics = newHealthMetrics()

	a.notifyDownloadFailure("12345", "user-msg-1", downloadResult{
		AllFailed:    true,
		FailureCount: 2,
	}, false)

	if got := len(rest.creates); got != 1 {
		t.Fatalf("CreateMessage calls = %d, want 1", got)
	}
	body := rest.creates[0].Content
	if body == "" {
		t.Fatal("CreateMessage content empty")
	}
	if got := a.metrics.snapshot().AttachmentDownloadFails; got != 1 {
		t.Errorf("AttachmentDownloadFails = %d, want 1", got)
	}
}

// TestNotifyDownloadFailure_PureImageDrops covers the
// pure-image escalation: the notice still fires (so the user
// knows the message was dropped), but the wording uses the
// "please retry" variant.
func TestNotifyDownloadFailure_PureImageDrops(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.metrics = newHealthMetrics()

	a.notifyDownloadFailure("12345", "user-msg-2", downloadResult{
		AllFailed:    true,
		FailureCount: 1,
	}, true)

	if got := len(rest.creates); got != 1 {
		t.Fatalf("CreateMessage calls = %d, want 1", got)
	}
	body := rest.creates[0].Content
	if body == "" {
		t.Fatal("CreateMessage content empty")
	}
	// Pure-image wording references the drop, not "sending text".
	if !strings.Contains(body, "Message dropped") {
		t.Errorf("pure-image notice missing drop wording: %q", body)
	}
}

// TestDownloadAndPublish_AllFailedEmitsNoticeAndStopsPublish
// exercises the integration: when downloadAttachmentsWithRetry
// returns AllFailed on a pure-image message, the inbound message
// is dropped (no publish to a.incoming) and the user-visible
// OutError notice fires once.
func TestDownloadAndPublish_AllFailedEmitsNoticeAndStopsPublish(t *testing.T) {
	orig := downloadRetryConfig.Backoffs
	downloadRetryConfig.Backoffs = []time.Duration{0, 5 * time.Millisecond, 5 * time.Millisecond}
	t.Cleanup(func() { downloadRetryConfig.Backoffs = orig })

	tmp := t.TempDir()
	rest := &flakyREST{downloadErrors: 100}
	a := newTestAdapter(rest)
	a.dataDir = tmp
	a.metrics = newHealthMetrics()
	a.botUserID = "999"
	a.botName = "nightme-bot"

	msg := &Message{
		ID:          "msg-4",
		ChannelID:   "12345",
		ChannelType: 1, // DM
		Author:      User{ID: "111", Username: "alice"},
		Content:     "", // pure-image
		Timestamp:   time.Now(),
		Attachments: []Attachment{
			{ID: "a1", Filename: "x.png", URL: "https://cdn/x", ContentType: "image/png"},
		},
	}
	a.downloadAndPublish(msg, "dc_12345", messages.InboundMessage{
		ChatID:     "dc_12345",
		UserID:     "111",
		MessageID:  "msg-4",
		HasMention: true,
	})

	select {
	case in := <-a.incoming:
		t.Errorf("pure-image drop leaked: %+v", in)
	case <-time.After(200 * time.Millisecond):
		// expected
	}
	if got := a.metrics.snapshot().AttachmentDownloadFails; got != 1 {
		t.Errorf("AttachmentDownloadFails = %d, want 1", got)
	}
}

// TestDownloadAndPublish_TextBearingDegradesToTextOnly verifies
// the F-61 partial-failure shape: a text-bearing message whose
// attachments all fail publishes text-only with a warning notice.
func TestDownloadAndPublish_TextBearingDegradesToTextOnly(t *testing.T) {
	orig := downloadRetryConfig.Backoffs
	downloadRetryConfig.Backoffs = []time.Duration{0, 5 * time.Millisecond, 5 * time.Millisecond}
	t.Cleanup(func() { downloadRetryConfig.Backoffs = orig })

	tmp := t.TempDir()
	rest := &flakyREST{downloadErrors: 100}
	a := newTestAdapter(rest)
	a.dataDir = tmp
	a.metrics = newHealthMetrics()
	a.botUserID = "999"
	a.botName = "nightme-bot"

	msg := &Message{
		ID:          "msg-5",
		ChannelID:   "12345",
		ChannelType: 1,
		Author:      User{ID: "111", Username: "alice"},
		Content:     "look at this",
		Timestamp:   time.Now(),
		Attachments: []Attachment{
			{ID: "a1", Filename: "x.png", URL: "https://cdn/x", ContentType: "image/png"},
		},
	}
	a.downloadAndPublish(msg, "dc_12345", messages.InboundMessage{
		ChatID:     "dc_12345",
		UserID:     "111",
		Text:       "look at this",
		MessageID:  "msg-5",
		HasMention: true,
	})

	select {
	case in := <-a.incoming:
		if in.Text != "look at this" {
			t.Errorf("Text = %q, want preserved", in.Text)
		}
		if len(in.Attachments) != 0 {
			t.Errorf("Attachments = %d, want 0 on total failure", len(in.Attachments))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no inbound message published (text-bearing should degrade to text-only)")
	}
}
