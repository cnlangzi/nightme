// contentblocks_test.go — unit tests for the image / text / file
// content-block → dsh wire DTO conversion. Image roundtrip uses a
// tiny synthetic PNG written to t.TempDir() so the test is
// self-contained (no fixture files, no network).

package dsh

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestContentBlocksToDTO_TextOnly covers the simple text path.
// No file I/O — contentBlocksToDTO for text is pure mapping.
func TestContentBlocksToDTO_TextOnly(t *testing.T) {
	got, err := contentBlocksToDTO([]agent.ContentBlock{
		{Type: agent.ContentText, Text: "hello"},
		{Type: agent.ContentText, Text: "world"},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0]["type"] != "text" || got[0]["text"] != "hello" {
		t.Errorf("block[0] = %v, want text/hello", got[0])
	}
	if got[1]["type"] != "text" || got[1]["text"] != "world" {
		t.Errorf("block[1] = %v, want text/world", got[1])
	}
}

// TestContentBlocksToDTO_EmptySkipped covers: empty text blocks
// and empty-path blocks are dropped (same semantics as print-mode).
func TestContentBlocksToDTO_EmptySkipped(t *testing.T) {
	got, err := contentBlocksToDTO([]agent.ContentBlock{
		{Type: agent.ContentText, Text: ""},
		{Type: agent.ContentText, Text: "kept"},
		{Type: agent.ContentImage, Path: ""},
		{Type: agent.ContentFile, Path: ""},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0]["text"] != "kept" {
		t.Errorf("block[0] = %v, want text=kept", got[0])
	}
}

// TestContentBlocksToDTO_ImageInlineBase64 covers the real vision
// path: write a tiny PNG to disk, verify contentBlocksToDTO
// reads it and produces a wire DTO with base64 data + mediaType + name.
func TestContentBlocksToDTO_ImageInlineBase64(t *testing.T) {
	pngPath := writeTinyPNG(t)

	got, err := contentBlocksToDTO([]agent.ContentBlock{
		{Type: agent.ContentImage, Path: pngPath, MediaType: "image/png"},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	blk := got[0]
	if blk["type"] != "image" {
		t.Errorf("type = %v, want image", blk["type"])
	}
	if blk["mediaType"] != "image/png" {
		t.Errorf("mediaType = %v, want image/png", blk["mediaType"])
	}
	b64, ok := blk["data"].(string)
	if !ok {
		t.Fatalf("data not a string: %T", blk["data"])
	}
	// Round-trip: decode base64 should match file bytes.
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	original, _ := os.ReadFile(pngPath)
	if string(decoded) != string(original) {
		t.Errorf("decoded base64 != file bytes")
	}
	// name is set to basename (not full path)
	if blk["name"] != filepath.Base(pngPath) {
		t.Errorf("name = %v, want basename %q", blk["name"], filepath.Base(pngPath))
	}
}

// TestContentBlocksToDTO_ImageMixedText covers the user's exact
// ask: text + image in one payload. Verifies both blocks survive
// in order and the image is base64-encoded.
func TestContentBlocksToDTO_ImageMixedText(t *testing.T) {
	pngPath := writeTinyPNG(t)

	got, err := contentBlocksToDTO([]agent.ContentBlock{
		{Type: agent.ContentText, Text: "look at this"},
		{Type: agent.ContentImage, Path: pngPath, MediaType: "image/png"},
		{Type: agent.ContentText, Text: "and tell me the color"},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0]["type"] != "text" || got[0]["text"] != "look at this" {
		t.Errorf("block[0] = %v", got[0])
	}
	if got[1]["type"] != "image" || got[1]["mediaType"] != "image/png" {
		t.Errorf("block[1] = %v", got[1])
	}
	if got[2]["type"] != "text" || got[2]["text"] != "and tell me the color" {
		t.Errorf("block[2] = %v", got[2])
	}
}

// TestContentBlocksToDTO_UnsupportedImageMediaType falls back to
// a text annotation so the model still knows about the file (via
// bash/fs tools) even though we couldn't inline the bytes.
func TestContentBlocksToDTO_UnsupportedImageMediaType(t *testing.T) {
	pngPath := writeTinyPNG(t)
	got, err := contentBlocksToDTO([]agent.ContentBlock{
		{Type: agent.ContentImage, Path: pngPath, MediaType: "image/heic"},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0]["type"] != "text" {
		t.Errorf("expected text fallback for unsupported MIME, got %v", got[0])
	}
	if !strings.Contains(got[0]["text"].(string), "image/heic") {
		t.Errorf("annotation should mention MIME: %v", got[0])
	}
	if !strings.Contains(got[0]["text"].(string), "unsupported") {
		t.Errorf("annotation should mark unsupported: %v", got[0])
	}
}

// TestContentBlocksToDTO_FileBlock degrades to annotation.
func TestContentBlocksToDTO_FileBlock(t *testing.T) {
	got, err := contentBlocksToDTO([]agent.ContentBlock{
		{Type: agent.ContentFile, Path: "/tmp/data.json"},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got[0]["type"] != "text" {
		t.Errorf("type = %v, want text", got[0]["type"])
	}
	if !strings.Contains(got[0]["text"].(string), "[file: /tmp/data.json]") {
		t.Errorf("annotation wrong: %v", got[0])
	}
}

// TestContentBlocksToDTO_ReadFileError propagates I/O errors
// rather than silently dropping the image. The model needs to
// know that the file was unreadable.
func TestContentBlocksToDTO_ReadFileError(t *testing.T) {
	_, err := contentBlocksToDTO([]agent.ContentBlock{
		{Type: agent.ContentImage, Path: "/nonexistent/path/foo.png", MediaType: "image/png"},
	})
	if err == nil {
		t.Fatal("err = nil, want error for missing file")
	}
	if !strings.Contains(err.Error(), "/nonexistent/path/foo.png") {
		t.Errorf("err should mention path: %v", err)
	}
}

// TestIsSupportedImageMediaType pins the supported MIME set so a
// future dsh addition (e.g. image/heic) is a deliberate change,
// not an accidental drift.
func TestIsSupportedImageMediaType(t *testing.T) {
	for _, mt := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		if !isSupportedImageMediaType(mt) {
			t.Errorf("expected %q supported", mt)
		}
	}
	for _, mt := range []string{"image/heic", "image/svg+xml", "image/bmp", "application/octet-stream", ""} {
		if isSupportedImageMediaType(mt) {
			t.Errorf("expected %q unsupported", mt)
		}
	}
}

// writeTinyPNG writes a minimal 8x8 PNG to t.TempDir() and returns
// its path. Built at test-time via compress/zlib so the bytes
// are guaranteed to decode as a valid PNG (no hand-computed CRCs
// to drift). The picture is a 8x8 red square — enough for
// round-trip tests that don't care about the actual pixels.
func writeTinyPNG(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.png")

	// PNG signature.
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

	// IHDR chunk: 8x8, 8-bit depth, color type 2 (RGB),
	// compression 0, filter 0, interlace 0. Data length = 13 bytes.
	png = append(png, pngChunk("IHDR",
		[]byte{
			0x00, 0x00, 0x00, 0x08, // width
			0x00, 0x00, 0x00, 0x08, // height
			0x08,                   // bit depth
			0x02,                   // color type (RGB)
			0x00, 0x00, 0x00,       // compression / filter / interlace
		})...)

	// IDAT chunk: 8 rows × (1 filter byte + 8×3 RGB bytes).
	// All-red. compress/zlib handles deflate for us.
	var raw []byte
	for y := 0; y < 8; y++ {
		raw = append(raw, 0x00) // filter: None
		for x := 0; x < 8; x++ {
			raw = append(raw, 0xff, 0x00, 0x00)
		}
	}
	compressed := zlibCompress(t, raw)
	png = append(png, pngChunk("IDAT", compressed)...)

	// IEND chunk: empty data.
	png = append(png, pngChunk("IEND", nil)...)

	if err := os.WriteFile(path, png, 0o644); err != nil {
		t.Fatalf("write fixture png: %v", err)
	}
	return path
}

// pngChunk constructs one PNG chunk: 4-byte big-endian length,
// 4-byte type, `data`, 4-byte CRC32 over type+data. The PNG
// spec requires this exact layout.
func pngChunk(typ string, data []byte) []byte {
	out := make([]byte, 0, 8+len(data)+4)
	out = append(out,
		byte(len(data)>>24), byte(len(data)>>16),
		byte(len(data)>>8), byte(len(data)),
	)
	out = append(out, typ...)
	out = append(out, data...)
	crc := crc32IEEE(typ, data)
	out = append(out,
		byte(crc>>24), byte(crc>>16),
		byte(crc>>8), byte(crc),
	)
	return out
}

// crc32IEEE computes IEEE CRC32 over (type || data), matching
// the polynomial PNG uses (0xEDB88320 reversed). We don't import
// hash/crc32 because the table is small and we want zero
// dependencies for the fixture builder.
func crc32IEEE(typ string, data []byte) uint32 {
	crc := uint32(0xffffffff)
	update := func(b byte) {
		crc ^= uint32(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xedb88320
			} else {
				crc >>= 1
			}
		}
	}
	for i := 0; i < len(typ); i++ {
		update(typ[i])
	}
	for _, b := range data {
		update(b)
	}
	return crc ^ 0xffffffff
}

// zlibCompress wraps raw bytes in a zlib stream (deflate with the
// standard 2-byte header + adler32 checksum). This is what the
// PNG IDAT chunk expects.
func zlibCompress(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}
