// Tests for the shared IM-card preview helper (command.PreviewForIM).
//
// These tests pin the contract that /steer and /queue both rely on:
//   - bodies <= previewRuneCap are echoed verbatim (no ellipsis)
//   - bodies exactly at the cap are NOT truncated (cap is inclusive)
//   - bodies over the cap are truncated at a rune boundary and end
//     with "..."
//   - the truncation is CJK / emoji safe (multi-byte UTF-8 sequences
//     are never split — would render as U+FFFD in the IM card)
//
// The rune cap constant is not exported from the preview package
// (it's an implementation detail). These tests hardcode 80 to match
// the documented value; if the cap changes, update both this file
// and the package-doc comment in preview.go.
package command_test

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/command"
)

// previewRuneCap mirrors internal/command/preview.go's previewRuneCap.
// Kept local to avoid exporting an implementation detail.
const previewRuneCap = 80

func TestPreviewForIM_EmptyBody(t *testing.T) {
	if got := command.PreviewForIM(""); got != "" {
		t.Fatalf("empty body: got %q, want \"\"", got)
	}
}

// Short body well below the cap must be returned verbatim, no
// ellipsis — flagging a short body as truncated would mislead users
// who can already see the full body in their own message.
func TestPreviewForIM_ShortBody_NoEllipsis(t *testing.T) {
	const bodyLen = 40 // well under previewRuneCap=80
	body := strings.Repeat("a", bodyLen)

	got := command.PreviewForIM(body)

	if strings.HasSuffix(got, "...") {
		t.Fatalf("short body preview should not end with ellipsis: %q", got)
	}
	if got != body {
		t.Fatalf("short body preview should match body exactly: got %q, want %q", got, body)
	}
}

// Boundary behaviour around the cap is the part most likely to
// regress silently (off-by-one in the rune cap constant would
// cause a 79 / 80 / 81 rune body to flip truncation state).
func TestPreviewForIM_BoundaryBodies_Truncation(t *testing.T) {
	// Exactly-at-cap body is NOT truncated (cap is inclusive).
	bodyAtCap := strings.Repeat("a", previewRuneCap)
	gotAtCap := command.PreviewForIM(bodyAtCap)
	if strings.HasSuffix(gotAtCap, "...") {
		t.Fatalf("at-cap preview should not end with ellipsis: %q", gotAtCap)
	}
	if gotAtCap != bodyAtCap {
		t.Fatalf("at-cap preview should match body exactly: got %q, want %q", gotAtCap, bodyAtCap)
	}

	// Just-over-cap body IS truncated.
	bodyOverCap := strings.Repeat("a", previewRuneCap+1)
	gotOverCap := command.PreviewForIM(bodyOverCap)
	if !strings.HasSuffix(gotOverCap, "...") {
		t.Fatalf("over-cap preview should end with ellipsis: %q", gotOverCap)
	}
	if len([]rune(gotOverCap)) >= len([]rune(bodyOverCap)) {
		t.Fatalf("over-cap preview should be shorter than body: preview runes=%d, body runes=%d",
			len([]rune(gotOverCap)), len([]rune(bodyOverCap)))
	}
}

// Well-over-cap body should be capped at previewRuneCap-3 runes of
// content + "..." ellipsis. Verify the rune count of the *content*
// portion (excluding the "..." ellipsis suffix) is exactly
// previewRuneCap-3 — anything else means the truncation math
// drifted.
func TestPreviewForIM_LongBody_RuneCapIsExact(t *testing.T) {
	body := strings.Repeat("中", 200) // 200 CJK runes, 600 bytes
	got := command.PreviewForIM(body)

	if !strings.HasSuffix(got, "...") {
		t.Fatalf("long body should be truncated with ellipsis: %q", got)
	}
	content := strings.TrimSuffix(got, "...")
	if gotRunes := len([]rune(content)); gotRunes != previewRuneCap-3 {
		t.Fatalf("content rune count: got %d, want %d", gotRunes, previewRuneCap-3)
	}
	// No U+FFFD — would indicate a mid-rune cut from byte-level
	// truncation.
	if strings.ContainsRune(got, '�') {
		t.Fatalf("long body preview contains U+FFFD (mid-rune cut): %q", got)
	}
}

// Emoji are 4-byte UTF-8 sequences (single rune). Truncating at a
// rune boundary must not split an emoji — the byte-level cut
// would render as U+FFFD in the IM card.
func TestPreviewForIM_LongBody_EmojiRuneBoundary(t *testing.T) {
	body := strings.Repeat("🚀", 100) // 100 emoji runes, 400 bytes
	got := command.PreviewForIM(body)

	if !strings.HasSuffix(got, "...") {
		t.Fatalf("emoji body should be truncated: %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("emoji preview contains U+FFFD (mid-rune cut): %q", got)
	}
	// Content portion should be exactly previewRuneCap-3 runes
	// of emoji.
	content := strings.TrimSuffix(got, "...")
	if gotRunes := len([]rune(content)); gotRunes != previewRuneCap-3 {
		t.Fatalf("emoji content rune count: got %d, want %d", gotRunes, previewRuneCap-3)
	}
}