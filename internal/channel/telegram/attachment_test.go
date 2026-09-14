package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestAdapter_HandleMessage_Photo_DownloadsAndPublishesAttachment exercises
// the full inbound photo path: a Telegram update carrying a Photo[]
// triggers downloadTelegramFile (getFile) + api.download, the bytes land
// at <dataDir>/telegram/<chatID>/<messageID>/, and the published
// InboundMessage carries one Attachment with LocalPath + MimeType +
// Type:"image" plus a Block of Type ContentImage so the agent can read
// the image.
func TestAdapter_HandleMessage_Photo_DownloadsAndPublishesAttachment(t *testing.T) {
	a, api := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	api.FileBytes = []byte("\xff\xd8\xff\xe0fake-jpeg-bytes")

	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 42,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 100, Type: "private"},
			From:      &User{ID: 7, FirstName: "Devin"},
			Photo: []PhotoSize{
				{FileID: "thumb-id", Width: 90, Height: 60},
				{FileID: "full-id", Width: 800, Height: 600},
			},
			Caption: "what color is this?",
		},
	})

	select {
	case got := <-a.Incoming():
		if len(got.Attachments) != 1 {
			t.Fatalf("attachments = %d, want 1", len(got.Attachments))
		}
		att := got.Attachments[0]
		if att.LocalPath == "" {
			t.Fatal("attachment LocalPath empty")
		}
		if _, err := os.Stat(att.LocalPath); err != nil {
			t.Fatalf("downloaded file missing: %v", err)
		}
		if att.MimeType != "image/jpeg" {
			t.Fatalf("attachment MimeType = %q, want image/jpeg", att.MimeType)
		}
		if att.FileKey != "full-id" {
			t.Fatalf("attachment FileKey = %q, want full-id (largest photo)", att.FileKey)
		}
		if att.Type != "image" {
			t.Fatalf("attachment Type = %q, want image (feishu-vocab parity for manager fallback)", att.Type)
		}
		if got.Text != "what color is this?" {
			t.Fatalf("Text = %q, want caption", got.Text)
		}
		if len(got.Blocks) != 2 {
			t.Fatalf("blocks = %d, want 2 (text + image)", len(got.Blocks))
		}
		if got.Blocks[0].Type != agent.ContentText || got.Blocks[0].Text != "what color is this?" {
			t.Fatalf("block 0 = %+v, want text caption", got.Blocks[0])
		}
		if got.Blocks[1].Type != agent.ContentImage {
			t.Fatalf("block 1 = %+v, want ContentImage", got.Blocks[1])
		}
		if got.Blocks[1].Path != att.LocalPath {
			t.Fatalf("block 1.Path = %q, want %q", got.Blocks[1].Path, att.LocalPath)
		}
		if got.Blocks[1].MediaType != "image/jpeg" {
			t.Fatalf("block 1.MediaType = %q, want image/jpeg", got.Blocks[1].MediaType)
		}
		// file should live under <dataDir>/telegram/<chatID>/<messageID>/
		wantDir := filepath.Join(a.dataDir, "telegram", "100", "42")
		if filepath.Dir(att.LocalPath) != wantDir {
			t.Fatalf("attachment dir = %q, want %q", filepath.Dir(att.LocalPath), wantDir)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}

	// Confirm getFile was called with the largest photo's FileID.
	calls := api.snapshotCalls()
	var getFileCall *fakeCall
	for i := range calls {
		if calls[i].Method == "getFile" {
			getFileCall = &calls[i]
			break
		}
	}
	if getFileCall == nil {
		t.Fatal("expected getFile call")
	}
	if getFileCall.Params["file_id"] != "full-id" {
		t.Fatalf("getFile file_id = %v, want full-id", getFileCall.Params["file_id"])
	}
}

// TestAdapter_HandleMessage_Document_PNG verifies that documents with
// an image MIME (e.g. user sends a PNG file) also flow into the agent
// as ContentImage, not ContentFile.
func TestAdapter_HandleMessage_Document_PNG(t *testing.T) {
	a, api := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	api.FileBytes = []byte("\x89PNG\r\n\x1a\nfake-png-bytes")

	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 50,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 200, Type: "private"},
			From:      &User{ID: 7},
			Document: &Document{
				FileID:   "doc-png-id",
				FileName: "blue.png",
				MimeType: "image/png",
			},
		},
	})

	select {
	case got := <-a.Incoming():
		if len(got.Attachments) != 1 {
			t.Fatalf("attachments = %d, want 1", len(got.Attachments))
		}
		att := got.Attachments[0]
		if att.LocalPath == "" {
			t.Fatal("attachment LocalPath empty")
		}
		if att.MimeType != "image/png" {
			t.Fatalf("attachment MimeType = %q, want image/png", att.MimeType)
		}
		if att.Type != "image" {
			t.Fatalf("attachment Type = %q, want image (image/* MIME → image category)", att.Type)
		}
		if filepath.Base(att.LocalPath) != "blue.png" {
			t.Fatalf("attachment name = %q, want blue.png", filepath.Base(att.LocalPath))
		}
		// BuildBlocks should classify image/png MIME as ContentImage.
		var imageBlock *agent.ContentBlock
		for i := range got.Blocks {
			if got.Blocks[i].Type == agent.ContentImage {
				imageBlock = &got.Blocks[i]
				break
			}
		}
		if imageBlock == nil {
			t.Fatalf("no ContentImage block in %+v", got.Blocks)
		}
		if imageBlock.MediaType != "image/png" {
			t.Fatalf("image block MediaType = %q, want image/png", imageBlock.MediaType)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}
}

// TestAdapter_HandleMessage_Photo_DownloadFailure_PureImageDropped
// covers the AllFailed + pure-image path: photo-only message, all 3
// retry attempts fail, the inbound is dropped, and a "please retry"
// notification goes out via the outbound channel. Without this path
// the user would watch the bot produce nothing — no reply, no
// reaction, no clue that their image was lost.
func TestAdapter_HandleMessage_Photo_DownloadFailure_PureImageDropped(t *testing.T) {
	a, api := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Shrink the retry backoffs so the test runs in <1s instead of
	// the production 0s/5s/15s ladder.
	origBackoffs := downloadRetryConfig.Backoffs
	downloadRetryConfig.Backoffs = []time.Duration{0, 0, 0}
	t.Cleanup(func() { downloadRetryConfig.Backoffs = origBackoffs })
	// Fail only getFile across all 3 retry attempts. Other API
	// calls (sendRichMessage for the placeholder + notify, plus
	// pollLoop's getUpdates) must succeed so the recorded call
	// log captures the user-facing notification text.
	api.MethodErrors = map[string]error{
		"getFile": errFakeDownloadFailed,
	}

	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 60,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 300, Type: "private"},
			From:      &User{ID: 7},
			Photo: []PhotoSize{
				{FileID: "broken-id", Width: 800, Height: 600},
			},
		},
	})

	// Pure-image (no caption) AllFailed → inbound must be dropped
	// entirely. The "please retry" reply goes through the outbound
	// path; we wait briefly for it before asserting nothing landed
	// on incoming.
	select {
	case got := <-a.Incoming():
		t.Fatalf("pure-image AllFailed must drop inbound; got %+v", got)
	case <-time.After(2 * time.Second):
		// expected: nothing published
	}

	// And the notifyDownloadFailure reply must eventually hit
	// sendRichMessage / editMessageText. The 250ms-debounced rich
	// turn flush emits editMessageText asynchronously, so we poll.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if body := findRichMessageBody(api.snapshotCalls()); strings.Contains(body, "please retry") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("\"please retry\" notification never landed within 2s; recorded methods: %v",
		callMethodNames(api.snapshotCalls()))
}

// TestAdapter_HandleMessage_Photo_DownloadFailure_TextBearingDegrades
// covers the AllFailed + text-bearing path: the message has a
// caption, so we degrade to text-only (drop the failed attachment)
// and warn the user that N attachments failed.
func TestAdapter_HandleMessage_Photo_DownloadFailure_TextBearingDegrades(t *testing.T) {
	a, api := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Shrink the retry backoffs (see PureImageDropped rationale).
	origBackoffs := downloadRetryConfig.Backoffs
	downloadRetryConfig.Backoffs = []time.Duration{0, 0, 0}
	t.Cleanup(func() { downloadRetryConfig.Backoffs = origBackoffs })
	// Fail only getFile — see PureImageDropped rationale.
	api.MethodErrors = map[string]error{
		"getFile": errFakeDownloadFailed,
	}

	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 61,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 350, Type: "private"},
			From:      &User{ID: 7},
			Photo: []PhotoSize{
				{FileID: "broken-id", Width: 800, Height: 600},
			},
			Caption: "what is this?",
		},
	})

	select {
	case got := <-a.Incoming():
		if got.Text != "what is this?" {
			t.Fatalf("Text = %q, want caption preserved", got.Text)
		}
		if len(got.Attachments) != 0 {
			t.Fatalf("attachments = %d, want 0 (degraded to text-only)", len(got.Attachments))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}

	// Wait for the 250ms-debounced rich turn flush to land before
	// snapshotting. The notifyDownloadFailure reply goes through
	// appendRichTurn → scheduleRichTurnFlush → flushRichTurn which
	// emits editMessageText asynchronously.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if body := findRichMessageBody(api.snapshotCalls()); strings.Contains(body, "sending text only") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("\"sending text only\" notification never landed within 2s; recorded methods: %v",
		callMethodNames(api.snapshotCalls()))
}

// findRichMessageBody extracts the rich turn payload from the most
// recent sendRichMessage cold-create call. The notification
// OutError → appendRichTurn path uses sendRichMessage for the
// cold-create (turn.messageID == 0) and editMessageText for the
// 250ms-debounced flush that carries the actual error text.
//
// Returns "" if the body hasn't been emitted yet — callers in a
// poll loop should keep retrying rather than fatal immediately.
func findRichMessageBody(calls []fakeCall) string {
	for _, want := range []string{"please retry", "sending text only"} {
		for i := range calls {
			body := stringifyRichMessage(calls[i].Params)
			if strings.Contains(body, want) {
				return body
			}
		}
	}
	return ""
}

// stringifyRichMessage flattens the rich_message param (which may
// be a JSON string, a map[string]any, or absent) into a single
// string for substring matching.
func stringifyRichMessage(params map[string]any) string {
	v, ok := params["rich_message"]
	if !ok {
		// editMessageText passes the body as raw "text" (markdown).
		// fall back to that key.
		if text, ok := params["text"].(string); ok {
			return text
		}
		return ""
	}
	switch typed := v.(type) {
	case string:
		return typed
	case map[string]any:
		b, _ := json.Marshal(typed)
		return string(b)
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func callMethodNames(calls []fakeCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Method
	}
	return out
}

// TestAdapter_HandleMessage_Photo_RetryRecovers covers the F-61
// outer-ladder happy path: the first attempt fails (transport
// blip), the second attempt succeeds. The user should see the
// image with no failure notification.
func TestAdapter_HandleMessage_Photo_RetryRecovers(t *testing.T) {
	a, api := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	api.FileBytes = []byte("\xff\xd8\xff\xe0fake-jpeg-bytes")
	// Shrink backoffs (production is 0s/5s/15s) so the test
	// completes in <1s.
	origBackoffs := downloadRetryConfig.Backoffs
	downloadRetryConfig.Backoffs = []time.Duration{0, 0, 0}
	t.Cleanup(func() { downloadRetryConfig.Backoffs = origBackoffs })
	// Use a custom API wrapper so only the api.download path
	// fails on the first attempt and recovers on retry. The
	// Errors queue can't do this — pollLoop's concurrent
	// getUpdates would race the test for that queue and silently
	// steal the failure, turning the "retry recovered" assertion
	// meaningless.
	wrapped := &flakyDownloadAPI{inner: api, failFirst: true}
	a.api = wrapped

	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 62,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 360, Type: "private"},
			From:      &User{ID: 7},
			Photo: []PhotoSize{
				{FileID: "flaky-id", Width: 800, Height: 600},
			},
			Caption: "retry me",
		},
	})

	select {
	case got := <-a.Incoming():
		if len(got.Attachments) != 1 {
			t.Fatalf("attachments = %d, want 1 (retry recovered)", len(got.Attachments))
		}
		if got.Attachments[0].LocalPath == "" {
			t.Fatal("attachment LocalPath empty; retry should have recovered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}

	if wrapped.calls < 2 {
		t.Fatalf("download was called %d times, want >= 2 (retry path exercised)", wrapped.calls)
	}
}

// flakyDownloadAPI wraps apiClient and fails the first download
// call. All other calls pass through. Used by RetryRecovers to
// exercise the F-61 outer ladder without poisoning other API
// methods via the Errors queue (pollLoop races would otherwise
// swallow the failure).
type flakyDownloadAPI struct {
	inner     *fakeAPI
	failFirst bool
	calls     int
}

func (f *flakyDownloadAPI) call(ctx context.Context, method string, params map[string]any, result any) error {
	return f.inner.call(ctx, method, params, result)
}

func (f *flakyDownloadAPI) download(ctx context.Context, filePath string) ([]byte, error) {
	f.calls++
	if f.failFirst {
		f.failFirst = false
		return nil, errFakeDownloadFailed
	}
	return f.inner.download(ctx, filePath)
}

// TestAdapter_HandleMessage_Document_PDF verifies that documents with
// non-image MIME (e.g. user sends a PDF) get Type:"file" so the bridge
// sees a ContentFile block, not a ContentImage block.
func TestAdapter_HandleMessage_Document_PDF(t *testing.T) {
	a, api := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	api.FileBytes = []byte("%PDF-1.4 fake bytes")

	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 70,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 400, Type: "private"},
			From:      &User{ID: 7},
			Document: &Document{
				FileID:   "doc-pdf-id",
				FileName: "report.pdf",
				MimeType: "application/pdf",
			},
		},
	})

	select {
	case got := <-a.Incoming():
		if len(got.Attachments) != 1 {
			t.Fatalf("attachments = %d, want 1", len(got.Attachments))
		}
		att := got.Attachments[0]
		if att.Type != "file" {
			t.Fatalf("attachment Type = %q, want file for PDF", att.Type)
		}
		if att.MimeType != "application/pdf" {
			t.Fatalf("attachment MimeType = %q, want application/pdf", att.MimeType)
		}
		// BuildBlocks should classify application/pdf as ContentFile.
		var fileBlock *agent.ContentBlock
		for i := range got.Blocks {
			if got.Blocks[i].Type == agent.ContentFile {
				fileBlock = &got.Blocks[i]
				break
			}
		}
		if fileBlock == nil {
			t.Fatalf("no ContentFile block in %+v", got.Blocks)
		}
		if fileBlock.MediaType != "application/pdf" {
			t.Fatalf("file block MediaType = %q, want application/pdf", fileBlock.MediaType)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}
}

// errFakeDownloadFailed is a sentinel used by
// TestAdapter_HandleMessage_Photo_DownloadFailure to make fakeAPI
// return an error on the second call (the api.download for the
// photo body). The first call (getFile) succeeds.
var errFakeDownloadFailed = &fakeDownloadErr{msg: "simulated download failure"}

type fakeDownloadErr struct{ msg string }

func (e *fakeDownloadErr) Error() string { return e.msg }

// TestTelegramAttachmentType covers the MIME-based classifier used by
// the Document attach site. Photos / Voice / Audio / Video pass their
// own Type directly; only Document routes through this helper.
//
// Includes case-insensitive variants — the helper lower-cases the
// input so RFC-allowed upper-case MIME strings still map to the
// right Type. Without case folding, "Image/PNG" → "file" → agent
// receives a non-multimodal ContentFile block + Anthropic API
// rejects the wire format.
func TestTelegramAttachmentType(t *testing.T) {
	cases := []struct {
		mime string
		want string
	}{
		{"image/png", "image"},
		{"image/jpeg", "image"},
		{"image/webp", "image"},
		{"Image/PNG", "image"},  // mixed case
		{"IMAGE/JPEG", "image"}, // uppercase
		{"audio/mpeg", "audio"},
		{"Audio/Ogg", "audio"}, // mixed case
		{"video/mp4", "media"},
		{"VIDEO/MP4", "media"},
		{"application/pdf", "file"},
		{"Application/PDF", "file"},
		{"application/zip", "file"},
		{"text/plain", "file"},
	}
	for _, c := range cases {
		if got := telegramAttachmentType(c.mime); got != c.want {
			t.Errorf("telegramAttachmentType(%q) = %q, want %q", c.mime, got, c.want)
		}
	}
}
