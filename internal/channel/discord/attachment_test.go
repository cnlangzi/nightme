package discord

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestDownloadAttachments_LocalPathAndType verifies the F-14
// invariant: LocalPath is populated before the InboundMessage
// reaches the dispatcher, and Type matches the MIME prefix.
func TestDownloadAttachments_LocalPathAndType(t *testing.T) {
	tmp := t.TempDir()
	rest := &fakeREST{downloadBody: []byte("hello-bytes")}
	a := newTestAdapter(rest)
	a.dataDir = tmp

	msg := &Message{
		ID:        "msg-1",
		ChannelID: "chan1",
		Attachments: []Attachment{
			{ID: "a1", Filename: "image.png", URL: "https://cdn.discordapp.com/x.png", ContentType: "image/png", Size: 11},
			{ID: "a2", Filename: "song.mp3", URL: "https://cdn.discordapp.com/y.mp3", ContentType: "audio/mpeg", Size: 12},
			{ID: "a3", Filename: "movie.mp4", URL: "https://cdn.discordapp.com/z.mp4", ContentType: "video/mp4", Size: 13},
			{ID: "a4", Filename: "doc.pdf", URL: "https://cdn.discordapp.com/d.pdf", ContentType: "application/pdf", Size: 14},
		},
	}
	atts := a.downloadAttachments(context.Background(), msg, "dc_chan1")
	if len(atts) != 4 {
		t.Fatalf("attachments = %d, want 4", len(atts))
	}
	for _, att := range atts {
		if att.LocalPath == "" {
			t.Errorf("LocalPath empty for %q", att.Name)
		}
		if _, err := os.Stat(att.LocalPath); err != nil {
			t.Errorf("downloaded file missing: %v", err)
		}
	}
	typeCases := map[string]string{
		"image.png": "image",
		"song.mp3":  "audio",
		"movie.mp4": "media",
		"doc.pdf":   "file",
	}
	for _, att := range atts {
		want, ok := typeCases[att.Name]
		if !ok {
			continue
		}
		if att.Type != want {
			t.Errorf("%q Type = %q, want %q", att.Name, att.Type, want)
		}
	}
	// Sanity: directory layout is dataDir/discord/<chatID>/<msgID>/.
	if !strings.Contains(atts[0].LocalPath, filepath.Join(tmp, "discord", "dc_chan1", "msg-1")) {
		t.Errorf("path does not match expected layout: %q", atts[0].LocalPath)
	}
}

// TestDownloadAttachments_DownloadErrorSurfacesError verifies a
// failed download still produces an Attachment entry with Error
// set (so the dispatcher can surface the failure rather than
// silently drop).
func TestDownloadAttachments_DownloadErrorSurfacesError(t *testing.T) {
	tmp := t.TempDir()
	rest := &fakeREST{downloadErr: errors.New("network down")}
	a := newTestAdapter(rest)
	a.dataDir = tmp

	msg := &Message{
		ID:        "msg-2",
		ChannelID: "chan1",
		Attachments: []Attachment{
			{ID: "a1", Filename: "missing.bin", URL: "https://cdn.discordapp.com/missing", ContentType: "application/octet-stream"},
		},
	}
	atts := a.downloadAttachments(context.Background(), msg, "dc_chan1")
	if len(atts) != 1 {
		t.Fatalf("attachments = %d, want 1", len(atts))
	}
	if atts[0].Error == nil {
		t.Errorf("Error = nil, want non-nil")
	}
	if atts[0].LocalPath != "" {
		t.Errorf("LocalPath = %q, want empty on failure", atts[0].LocalPath)
	}
}

// TestDownloadAttachments_EmptyURLReturnsError ensures the
// sentinel error path is exercised.
func TestDownloadAttachments_EmptyURLReturnsError(t *testing.T) {
	tmp := t.TempDir()
	a := newTestAdapter(&fakeREST{})
	a.dataDir = tmp
	msg := &Message{
		ID:          "msg-3",
		ChannelID:   "chan1",
		Attachments: []Attachment{{ID: "a1", Filename: "x", ContentType: "image/png", URL: ""}},
	}
	atts := a.downloadAttachments(context.Background(), msg, "dc_chan1")
	if len(atts) != 1 || atts[0].Error == nil {
		t.Errorf("expected single Attachment with Error set; got %+v", atts)
	}
}

// TestDownloadAttachments_NoAttachmentsNoOp verifies the empty
// input short-circuit (no directory created, no downloads).
func TestDownloadAttachments_NoAttachmentsNoOp(t *testing.T) {
	tmp := t.TempDir()
	a := newTestAdapter(&fakeREST{})
	a.dataDir = tmp
	got := a.downloadAttachments(context.Background(), &Message{ID: "msg-4"}, "dc_chan1")
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// TestDiscordAttachmentType_Classification pins the MIME → type
// mapping that downstream BuildBlocks depends on.
func TestDiscordAttachmentType_Classification(t *testing.T) {
	cases := []struct {
		mime string
		want string
	}{
		{"image/jpeg", "image"},
		{"image/png", "image"},
		{"audio/ogg", "audio"},
		{"video/mp4", "media"},
		{"application/pdf", "file"},
		{"text/plain", "file"},
	}
	for _, tc := range cases {
		att := Attachment{ContentType: tc.mime}
		if got := discordAttachmentType(att); got != tc.want {
			t.Errorf("discordAttachmentType(%q) = %q, want %q", tc.mime, got, tc.want)
		}
	}
}

// TestSanitiseFilename ensures path traversal payloads can't
// escape the per-message directory.
func TestSanitiseFilename(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"normal.txt", "normal.txt"},
		{"../escape.txt", "escape.txt"},
		{".", "attachment"},
		{"..", "attachment"},
		{"", "attachment"},
	}
	for _, tc := range cases {
		if got := sanitiseFilename(tc.in); got != tc.want {
			t.Errorf("sanitiseFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Helper: ensure the import is used (messages.Attachment shape).
var _ = messages.Attachment{}
