package telegram

import (
	"context"
	"encoding/json"
	"html"
	"strings"
)

// ---------------------------------------------------------------------------
// Telegram Bot API 10.1 rich message path (docs/channel/telegram.md §20).
//
// L3 (current): every rich_message send carries rich_message[blocks] —
// block-level rich blocks built by markdownToRichBlocks (rich_walker.go)
// for the body and by footerLinesToRichText (result_blocks.go) for the
// StatusBar trailer. trySendRichBlocks is the only sender; the legacy
// rich_message[markdown] path (trySendRichMarkdown + canUseRichMarkdown
// + estimateRichBlocks) was retired when OutResult migrated from the
// plaintext-statusbar-in-markdown shape to a real footer block.
// ---------------------------------------------------------------------------

// richBlockCountLimit is the Telegram server's per-message block cap
// for rich_message[blocks] (verified in
// docs/channel/telegram.md §20.3). sendRichMessage returns
// RICH_MESSAGE_BLOCKS_TOO_MANY above this count.
const richBlockCountLimit = 500

// richBlockCountPreflightNumerator / Divisor: preflight caps at
// Numerator/Divisor of the server hard limit. 4/5 → 400 / 500. Keep
// this ratio close to 1 so we don't unnecessarily fall back, but
// far enough from 1 to absorb parser heuristics variance.
const (
	richBlockCountPreflightNumerator = 4
	richBlockCountPreflightDivisor   = 5
)

// richMarkdownCharLimit is the per-message char ceiling we respect
// for the rich path. The server's per-block ceiling is 32K+ chars
// (verified up to 40K single paragraph, §20.1); we cap at 32K to
// match the documented limit and leave 0-byte headroom for
// escape / JSON-encode expansion. Above this we fall back to plain.
const richMarkdownCharLimit = 32000

// blockThresholdForRich returns the preflight block-count ceiling.
// Exposed as a function so tests can compare against the documented
// constants without duplicating the arithmetic.
func blockThresholdForRich() int {
	return richBlockCountLimit * richBlockCountPreflightNumerator / richBlockCountPreflightDivisor
}

// trySendRichBlocks sends one Telegram message via the sendRichMessage
// API, carrying a rich_message[blocks] body. blocksJSON must be a
// JSON-encoded array (the output of markdownToRichBlocks when it
// returns ok=true). Caller decides error handling; today the only
// caller is sendOutResultMessage, which surfaces errors to the
// runtime rather than silently falling back to plain text.
func (a *Adapter) trySendRichBlocks(
	ctx context.Context,
	chatID string,
	topicID int,
	replyToMessageID int,
	blocksJSON string,
) (int64, error) {
	// blocksJSON is a JSON array. Wrap in {"blocks": [...]} and
	// embed as json.RawMessage so encoding/json inlines it
	// verbatim instead of re-marshalling (which would escape the
	// quotes and break the wire form).
	params := map[string]any{
		"chat_id": chatID,
		"rich_message": map[string]any{
			"blocks": json.RawMessage(blocksJSON),
		},
	}
	if topicID > 0 {
		params["message_thread_id"] = topicID
	}
	if replyToMessageID > 0 {
		params["reply_to_message_id"] = replyToMessageID
	}

	var result SendMessageResult
	if err := a.apiCall(ctx, "sendRichMessage", params, &result); err != nil {
		return 0, err
	}
	if result.MessageID == 0 {
		return 0, &apiError{Message: "sendRichMessage returned empty message_id"}
	}
	return int64(result.MessageID), nil
}

// richModeAllowsSend was a config gate that controlled whether the
// adapter could use the rich-message path. L3 retired the gate —
// rich mode is always on (no config flag, no opt-out). Kept as a
// stub for any callers that still reference it; returns true so
// the call sites keep working unchanged.
//
// Deprecated: callers should switch to unconditional rich-mode
// use. Will be removed once every reference is gone.
func (a *Adapter) richModeAllowsSend() bool { return true }

// ---------------------------------------------------------------------------
// L2 placeholder: markdownToRichBlocks (AST walker).
//
// Will be implemented when L2 lands — explicit blocks walker over
// goldmark AST (paragraph / heading / pre / list / blockquote /
// details / table / footer / map / math / buttons / photo) plus
// inline RichText entities (bold / italic / code / url). The
// signature is committed here so L2 callers can wire against it
// before the walker body lands.
// ---------------------------------------------------------------------------

// normaliseRichMode was a config-validation helper that mapped
// config.Telegram.RichMode values into a canonical form. With the
// config field gone (L3: rich mode is always on), this is dead
// code — callers should drop their RichMode string fields.
//
// Deprecated: kept as a stub returning the input verbatim so
// existing test references compile. Will be removed once tests
// are updated.
func normaliseRichMode(raw string) string { return raw }

// markdownToRichBlocks is implemented in rich_walker.go (L2). It
// walks raw markdown and emits a JSON-encoded rich_message[blocks]
// array; returns ok=false when the walker can't represent the input,
// in which which case callers should fall back to the L1
// rich_message[markdown] path.

// htmlChoiceBodyToBlocks parses the small HTML shape that
// renderChoice produces — "<b>Title</b>\n\nBody" — into rich
// blocks (heading + paragraph). Used by sendRichFromHTML to
// convert Choice / Permission / ForceReply prompts into
// rich_message[blocks] format. No fallback path: every outbound
// bubble to Telegram must be rich_message format.
//
// renderChoice HTML-escapes its interpolated values via
// escapeInline, and Telegram rich block text fields are plain
// text — they do not interpret HTML. Unescape entities so the
// user sees the original characters (& not &, < not <).
func htmlChoiceBodyToBlocks(htmlBody string) string {
	title, body := splitChoiceHTML(htmlBody)
	title = html.UnescapeString(title)
	body = html.UnescapeString(body)
	var blocks []map[string]any
	if title != "" {
		blocks = append(blocks, map[string]any{
			"type": "heading",
			"text": title,
			"size": 2,
		})
	}
	if body != "" {
		blocks = append(blocks, map[string]any{
			"type": "paragraph",
			"text": body,
		})
	}
	if len(blocks) == 0 {
		// Defensive: empty input still produces a valid blocks
		// array so the wire form is {"blocks":[]} and never
		// {"blocks":null}.
		blocks = append(blocks, map[string]any{
			"type": "paragraph",
			"text": "",
		})
	}
	b, _ := json.Marshal(blocks)
	return string(b)
}

// splitChoiceHTML extracts the optional <b>...</b> heading and
// remaining body from a renderChoice-shaped string. Returns
// (title, body) with leading/trailing whitespace stripped.
func splitChoiceHTML(htmlBody string) (string, string) {
	trimmed := strings.TrimSpace(htmlBody)
	if !strings.HasPrefix(trimmed, "<b>") {
		return "", trimmed
	}
	end := strings.Index(trimmed, "</b>")
	if end < 0 {
		return "", trimmed
	}
	title := trimmed[3:end]
	rest := strings.TrimSpace(trimmed[end+4:])
	return title, rest
}

// sendRichFromHTML sends a Telegram message in rich_message[blocks]
// format. Mirrors sendTelegramMessage's signature but uses
// rich_message exclusively — there is NO plain-text fallback path
// per the design contract (every outbound bubble is rich).
//
// Used for Choice / Permission / ForceReply prompts that
// currently build HTML bodies via renderChoice — converting them
// to rich blocks keeps the visual surface uniform.
func (a *Adapter) sendRichFromHTML(
	ctx context.Context,
	chatID string,
	topicID int,
	replyToMessageID int,
	htmlBody string,
	keyboard map[string]any,
) (SendMessageResult, error) {
	params := map[string]any{
		"chat_id": chatID,
		"rich_message": map[string]any{
			"blocks": json.RawMessage(htmlChoiceBodyToBlocks(htmlBody)),
		},
	}
	if topicID > 0 {
		params["message_thread_id"] = topicID
	}
	if replyToMessageID > 0 {
		params["reply_to_message_id"] = replyToMessageID
	}
	if keyboard != nil {
		params["reply_markup"] = keyboard
	}
	var result SendMessageResult
	if err := a.apiCall(ctx, "sendRichMessage", params, &result); err != nil {
		return SendMessageResult{}, err
	}
	return result, nil
}

// editRichFromHTML edits a Telegram message in rich_message[blocks]
// format. Mirrors editTelegramMessage's signature, replacing
// text + parse_mode with rich_message[blocks].
func (a *Adapter) editRichFromHTML(
	ctx context.Context,
	chatID string,
	messageID int,
	htmlBody string,
	keyboard map[string]any,
) error {
	params := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"rich_message": map[string]any{
			"blocks": json.RawMessage(htmlChoiceBodyToBlocks(htmlBody)),
		},
	}
	if keyboard != nil {
		params["reply_markup"] = keyboard
	}
	return a.apiCall(ctx, "editMessageText", params, nil)
}
