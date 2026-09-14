package telegram

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/cnlangzi/nightme/internal/statusbar"
)

// ---------------------------------------------------------------------------
// Telegram Bot API 10.1 rich message path (docs/channel/telegram.md §20).
//
// Three layers:
//   1. estimateRichBlocks    — line-based preflight count (L1).
//   2. trySendRichMarkdown   — L1 send helper. Posts rich_message[markdown]
//      via sendRichMessage, falling back to plain text on any server error.
//   3. markdownToRichBlocks  — L2 AST walker (placeholder; not yet wired).
//
// L1 is implemented and gated on TelegramConfig.RichMode (default "off" —
// pre-10.1 client compatibility, see §20.6.1 and §20.8). L2/L3 land in
// subsequent commits; this file's structure anticipates them so L1 stays
// internal and the public surface grows in one direction.
// ---------------------------------------------------------------------------

// richBlockCountLimit is the Telegram server's per-message block cap
// for rich_message[markdown] / [html] / [blocks] (verified in
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

// estimateRichBlocks counts structural blocks in raw markdown to
// predict how many blocks Telegram's auto-parser will produce.
//
// Heuristic aligns with the server's parser behaviour observed in
// round 4 (§20.3): each heading / fenced code block / list item /
// blockquote line counts as 1 block; consecutive non-blank lines
// that aren't recognised structural markers collapse into a single
// paragraph block. Plain-prose paragraphs are the most common
// "inaccuracy" target — the server may emit one block per blank-
// line-separated paragraph (matching our heuristic) or one block
// per sentence (more aggressive). The preflight divisor (4/5) gives
// enough slack that sentence-level splitting rarely trips us into
// fallback unnecessarily.
//
// Pure function: no AST walk, no library dependency. The project's
// existing markdown rendering (render.go) is also regex/line-based,
// so a line-based preflight stays consistent with the rest of the
// pipeline. L2 will introduce a real AST walker (goldmark) once we
// add it as a direct dependency.
func estimateRichBlocks(rawMD string) int {
	if rawMD == "" {
		return 0
	}

	const (
		headingPat   = `^#{1,6}\s`
		bulletPat    = `^[-*+]\s+`
		orderedPat   = `^\d+\.\s+`
		quotePat     = `^>\s*`
		fenceOpenPat = `^` + "```"
	)

	n := 0
	inFence := false
	paragraphOpen := false

	flushParagraph := func() {
		if paragraphOpen {
			n++
			paragraphOpen = false
		}
	}

	for _, rawLine := range strings.Split(rawMD, "\n") {
		line := strings.TrimSpace(rawLine)

		if strings.HasPrefix(line, fenceOpenPat) {
			if !inFence {
				n++
				inFence = true
			} else {
				inFence = false
			}
			flushParagraph()
			continue
		}
		if inFence {
			// Lines inside a fenced code block don't break out as
			// separate blocks — the entire fence is one block.
			continue
		}

		if line == "" {
			flushParagraph()
			continue
		}

		switch {
		case regexp.MustCompile(headingPat).MatchString(line),
			regexp.MustCompile(bulletPat).MatchString(line),
			regexp.MustCompile(orderedPat).MatchString(line),
			regexp.MustCompile(quotePat).MatchString(line):
			n++
			flushParagraph()
		default:
			paragraphOpen = true
		}
	}
	flushParagraph()
	return n
}

// canUseRichMarkdown decides whether the rich path is worth trying for
// a given markdown payload. Centralises the preflight so both L1's
// OutResult handler and any future L2/L3 caller share the gate.
func canUseRichMarkdown(rawMD string) bool {
	if len(rawMD) == 0 || len(rawMD) > richMarkdownCharLimit {
		return false
	}
	return estimateRichBlocks(rawMD) <= blockThresholdForRich()
}

// buildRichMarkdownWithTrailer appends the StatusBar trailer to a
// markdown body. Rich messages don't have a separate footer field
// (InputRichMessage exposes only blocks / html / markdown / media),
// so the trailer rides inline. statusbar.RenderPanel output is
// unicode box-drawing (`┌──› / └──›`) — Telegram's rich renderer
// treats it as plain text, which is the intended visual: a distinct
// frame below the result body, matching the chain chunk renderer's
// footer convention.
//
// Returns rawMD unchanged when footerLines is empty.
func buildRichMarkdownWithTrailer(rawMD string, footerLines []string) string {
	if len(footerLines) == 0 {
		return rawMD
	}
	return rawMD + "\n\n" + statusbar.RenderPanel(footerLines)
}

// trySendRichMarkdown sends one Telegram message via the
// sendRichMessage API, carrying a rich_message[markdown] body.
//
// chatID / topicID / replyToMessageID mirror the semantics of
// sendTelegramMessage: topicID > 0 routes into a Forum topic;
// replyToMessageID > 0 anchors the new bubble to the user's message.
//
// Returns the new message's ID on success, the apiError on failure.
// The caller (sendOutResultMessage's L1 branch) decides how to react
// to errors — L1's contract is "try rich, fall back to plain on any
// non-2xx".
func (a *Adapter) trySendRichMarkdown(
	ctx context.Context,
	chatID string,
	topicID int,
	replyToMessageID int,
	rawMD string,
) (int64, error) {
	params := map[string]any{
		"chat_id": chatID,
		"rich_message": map[string]any{
			"markdown": rawMD,
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

// trySendRichBlocks sends one Telegram message via the sendRichMessage
// API, carrying a rich_message[blocks] body. blocksJSON must be a
// JSON-encoded array (the output of markdownToRichBlocks when it
// returns ok=true). Mirrors trySendRichMarkdown's contract: caller
// decides error handling; L2 wiring falls back to chain on failure.
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
// in which case callers should fall back to the L1
// rich_message[markdown] path.
