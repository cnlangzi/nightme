//go:build !windows

package feishu

import (
	"bytes"
	"strings"
	"testing"
)

// TestQrencode_Renders feeds an example URL through the renderer
// and checks the output is non-empty, multi-line, carries the
// half-block glyphs, and is roughly square (line count ≈ half the
// column count). We deliberately do NOT check pixel-perfect output
// (skip2 owns the matrix); we just confirm a QR appears at full
// resolution — no downsampling — so the mobile scanner can lock on.
//
// RenderASCII is the Unix/macOS renderer — Windows users take the
// RenderANSI or WritePNGToTemp paths (qrencode_windows.go) and
// never hit this function. Build tags ensure this test file is only
// compiled on non-Windows.
func TestQrencode_Renders(t *testing.T) {
	const url = "https://accounts.feishu.cn/oauth2/v1/app/registration?from=sdk"

	var buf bytes.Buffer
	if err := RenderASCII(url, &buf, false); err != nil {
		t.Fatalf("RenderASCII: %v", err)
	}
	out := buf.String()
	if out == "" {
		t.Fatal("RenderASCII produced empty output")
	}
	// Glyph alphabet: full block, upper half, lower half, space.
	// We don't assert which combination appears — that depends on
	// the QR bit pattern. Just confirm we used block characters.
	if !strings.ContainsAny(out, " █▀▄") {
		t.Errorf("output contains no half-block glyphs:\n%s", out)
	}
	if !strings.Contains(out, "\n") {
		t.Errorf("output contains no newline (expected multi-line QR):\n%s", out)
	}
	// Every non-empty line should be the same width — the renderer
	// walks a half-block grid where each line is one source row's
	// worth of columns. We measure in runes (not bytes) because
	// "█"/"▀"/"▄" are 3 bytes each in UTF-8.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("output has no lines after trimming")
	}
	width := len([]rune(lines[0]))
	for i, l := range lines {
		if w := len([]rune(l)); w != width {
			t.Errorf("line %d width=%d, want %d (renderer should emit uniform-width lines):\n%s", i, w, width, out)
		}
	}
	// Full-resolution: source QR is ~41 modules wide at medium ECC,
	// so the output should be roughly that wide. The exact number
	// depends on skip2's QR-version selection for the payload; we
	// accept 25–65 so future payload changes don't snap the test.
	if width < 25 || width > 65 {
		t.Errorf("QR width=%d, want ~41 (full-resolution, no downsampling):\n%s", width, out)
	}
	// Half-block compression: line count should be roughly half the
	// column count (rounded up). This is what makes the rendered
	// output visually square despite the 2:1 terminal cell aspect.
	halfHeight := (width + 1) / 2
	if len(lines) > halfHeight+1 || len(lines) < halfHeight-1 {
		t.Errorf("QR line count=%d, want ~%d (half of width %d via half-block compression):\n%s",
			len(lines), halfHeight, width, out)
	}
}

// TestQrencode_EmptyInput guards against accidentally encoding an
// empty string — skip2 returns "no data" which we want surfaced as
// a meaningful error rather than silently emitted.
func TestQrencode_EmptyInput(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderASCII("", &buf, false); err == nil {
		t.Error("RenderASCII(\"\") = nil, want error")
	}
}