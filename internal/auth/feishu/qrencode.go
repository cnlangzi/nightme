// Package feishu provides the Feishu (Lark) auth Provider for nightme.
//
// The Terminal QR renderer (RenderASCII) is also kept here so the
// feishu-only rendering logic does not leak into the parent package.
package feishu

import (
	"fmt"
	"io"

	"github.com/skip2/go-qrcode"
)

// qrcodeErrorLevel trades QR density (more modules) for scan
// reliability in noisy terminals. Medium is the skip2 default and
// is good enough for CLI use — no need to chase Low for a few extra
// rows of vertical space.
const qrcodeErrorLevel = qrcode.Medium

// RenderASCII encodes content as a QR code and writes it to w using
// the half-block Unicode characters ("▀", "▄", " ", "█") so each
// terminal row carries two pixel rows. The output is roughly 22
// rows tall regardless of payload size (cap is the skip2 default
// at medium error correction).
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
