// Package command — preview helpers shared by slash commands
// that echo a short preview of the user's input back in an IM
// card reply (/steer, /queue, and any future echo-style command).
package command

import (
	"strings"
	"unicode/utf8"
)

// previewRuneCap is the IM-card-friendly preview length. IM
// channels (Feishu especially) truncate plain-text payloads
// around 4 KB, but the visible header is much shorter; 80 runes
// keeps one short sentence visible while still leaving room for
// the emoji + label + ellipsis that commands prefix the body
// with ("🛑 Steering: ", "📥 Queued: ", etc.).
const previewRuneCap = 80

// PreviewForIM returns a preview of s suitable for echo in an IM
// card reply. Strings already at or below previewRuneCap runes
// are returned unchanged (no ellipsis appended — there's no
// reason to flag a short body as truncated). Longer strings are
// truncated at a rune boundary (never splitting a multi-byte
// UTF-8 sequence — CJK / emoji would render as U+FFFD in the
// IM card if cut mid-byte) with "..." appended.
//
// Shared by /steer and /queue so the preview contract can be
// tweaked in one place.
func PreviewForIM(s string) string {
	if utf8.RuneCountInString(s) <= previewRuneCap {
		return s
	}
	var b strings.Builder
	b.Grow(previewRuneCap*4 + 3) // worst-case 4 bytes per rune + ellipsis
	count := 0
	for _, r := range s {
		if count >= previewRuneCap-3 {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String() + "..."
}