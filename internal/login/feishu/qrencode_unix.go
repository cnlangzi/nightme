//go:build !windows

package feishu

import (
	"fmt"
	"io"

	"github.com/skip2/go-qrcode"
)

// RenderASCII encodes content as a QR code and writes it to w using
// the half-block Unicode characters ("▀", "▄", " ", "█") so each
// terminal row carries two source-module rows. Each output column
// maps to exactly one source module, so the rendered modules stay
// physically square (the terminal cell is roughly 2:1 tall, and the
// half-block halves vertical extent, giving a 1:1 visual aspect).
//
// No downsampling is applied: the QR's module fidelity is what
// makes it scannable. For the typical Feishu auth URL at medium
// error correction this produces a 41-column × ~21-line grid.
//
// This is the default renderer on macOS, Linux, and *BSD. It is NOT
// compiled in for Windows — see qrencode_windows.go for the
// platform-specific renderer that picks between 24-bit ANSI color
// (Windows Terminal) and a PNG file (legacy conhost).
//
// inverseColor=false uses the standard "dark on light" mapping
// (matches what most terminals render correctly); pass true if your
// terminal has a light background.
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