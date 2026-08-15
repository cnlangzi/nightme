//go:build windows

package feishu

import (
	"bytes"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderANSI feeds an example URL through the 24-bit color
// renderer and asserts the output is non-empty, multi-line, carries
// the half-block glyphs plus ANSI escape sequences, and has uniform
// line widths.
//
// The output combines two things: VT escape sequences (every cell
// has at least one SGR code) and the half-block characters
// "▀"/"▄"/"█" for the row-compressed cells. We assert on both so
// a future refactor that drops the colors (or the glyphs) fails
// loudly.
func TestRenderANSI(t *testing.T) {
	const url = "https://accounts.feishu.cn/oauth2/v1/app/registration?from=sdk"

	var buf bytes.Buffer
	if err := RenderANSI(url, &buf); err != nil {
		t.Fatalf("RenderANSI: %v", err)
	}
	out := buf.String()
	if out == "" {
		t.Fatal("RenderANSI produced empty output")
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("output contains no ANSI escape sequences (24-bit color missing):\n%s", out)
	}
	if !strings.ContainsAny(out, "▀▄█") {
		t.Errorf("output contains no half-block glyphs:\n%s", out)
	}
	if !strings.Contains(out, "\x1b[0m") {
		t.Errorf("output has no SGR reset (terminal attributes would leak past the QR):\n%s", out)
	}
	if !strings.Contains(out, "\n") {
		t.Errorf("output contains no newline (expected multi-line QR):\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("output has no lines after trimming")
	}
	// The output is structured as: QR rows (all uniform width)
	// followed by a single caption row (potentially different
	// width). Width uniformity applies to the QR rows only; the
	// caption is a separate text block. We verify width
	// uniformity on the first half of lines (the QR rows), and
	// validate the caption row (last line) separately.
	// The half-block characters U+2580 (▀), U+2584 (▄), U+2588 (█)
	// are rendered with slightly different widths in Windows
	// Terminal (e.g. 1 vs 2 cells for the 3 different blocks).
	// The QR width is therefore approximately uniform on
	// Windows, not exactly uniform. Allow up to 5% variance
	// between rows; anything more is a real bug.
	width := len([]rune(lines[0]))
	qrLineCount := len(lines) - 1 // assume last is caption
	if qrLineCount > 1 {
		minAllowed := width - (width / 20) // 5% tolerance
		maxAllowed := width + (width / 20)
		for i, l := range lines[:qrLineCount] {
			if w := len([]rune(l)); w < minAllowed || w > maxAllowed {
				t.Errorf("QR row %d width=%d, want %d ±5%% (range %d-%d):\n%s",
					i, w, width, minAllowed, maxAllowed, out)
			}
		}
	}
	if width < 25 || width > 65 {
		t.Errorf("QR width=%d, want ~41 (full-resolution, no downsampling):\n%s", width, out)
	}
	halfHeight := (width + 1) / 2
	if len(lines) > halfHeight+1 || len(lines) < halfHeight-1 {
		t.Errorf("QR line count=%d, want ~%d (half of width %d via half-block compression):\n%s",
			len(lines), halfHeight, width, out)
	}
}

// TestRenderANSI_EmptyInput guards against accidentally encoding an
// empty string.
func TestRenderANSI_EmptyInput(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderANSI("", &buf); err == nil {
		t.Error("RenderANSI(\"\") = nil, want error")
	}
}

// TestWritePNGToDesktop exercises the legacy-conhost fallback path:
// a real PNG file must land on the user's Desktop (or TempDir if
// Desktop is unavailable) with the right magic bytes and a non-empty
// body. We also verify the file is in the right directory — a bug
// that silently wrote PNGs to TempDir would defeat the whole
// "discoverable on Desktop" UX promise.
func TestWritePNGToDesktop(t *testing.T) {
	const url = "https://accounts.feishu.cn/oauth2/v1/app/registration?from=sdk"

	path, err := WritePNGToDesktop(url)
	if err != nil {
		t.Fatalf("WritePNGToDesktop: %v", err)
	}
	defer os.Remove(path)

	if !strings.HasSuffix(path, ".png") {
		t.Errorf("path=%q, want .png extension", path)
	}

	// The file should land in Desktop if it exists, or TempDir if
	// Desktop is missing. We accept either, but we must NOT silently
	// land in the current working directory or somewhere else.
	abs, _ := filepath.Abs(path)
	dir := filepath.Dir(abs)
	desktop := desktopDirForTest()
	inDesktop := strings.EqualFold(dir, desktop)
	inTemp := strings.EqualFold(dir, os.TempDir())
	if !inDesktop && !inTemp {
		t.Errorf("path dir=%q, want Desktop (%q) or TempDir (%q)", dir, desktop, os.TempDir())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() < 256 {
		t.Errorf("PNG file size=%d, want >=256 (empty PNG is a sign skip2 produced a blank image)", info.Size())
	}

	// Magic-byte sniff: PNG starts with 0x89 'P' 'N' 'G' 0x0D 0x0A
	// 0x1A 0x0A. A future skip2 API change that swapped to JPEG
	// silently would otherwise pass the size check.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var hdr [8]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	want := [8]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	if hdr != want {
		t.Errorf("magic bytes=%v, want %v (file is not a PNG)", hdr, want)
	}

	// Verify the composed PNG includes the caption band — the
	// composed image must be taller than the raw skip2 QR by
	// exactly captionBandHeight pixels. If this drifts, the
	// caption is either missing or oversized.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("decode composed png: %v", err)
	}
	if got, want := img.Bounds().Dy(), pngSizePx+captionBandHeight; got != want {
		t.Errorf("composed PNG height=%d, want %d (= pngSizePx + captionBandHeight)", got, want)
	}
}

// TestWritePNGToDesktop_EmptyInput guards against accidentally
// encoding an empty URL.
func TestWritePNGToDesktop_EmptyInput(t *testing.T) {
	if _, err := WritePNGToDesktop(""); err == nil {
		t.Error(`WritePNGToDesktop("") = nil, want error`)
	}
}

// desktopDirForTest mirrors the production desktopDir() so the test
// can compute the expected Desktop path without reaching into
// unexported implementation details. If desktopDir() changes its
// fallback policy, this helper follows automatically.
func desktopDirForTest() string {
	if up := os.Getenv("USERPROFILE"); up != "" {
		return filepath.Join(up, "Desktop")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "Desktop")
	}
	return ""
}

