//go:build windows

package feishu

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/skip2/go-qrcode"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// pngSizePx is the pixel side length for the saved QR PNG. 512 is
// large enough that phone cameras lock on instantly even at arm's
// length, and small enough to fit any screen real-estate when the
// user opens it in a photo viewer.
const pngSizePx = 512

// darkR/G/B and lightR/G/B define the two colors used by the ANSI
// renderer. They are pure black and pure white so the QR has the
// maximum contrast scanners expect. Tinting (e.g. light grey) hurts
// scan reliability and is not worth the cosmetic difference.
const (
	darkR, darkG, darkB     = 0, 0, 0
	lightR, lightG, lightB = 255, 255, 255
)

// ansiReset clears all SGR attributes so subsequent unrelated
// stdout from the program isn't painted with the QR's last
// background color.
const ansiReset = "\x1b[0m"

// ansiFgRGB and ansiBgRGB are the 24-bit "true color" SGR escape
// sequences used by RenderANSI.
const (
	ansiFgRGB = "\x1b[38;2;%d;%d;%dm"
	ansiBgRGB = "\x1b[48;2;%d;%d;%dm"
)

// RenderASCII encodes content as a QR code and writes it to w using
// the half-block Unicode characters ("▀", "▄", " ", "█") so each
// terminal row carries two source-module rows. This is the same
// shape as the Unix renderer in qrencode_unix.go — same function
// name, same signature, same body — and is duplicated here so
// provider_windows.go can dispatch to it on legacy conhost
// (cmd.exe / Windows PowerShell) without coupling to the
// !windows-only build.
//
// On Windows we only reach this function after enableConsoleOutputUTF8
// (in provider_windows.go) has set the console output code page to
// 65001. With the code page right, the half-block glyphs decode
// correctly on Win10+ hosts whose terminal font includes them
// (Consolas and Cascadia Code both ship U+2580/U+2584/U+2588
// shapes). On Win7 / fonts that lack the glyphs, the QR still
// won't scan — provider_windows.go falls back to WritePNGToDesktop
// for that case.
//
// inverseColor=false uses the standard "dark on light" mapping;
// pass true if your terminal has a light background.
func RenderASCII(content string, w io.Writer, inverseColor bool) error {
	if content == "" {
		return fmt.Errorf("feishu: cannot render QR for empty content")
	}
	q, err := qrcode.New(content, qrcodeErrorLevel)
	if err != nil {
		return fmt.Errorf("feishu: encode qr: %w", err)
	}
	// ToSmallString uses half-block characters; without it each row
	// is 2 terminal lines which makes a 30-module QR noticeably tall.
	if _, err := io.WriteString(w, q.ToSmallString(inverseColor)); err != nil {
		return fmt.Errorf("feishu: write qr: %w", err)
	}
	return nil
}

// RenderANSI encodes content as a QR code and writes it to w using
// 24-bit ANSI color escape codes combined with half-block Unicode
// characters. Each terminal row carries two source-module rows
// (matching skip2's half-block compression), so the rendered modules
// stay physically square in a 2:1 terminal cell.
//
// The output looks like the ASCII half-block renderer on Unix, but
// the module colors come from background/foreground escape codes
// rather than from the glyph itself. That makes it tolerant of
// fonts whose U+2580 / U+2584 / U+2588 glyphs are absent — as long
// as the terminal honors VT escape processing (Windows Terminal
// does; legacy conhost does not), the colors do all the work and
// the half-block chars just need to take up vertical space.
//
// For the typical Feishu auth URL at medium error correction this
// produces a 41-column × ~21-line grid — same shape as the Unix
// RenderASCII so a screenshot comparison across platforms shows
// the same physical size.
//
// Returns an error only if content is empty or QR encoding fails.
// Write errors to w are silently ignored — by the time they happen
// stdout is broken and there's nothing for the user to do.
func RenderANSI(content string, w io.Writer) error {
	if content == "" {
		return fmt.Errorf("feishu: cannot render QR for empty content")
	}
	q, err := qrcode.New(content, qrcodeErrorLevel)
	if err != nil {
		return fmt.Errorf("feishu: encode qr: %w", err)
	}
	bits := q.Bitmap() // [][]bool, true = dark module

	// Half-block compression: each terminal row represents two
	// source rows. We pair (y, y+1) for all y in [0, len-1).
	for y := 0; y < len(bits)-1; y += 2 {
		if _, err := io.WriteString(w, renderANSILine(bits[y], bits[y+1])); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	// Odd-height QR (rare — skip2 always emits odd-length bitmaps
	// for version 1 micro-QRs, but our URL doesn't trigger that
	// path): emit the last row as upper-half blocks with a light
	// background.
	if len(bits)%2 == 1 {
		y := len(bits) - 1
		if _, err := io.WriteString(w, renderANSISingleRow(bits[y])); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	return nil
}

// renderANSILine turns a pair of source rows into a single ANSI
// half-block terminal row. We build the whole row in a strings.Builder
// first so the terminal sees one Write per line — partial writes
// mid-row can leave the cursor mid-module on a slow pipe.
func renderANSILine(top, bot []bool) string {
	var b strings.Builder
	b.Grow(len(top) * 16) // rough estimate: each cell averages ~16 bytes
	for x := 0; x < len(top); x++ {
		switch {
		case top[x] && bot[x]:
			// Full block: only the background color matters; the
			// foreground is overridden by the background anyway.
			fmt.Fprintf(&b, ansiBgRGB+"\x20", darkR, darkG, darkB)
		case top[x] && !bot[x]:
			// Upper half: black foreground, white background.
			fmt.Fprintf(&b, ansiFgRGB+ansiBgRGB+"▀",
				darkR, darkG, darkB, lightR, lightG, lightB)
		case !top[x] && bot[x]:
			// Lower half: white foreground, black background.
			fmt.Fprintf(&b, ansiFgRGB+ansiBgRGB+"▄",
				lightR, lightG, lightB, darkR, darkG, darkB)
		default:
			// Empty: only the background color matters.
			fmt.Fprintf(&b, ansiBgRGB+"\x20", lightR, lightG, lightB)
		}
	}
	b.WriteString(ansiReset)
	return b.String()
}

// renderANSISingleRow handles the last source row when the QR has
// an odd number of rows. Each module is rendered as an upper-half
// block whose foreground reflects the source color and whose
// background is light. This isn't a standard QR (skip2 always emits
// even-length bitmaps for our payload sizes), but the helper keeps
// the renderer robust against future URL length changes.
func renderANSISingleRow(row []bool) string {
	var b strings.Builder
	b.Grow(len(row) * 16)
	for x := 0; x < len(row); x++ {
		if row[x] {
			fmt.Fprintf(&b, ansiFgRGB+ansiBgRGB+"▀",
				darkR, darkG, darkB, lightR, lightG, lightB)
		} else {
			fmt.Fprintf(&b, ansiBgRGB+"\x20", lightR, lightG, lightB)
		}
	}
	b.WriteString(ansiReset)
	return b.String()
}

// WritePNGToDesktop encodes content as a 512×512 PNG QR code and
// writes it to a freshly-created file in the user's Desktop folder.
// The Desktop is preferred over os.TempDir() because:
//   - The user has a visible icon they can click as a backup if the
//     auto-open fails.
//   - TempDir on Windows (%TEMP%) is a hidden AppData subfolder
//     that's hard to navigate to from File Explorer.
// If the Desktop folder can't be located or written (e.g. the user
// has a domain policy that redirects Desktop to a network share
// that's offline), we fall back to os.TempDir() — the user's QR is
// still produced, just in a less convenient place.
//
// The file is intentionally NOT deleted: login takes up to 10
// minutes to complete (user has to scan + confirm in the Feishu
// app). The OS handles long-term cleanup of Desktop / TempDir.
func WritePNGToDesktop(content string) (string, error) {
	if content == "" {
		return "", fmt.Errorf("feishu: cannot render QR for empty content")
	}
	q, err := qrcode.New(content, qrcodeErrorLevel)
	if err != nil {
		return "", fmt.Errorf("feishu: encode qr: %w", err)
	}
	// skip2's PNG(size) returns the raw QR bitmap at the requested
	// pixel size. We then add a caption band beneath it via
	// composeWithCaption so the saved image is self-explanatory —
	// the user knows what to do with it without needing the
	// terminal text alongside.
	qrPNG, err := q.PNG(pngSizePx)
	if err != nil {
		return "", fmt.Errorf("feishu: encode png: %w", err)
	}
	composed, err := composeWithCaption(qrPNG)
	if err != nil {
		return "", fmt.Errorf("feishu: compose caption: %w", err)
	}

	dir, err := desktopDir()
	if err != nil {
		// Fall back to TempDir if we can't locate Desktop. We don't
		// error out — the user can still scan whatever file we land
		// in, just with a less convenient path.
		dir = os.TempDir()
	}

	// Pattern includes "nightme-login-qr" so the file is greppable
	// when the user is hunting it down. We use the parent dir as
	// the first arg so the pattern doesn't accidentally land in
	// os.TempDir() when we explicitly chose Desktop.
	f, err := os.CreateTemp(dir, "nightme-login-qr-*.png")
	if err != nil {
		return "", fmt.Errorf("feishu: create file in %s: %w", dir, err)
	}
	defer f.Close()
	if _, err := f.Write(composed); err != nil {
		return "", fmt.Errorf("feishu: write png: %w", err)
	}
	return filepath.Abs(f.Name())
}

// captionText is the instruction line drawn beneath the QR in the
// saved PNG. Keeping it as a const (rather than a literal) means
// it shows up in the test assertions and grep, so a copy edit
// can't silently drop the user-facing reminder.
const captionText = "Scan this QR with the Feishu mobile app"

// captionBandHeight is the vertical space (in pixels) reserved
// below the QR for the caption. Sized to fit the 13-pixel
// basicfont.Face7x13 with comfortable top/bottom padding.
const captionBandHeight = 36

// composeWithCaption takes the raw QR PNG bytes from skip2 (a
// 512×512 RGBA image with a white background and black modules)
// and produces a new PNG that adds a caption band underneath. The
// total output is (pngSizePx)×(pngSizePx + captionBandHeight).
//
// We use golang.org/x/image/font/basicfont.Face7x13 — a built-in
// 7×13 bitmap font that ships in x/image, so no TTF file needs to
// be bundled. The caption text is drawn in black on white, centered
// horizontally. If the caption doesn't fit horizontally we still
// emit it (left-aligned, truncated visually) — losing a few
// characters at the right edge is better than failing to render
// the image at all.
//
// Returns the composed PNG bytes ready to be written to disk.
// Errors here are rare (only basicfont face issues), so a single
// wrapped error is sufficient.
func composeWithCaption(qrPNG []byte) ([]byte, error) {
	srcImg, _, err := image.Decode(bytes.NewReader(qrPNG))
	if err != nil {
		return nil, fmt.Errorf("decode qr png: %w", err)
	}

	w := srcImg.Bounds().Dx()
	h := srcImg.Bounds().Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h+captionBandHeight))

	// White background — the caption band otherwise inherits
	// whatever default the new RGBA() image has (transparent),
	// which renders as black on most photo viewers and makes the
	// caption text invisible against its own dark background.
	draw.Draw(out, out.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	// Copy the QR into the top band.
	draw.Draw(out, srcImg.Bounds(), srcImg, srcImg.Bounds().Min, draw.Src)

	// Caption text — centered horizontally within the QR width,
	// vertically positioned in the caption band. basicfont.Face7x13
	// is 13 pixels tall; we offset by 8px from the top of the band
	// for vertical centering (13 / 2 ≈ 6, rounded up to 8 for a
	// hair more breathing room).
	face := basicfont.Face7x13
	charW := 7 // basicfont.Face7x13 advance per glyph
	textW := charW * len(captionText)
	x0 := (w - textW) / 2
	if x0 < 0 {
		// Caption longer than image is wide — left-align rather
		// than fail.
		x0 = 0
	}
	y0 := h + 8 + 13 // baseline ≈ 8 from band top + 13 cap height

	d := &font.Drawer{
		Dst:  out,
		Src:  image.NewUniform(color.Black),
		Face: face,
		Dot:  fixed.P(x0, y0),
	}
	d.DrawString(captionText)

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("encode composed png: %w", err)
	}
	return buf.Bytes(), nil
}

// desktopDir returns the absolute path of the current user's Desktop
// folder. Order of preference:
//   1. %USERPROFILE%\Desktop (the canonical Windows location)
//   2. $HOME/Desktop (defensive fallback for non-standard setups)
// We do not consult the registry's "User Shell Folders" because the
// environment variables are set at logon by Explorer itself and
// reflect any user-initiated Desktop relocation.
func desktopDir() (string, error) {
	if up := os.Getenv("USERPROFILE"); up != "" {
		return filepath.Join(up, "Desktop"), nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "Desktop"), nil
	}
	return "", fmt.Errorf("feishu: cannot locate Desktop directory")
}