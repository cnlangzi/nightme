// F-44 revert: rolling-log OutReply entries folded into the receipt
// card (F-25 → F-40 model). Each OutReply chunk lands as a LogEntry
// in the receipt's entries slice; the receipt's renderLocked
// function maps each entry to one or more `div` elements (split via
// splitMarkdownForDivs when the entry text exceeds divTextCharLimit).
//
// Overflow bail-out (fix-reply-placehold-card): if appending an entry
// would push the card past 50 elements / 30 KB envelope,
// AppendEntry returns ErrReceiptOverflow and the caller
//
//  1. sends that entry as a fresh top-level placeholder card (no
//     thread anchor — ReplyInChat, always visible in main chat,
//     escapes the parent-thread drawer), and
//  2. calls receipt.RolloverTo(msgID, overflowEntry, footerLines)
//     so subsequent chunks PATCH the new card instead of producing
//     a stream of N standalone bubbles.
//
// The receipt's old card stays anchored to the user message
// (ReplyInBoth) so the visual thread to the user input is preserved
// in the normal case; the new placeholder is a top-level Create
// (rootID="") for the overflow chunks.

package feishu

import "errors"

// LogEntry is one line in the rolling-log receipt. F-25 → F-40 used
// to also carry a timestamp / agent-name suffix; F-44 keeps the
// surface minimal (just icon + text) since the prompt state icon
// (⏳/🔄/✅/❌) was removed in favor of OutMessageState → AddReaction
// on the user message and the agent-name footer is deferred to a
// follow-up PR.
type LogEntry struct {
	// Icon is a short emoji prefix shown before the entry text. The
	// F-40 design used "💬" for OutReply text chunks; the field is
	// kept on the struct so future entry types (e.g. a tool-result
	// line) can pick their own glyph without changing the rendering
	// pipeline.
	Icon string
	// Text is the entry body, taken verbatim from OutboundMessage.Text
	// after SanitizeCardMarkdown. Split into multiple div elements
	// by splitMarkdownForDivs when it exceeds divTextCharLimit.
	Text string
}

// ErrReceiptOverflow is returned by AppendEntry when adding the
// entry would push the card past the element-count or envelope-
// size limit. The caller is expected to:
//
//  1. Catch this sentinel.
//  2. Build a body for a fresh top-level placeholder card that
//     seeds itself with the entry that overflowed (so the user
//     sees the same content the old postOrphanReplyCard fallback
//     would have surfaced).
//  3. Send that body via SendCardForReceipt(chatID, body, "", false)
//     to get the new placeholder's msgID.
//  4. Call receipt.RolloverTo(msgID, overflowEntry, footerLines)
//     to migrate the receipt's tracking to the new card. Future
//     AppendEntry / SetTaskList calls PATCH the new card instead
//     of the original, so a stream of N overflow chunks collapses
//     into one placeholder card + N-1 PATCHes, not N standalone
//     bubbles.
//
// Detection: buildReceiptCard returns the projected body for the
// would-be post-append state; AppendEntry counts elements and
// bytes, returns this error if either exceeds the cap BEFORE
// issuing the PATCH. No Feishu PATCH call is made in the overflow
// path, so the existing card stays untouched (no orphan-element
// half-render).
var ErrReceiptOverflow = errors.New("feishu receipt: append would exceed card limit; caller should send as fresh top-level message and call RolloverTo")

// receiptMaxElements is the Feishu Card 2.0 hard cap on body element
// count. Cards with more than this many elements are rejected by
// the API. The receipt budget is split between entries (multi-div
// when the entry text is long) and the task checklist; the
// overflow check is conservative — if entries * 1 (avg) + tasks
// > cap, the append bails out.
const receiptMaxElements = 50

// wouldReceiptOverflow reports whether a card body with the given
// element count and byte size exceeds the Feishu Card 2.0 limits.
// Used by AppendEntry for the pre-PATCH bail-out decision. The
// envelope constant (resultCardEnvelopeBudget) lives in
// result_render.go; the element cap is local to this file since
// it's specific to the receipt's multi-div entries + task layout.
func wouldReceiptOverflow(elementCount int, bodyBytes int) bool {
	if elementCount > receiptMaxElements {
		return true
	}
	if bodyBytes > resultCardEnvelopeBudget {
		return true
	}
	return false
}
