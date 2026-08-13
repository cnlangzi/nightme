//go:build !windows

package feishu

import (
	"bytes"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
)

// TestPrintQRCode_WritesURLAndQR wires the OnQRCode path so a
// future refactor of printQRCode cannot accidentally drop the URL
// line, the QR glyphs, or the "Waiting for scan" hint.
//
// The Unix build always routes through renderQRPlatform →
// RenderASCII, so this test asserts on the half-block Unicode glyphs.
// The Windows build has its own copy of this test in
// provider_windows_test.go that asserts on the PNG fallback /
// RenderANSI path instead.
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
	if !strings.ContainsAny(out, " █▀▄") {
		t.Errorf("printQRCode did not emit QR glyphs\n%s", out)
	}
	// Friendly next-step hint: without this the user just sees the
	// QR and a polling loop they don't know is waiting. We assert on
	// a substring that's likely to survive copy edits ("Waiting" /
	// "scan") so a future tweak to wording doesn't snap the test.
	if !strings.Contains(out, "Waiting") || !strings.Contains(out, "scan") {
		t.Errorf("printQRCode missing waiting-for-scan hint\n%s", out)
	}
}