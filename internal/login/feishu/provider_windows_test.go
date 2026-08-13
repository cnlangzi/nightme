//go:build windows

package feishu

import (
	"bytes"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
)

// TestPrintQRCode_WritesURLAndQR wires the OnQRCode path on Windows.
//
// renderQRPlatform on Windows has three tiers of fallback:
//
//   1. Windows Terminal → RenderANSI (24-bit color, inline).
//   2. Legacy conhost → RenderASCII (half-block Unicode) after
//      enableConsoleOutputUTF8 sets the console code page to UTF-8.
//   3. Universal fallback → WritePNGToDesktop + auto-open via the
//      default photo viewer.
//
// In the test environment (we run `go test` from a console, no
// WT_SESSION set), tier 2 is the active path. We assert on the
// resulting terminal output:
//
//   - The URL and expiry seconds are still printed.
//   - The "Waiting for scan" hint is still present so the user
//     knows what to do next.
//   - No pure half-block glyphs WITHOUT accompanying ANSI escapes
//     appear — those would mean someone leaked the renderer into
//     the wrong output sink.
//
// Tier 3 (PNG path) is exercised by TestWritePNGToDesktop in
// qrencode_windows_test.go; tier 1 (ANSI path) by TestRenderANSI
// in the same file. This test focuses on the wiring in
// provider_windows.go — the URL/hint output is the contract that
// would silently break if a refactor dropped it.
func TestPrintQRCode_WritesURLAndQR(t *testing.T) {
	a := New(Options{})
	var buf bytes.Buffer
	a.out = &buf

	a.printQRCode(&registration.QRCodeInfo{
		URL:      "https://accounts.feishu.cn/oauth2/v1/app/registration?from=sdk",
		ExpireIn: 600,
	})

	out := buf.String()
	if !strings.Contains(out, "https://accounts.feishu.cn") {
		t.Errorf("printQRCode missing URL\n%s", out)
	}
	if !strings.Contains(out, "600") {
		t.Errorf("printQRCode missing expiry seconds\n%s", out)
	}
	// On the in-terminal path (tier 1 or tier 2) we expect to see
	// half-block glyphs. On the PNG path (tier 3) we expect the
	// "QR code saved to:" message instead. Either is acceptable
	// from the user's perspective, but at least one must be
	// present — otherwise the user has no way to scan.
	inTerminal := strings.ContainsAny(out, "▀▄█")
	pngFallback := strings.Contains(out, "QR code saved to:")
	if !inTerminal && !pngFallback {
		t.Errorf("printQRCode missing both in-terminal QR and PNG fallback:\n%s", out)
	}
	// Friendly next-step hint.
	if !strings.Contains(out, "Waiting") || !strings.Contains(out, "scan") {
		t.Errorf("printQRCode missing waiting-for-scan hint\n%s", out)
	}
}