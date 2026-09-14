package telegram

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// writeSolidPNG creates a 100x100 PNG filled with the given color
// and writes it to <dir>/<name>. Returns the absolute path.
func writeSolidPNG(t *testing.T, dir, name string, c color.Color) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, c)
		}
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return path
}

// TestAdapter_HandleMessage_Photo_RealPNGBytesToBlock covers the
// Document(image/png) path — the e2e shape that exercises the
// MIME-preserving branch (Photo's envelope hard-codes image/jpeg
// regardless of bytes). A user sending "blue.png" via Telegram
// ends up here: the Document.FileName / MimeType carry through to
// the Attachment and the ContentImage block, so the bridge can
// base64-encode with the correct media_type.
//
// Earlier revisions of this test used Message.Photo with raw PNG
// bytes — that artificially mismatched MediaType (image/jpeg) vs
// payload (PNG); the bridge would still base64-encode OK in
// practice but the wire format lied about the bytes. The Document
// path keeps MediaType honest.
func TestAdapter_HandleMessage_Photo_RealPNGBytesToBlock(t *testing.T) {
	a, api := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Real PNG bytes (red 100x100) — must round-trip as PNG.
	realPNG := writeSolidPNG(t, t.TempDir(), "red.png", color.RGBA{R: 255, A: 255})
	pngBytes, err := os.ReadFile(realPNG)
	if err != nil {
		t.Fatalf("read real png: %v", err)
	}
	api.FileBytes = pngBytes

	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 99,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 500, Type: "private"},
			From:      &User{ID: 7},
			Document: &Document{
				FileID:   "real-png-id",
				FileName: "red.png",
				MimeType: "image/png",
			},
			Caption: "identify the color",
		},
	})

	var got messages.InboundMessage
	select {
	case got = <-a.Incoming():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}

	if len(got.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(got.Attachments))
	}
	att := got.Attachments[0]
	if att.Type != "image" {
		t.Fatalf("attachment Type = %q, want image", att.Type)
	}
	if att.MimeType != "image/png" {
		t.Fatalf("attachment MimeType = %q, want image/png (Document MIME preserved end-to-end)", att.MimeType)
	}

	// Saved bytes must round-trip as a valid image of the right
	// dimensions — same contract the claudecode bridge depends on
	// when it base64-encodes the file into the stream-json turn.
	saved, err := os.ReadFile(att.LocalPath)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if !bytes.Equal(saved, pngBytes) {
		t.Fatalf("saved bytes differ from fakeAPI payload")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(saved))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if format != "png" {
		t.Fatalf("image format = %q, want png", format)
	}
	if cfg.Width != 100 || cfg.Height != 100 {
		t.Fatalf("image dims = %dx%d, want 100x100", cfg.Width, cfg.Height)
	}

	// Find the ContentImage block.
	var imgBlock *agent.ContentBlock
	for i := range got.Blocks {
		if got.Blocks[i].Type == agent.ContentImage {
			imgBlock = &got.Blocks[i]
			break
		}
	}
	if imgBlock == nil {
		t.Fatalf("no ContentImage block in %+v", got.Blocks)
	}
	if imgBlock.Path != att.LocalPath {
		t.Fatalf("image block Path = %q, want %q", imgBlock.Path, att.LocalPath)
	}
	if imgBlock.MediaType != "image/png" {
		t.Fatalf("image block MediaType = %q, want image/png", imgBlock.MediaType)
	}
}

// TestAdapter_HandleMessage_SolidColor_BuildsContentImageArray is the
// end-to-end shape the claudecode bridge consumes: each block in
// got.Blocks must serialise into the Anthropic-API content-array
// format (text + image-with-base64-source). Asserting this locally
// avoids the cost of a real daemon + claudecode subprocess per
// iteration and proves the wire format is what the agent expects.
//
// Uses Document(image/png) rather than Photo so the MediaType on
// the wire is honest — Photo's envelope hard-codes image/jpeg even
// when the bytes are PNG.
func TestAdapter_HandleMessage_SolidColor_BuildsContentImageArray(t *testing.T) {
	a, api := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Solid blue PNG.
	pngPath := writeSolidPNG(t, t.TempDir(), "blue.png", color.RGBA{B: 255, A: 255})
	pngBytes, err := os.ReadFile(pngPath)
	if err != nil {
		t.Fatalf("read png: %v", err)
	}
	api.FileBytes = pngBytes

	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 100,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 600, Type: "private"},
			From:      &User{ID: 7},
			Document: &Document{
				FileID:   "blue-id",
				FileName: "blue.png",
				MimeType: "image/png",
			},
			Caption: "what color is this image?",
		},
	})

	var got messages.InboundMessage
	select {
	case got = <-a.Incoming():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}

	// Build the content-array the claudecode bridge would emit.
	content := buildAnthropicContentArray(t, got.Blocks)
	if len(content) != 2 {
		t.Fatalf("content-array len = %d, want 2 (text + image)", len(content))
	}
	if content[0]["type"] != "text" {
		t.Fatalf("content[0] type = %v, want text", content[0]["type"])
	}
	if content[0]["text"] != "what color is this image?" {
		t.Fatalf("content[0] text = %v", content[0]["text"])
	}
	if content[1]["type"] != "image" {
		t.Fatalf("content[1] type = %v, want image", content[1]["type"])
	}
	source, ok := content[1]["source"].(map[string]any)
	if !ok {
		t.Fatalf("content[1].source = %+v, want map", content[1]["source"])
	}
	if source["type"] != "base64" {
		t.Fatalf("source type = %v, want base64", source["type"])
	}
	if source["media_type"] != "image/png" {
		t.Fatalf("source media_type = %v, want image/png (Document MIME preserved)", source["media_type"])
	}
	data, ok := source["data"].(string)
	if !ok || data == "" {
		t.Fatalf("source data empty / wrong type")
	}
	// The base64-decoded payload must round-trip to a valid image
	// (the byte stream we sent through the fakeAPI).
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("decode source.data: %v", err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if format != "png" {
		t.Fatalf("image format = %q, want png", format)
	}
	if cfg.Width != 100 || cfg.Height != 100 {
		t.Fatalf("image dims = %dx%d, want 100x100", cfg.Width, cfg.Height)
	}

	// Sanity: the JSON marshal must produce a single user turn the
	// claudecode driver can flush to stdin.
	turn := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": content},
	}
	out, err := json.Marshal(turn)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"base64"`) || !strings.Contains(string(out), `"image/png"`) {
		t.Fatalf("serialised turn missing expected fields: %s", out)
	}
}

// buildAnthropicContentArray mirrors the exact encoding the
// claudecode bridge does in internal/bridge/claudecode/claudecode.go
// (SendBlocks). Kept here as a private helper so this test stays a
// pure self-check of the wire format — the bridge's own tests
// already cover the production code path.
//
// imageSizeLimit bytes caps an inline image (matches the
// claudecode.go 5 MiB cap).
const imageSizeLimit = 5 * 1024 * 1024

func buildAnthropicContentArray(t *testing.T, blocks []agent.ContentBlock) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			if b.Text == "" {
				continue
			}
			out = append(out, map[string]any{"type": "text", "text": b.Text})
		case agent.ContentImage:
			if b.Path == "" {
				continue
			}
			info, err := os.Stat(b.Path)
			if err != nil {
				t.Fatalf("stat %s: %v", b.Path, err)
			}
			if info.Size() > imageSizeLimit {
				out = append(out, map[string]any{"type": "text", "text": "Image too large to inline"})
				continue
			}
			data, err := os.ReadFile(b.Path)
			if err != nil {
				t.Fatalf("read %s: %v", b.Path, err)
			}
			encoded := base64.StdEncoding.EncodeToString(data)
			out = append(out, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": b.MediaType,
					"data":       encoded,
				},
			})
		default:
			t.Fatalf("unexpected block type %v in test fixture", b.Type)
		}
	}
	return out
}
