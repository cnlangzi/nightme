package telegram

import (
	"context"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/statusbar"
)

// ---------------------------------------------------------------------------
// v9 chain rolling log — flush / debounce / append primitives.
//
// These functions operate on a *placeholderChain retrieved via
// chainLRU.getOrCreate. They do NOT talk to the Telegram API directly
// — that responsibility lives on the Adapter (which owns the
// apiClient). The Adapter is expected to pass a sender closure into
// appendSegment so this file stays pure data-structures.
// ---------------------------------------------------------------------------

// sendChunkFn is the abstraction over the Telegram sendMessage call.
// Adapter supplies the concrete implementation.
//
// Returns the new message ID. Errors propagate up through appendSegment.
type sendChunkFn func(
	ctx context.Context,
	chatID string,
	topicID int,
	replyToMessageID int,
	text string,
) (int64, error)

// editChunkFn is the abstraction over the Telegram editMessageText
// call. The Adapter implementation handles debounce timing and rate
// limiting. Each call passes its own ctx (no closure capture, see
// adapter.chainEditFn).
type editChunkFn func(
	ctx context.Context,
	chatID string,
	messageID int64,
	text string,
) error

// chainChunkThresholdChars = raw-buffer ceiling per chunk.
// Above this, the next segment goes on a freshly-created chunk instead
// of the active one. See docs/channel/telegram.md §11.12.3.
const chainChunkThresholdChars = 3500

// appendSegment is the hot path. Inbound segment lands on the chain's
// active chunk; if it would push the chunk over the budget, the active
// chunk is locked and a new chunk is materialised (via sendChunkFn).
//
// statusBarLines == nil means "no footer refresh" — only segment is
// appended, lastFooter stays put.
// statusBarLines != nil means "footer refresh on next render" —
// chain.lastFooter is replaced with this snapshot.
//
// User-messageID is the user message that triggered this turn; every
// new chunk is reply_to=userMessageID so the chain hangs under the
// user's own message (per §11.12.11).
//
// P0 #1 fix (2026-08-23): both case-1 (cold-create) and case-4
// (overflow chunk materialisation) now seed the new chunk's buf with
// the inlined segment. Before this fix, the initial sendMessage body
// contained the segment while cur.buf stayed empty — the very next
// flush via renderActiveChunkBody would silently drop the first
// segment because it only renders cur.buf, not the segment that's
// already on Telegram. After this fix, subsequent renders include
// the segment in the rendered buf, so editMessageText body stays
// consistent with the original sendMessage body.
//
// P0 #3 fix (2026-08-23): the previous case-3 ("chain has a
// pre-existing next chunk slot, advance cursor and recurse") called
// appendSegment recursively while still holding chain.mu —
// sync.Mutex is not reentrant, so any future change that exercised
// this path would deadlock the goroutine. The case-3 logic is now
// inlined as a fast-forward loop at the top of the function; we
// never recurse with chain.mu held.
//
// P2 fix (2026-08-23): cold-start body now includes the
// `────────` separator so the rendered shape is identical to what
// renderActiveChunkBody will produce on the next editMessageText;
// otherwise the user briefly sees the separator appear "for free"
// on the first event after cold-start.
func appendSegment(
	ctx context.Context,
	chain *placeholderChain,
	chatID string,
	topicID int,
	userMessageID int,
	segment string,
	statusBarLines []string,
	sendFn sendChunkFn,
	editFn editChunkFn,
) error {
	chain.mu.Lock()
	defer chain.mu.Unlock()

	if statusBarLines != nil {
		chain.lastFooter = statusBarLines
	}

	// Fast-forward past any pre-locked chunks. The previous code
	// recursed into appendSegment with chain.mu held to handle this;
	// sync.Mutex is not reentrant so we now use a loop instead.
	for chain.cursor >= 0 &&
		chain.chunks[chain.cursor].isFull &&
		chain.cursor+1 < len(chain.chunks) {
		chain.cursor++
	}

	// 1. Empty chain → materialise the first chunk via send.
	if chain.cursor < 0 {
		headerLine := heartbeatText(nil)
		body := headerLine + "\n────────\n" + segment
		if chain.lastFooter != nil {
			body += "\n\n" + statusbar.RenderPanel(chain.lastFooter)
		}
		messageID, err := sendFn(ctx, chatID, topicID, userMessageID, body)
		if err != nil {
			return err
		}
		chunk := &placeholderChunk{
			messageID:  messageID,
			headerLine: headerLine,
		}
		// P0 #1 fix: seed cur.buf so subsequent renders include
		// the inlined segment. Without this, renderActiveChunkBody
		// produces header + ──── + footer (no segment) on the
		// next flush and overwrites the original message content.
		chunk.buf.WriteString(segment)
		chunk.buf.WriteByte('\n')
		chunk.charCount = len(segment) + 1
		chain.chunks = []*placeholderChunk{chunk}
		chain.cursor = 0
		chain.dirty = true
		return nil
	}

	// 2. Try to append on the active chunk.
	cur := chain.chunks[chain.cursor]
	if !cur.isFull && cur.charCount+len(segment)+1 <= chainChunkThresholdChars {
		cur.buf.WriteString(segment)
		cur.buf.WriteByte('\n')
		cur.charCount += len(segment) + 1
		chain.dirty = true
		return nil
	}

	// 3. Overflow → lock current chunk and materialise a fresh one.
	// The new chunk INHERITS cur.headerLine (the previous chunk's
	// last headerLine — the latest heartbeat snapshot or the
	// cold-create banner). The user's chat view therefore stays
	// visually continuous across the page break: the new chunk's
	// first line is the same status line that was just rendered
	// on the previous chunk, and the next OutHeartbeat will
	// patch it forward to a fresh snapshot. We intentionally do
	// NOT use a '📄 (continued)' sentinel — the user perceives the
	// chain as one timeline, not a paginated document.
	cur.isFull = true
	inheritedHeader := cur.headerLine
	body := inheritedHeader + "\n────────\n" + segment
	if chain.lastFooter != nil {
		body += "\n\n" + statusbar.RenderPanel(chain.lastFooter)
	}
	messageID, err := sendFn(ctx, chatID, topicID, userMessageID, body)
	if err != nil {
		return err
	}
	newChunk := &placeholderChunk{
		messageID:  messageID,
		headerLine: inheritedHeader,
	}
	// P0 #1 fix: same rationale as case-1. The overflow chunk
	// ships the segment inline AND records it in cur.buf so
	// subsequent renders match.
	newChunk.buf.WriteString(segment)
	newChunk.buf.WriteByte('\n')
	newChunk.charCount = len(segment) + 1
	chain.chunks = append(chain.chunks, newChunk)
	chain.cursor = len(chain.chunks) - 1
	chain.dirty = true
	return nil
}

// stopDebounceTimer stops the chain's pending debounce flush timer
// if any. Caller MUST hold chain.mu.
//
// This is exported as a method-style helper (with chain.mu held by
// the caller) so OnPromptEnded and the LRU evict path can both
// safely cancel timers without racing with scheduleFlushDebounced.
func stopDebounceTimer(chain *placeholderChain) {
	if chain.debounceTimer != nil {
		chain.debounceTimer.Stop()
		chain.debounceTimer = nil
	}
}

// flushChainNow synchronously renders the active chunk's text and
// pushes it via editChunkFn. No-op when the chain is clean.
//
// P0 #2 fix (2026-08-23): when the rendered body exceeds
// maxTelegramTextLength (3900 chars — Telegram's hard limit is 4096),
// the previous code split the rendered body into pieces[1..] and
// edited each onto a chain.tail — but kept cur.buf as the FULL
// pre-split content, so the next flush would re-render the full
// content onto the tail piece (deleting the prefix pieces from
// the user's view). The fix is to rotate the chain: edit
// pieces[0] onto cur, lock cur, append pieces[1..N-1] as fresh
// chain chunks (each with its own messageID and an empty buf),
// then advance the cursor to the last new chunk. Subsequent
// appends land on the new chunk; cur is frozen and never re-rendered.
func flushChainNow(
	ctx context.Context,
	chain *placeholderChain,
	chatID string,
	topicID int,
	userMessageID int,
	editFn editChunkFn,
	sendFn sendChunkFn,
) error {
	chain.mu.Lock()
	defer chain.mu.Unlock()

	if !chain.dirty || chain.cursor < 0 {
		return nil
	}
	cur := chain.chunks[chain.cursor]

	// renderActiveChunkBody returns the final HTML-ready body
	// (header as raw HTML, buf through RenderMarkdown, footer as
	// plain text). Do NOT pass it through RenderMarkdown again
	// — that would re-escape the header's <b>...</b> tags and
	// Telegram would render them as literal text.
	rendered := renderActiveChunkBody(cur, chain.lastFooter)

	if len(rendered) <= maxTelegramTextLength {
		if err := editFn(ctx, chatID, cur.messageID, rendered); err != nil {
			return err
		}
		chain.dirty = false
		return nil
	}

	// Overflow path: rotate into multiple chain chunks instead
	// of editing a single tail. Each piece is its own message
	// on Telegram and its own chain.chunks entry.
	//
	// We split at line boundaries (splitTelegramText preserves
	// line breaks until a piece would exceed the limit). The
	// split-points are at character indices IN THE RENDERED
	// STRING; rendered is the source-of-truth for what we've
	// emitted, so we treat it as such for buf bookkeeping too.
	pieces, err := splitTelegramText(rendered, maxTelegramTextLength)
	if err != nil {
		return err
	}
	if len(pieces) == 0 {
		chain.dirty = false
		return nil
	}

	// pieces[0] → edit onto current chunk.
	if err := editFn(ctx, chatID, cur.messageID, pieces[0]); err != nil {
		return err
	}
	cur.isFull = true
	cur.buf.Reset()
	cur.charCount = 0
	// Save rendered[:len(pieces[0])] as the chunk's "frozen tail"
	// emit, so re-renders on overflow chunks render only the
	// remaining buf (which is the new appended content, not
	// pieces[0]'s already-emitted text).
	cur.flushedRenderedLen = len(pieces[0])
	chain.dirty = false

	// pieces[1..N-1] → each becomes a fresh locked chain chunk.
	for _, p := range pieces[1 : len(pieces)-1] {
		mid, err := sendFn(ctx, chatID, topicID, userMessageID, p)
		if err != nil {
			return err
		}
		next := &placeholderChunk{
			messageID:           int64(mid),
			headerLine:          "",
			flushedRenderedLen:  len(p),
			isFull:              true,
		}
		chain.chunks = append(chain.chunks, next)
	}

	// pieces[len(pieces)-1] → if there are ≥ 2 pieces, this
	// becomes the new active chunk. buf stays empty until the next
	// append; flushedRenderedLen tracks how much of the
	// post-render (buf unknown) was already emitted so future
	// renders over the new tail don't include it.
	if len(pieces) > 1 {
		lastPiece := pieces[len(pieces)-1]
		mid, err := sendFn(ctx, chatID, topicID, userMessageID, lastPiece)
		if err != nil {
			return err
		}
		tail := &placeholderChunk{
			messageID:          int64(mid),
			headerLine:         cur.headerLine, // carry the working header into the tail
			flushedRenderedLen: len(lastPiece),
		}
		chain.chunks = append(chain.chunks, tail)
		chain.cursor = len(chain.chunks) - 1
		chain.dirty = false
	}

	return nil
}

// scheduleFlushDebounced arms (or resets) a 250ms debounce timer that
// will invoke flushChainNow. Bursts of appendSegment calls within
// the window coalesce into a single editMessageText — see docs
// §11.12.7.
//
// editFn and sendFn are passed in by the caller (the Adapter wraps
// its apiClient + rate-limiter pipeline; tests supply test doubles).
// The closures do NOT capture the request's context.Context
// because the debounce fires 250ms+ after the request may have
// returned (request ctx cancelled). The timer creates a fresh
// background ctx with a 5s timeout.
//
// chain.debounceTimer access is serialised under chain.mu so the
// Stop/Replace pair is atomic with respect to other appends and
// the OnPromptEnded purge path.
//
// Returns nil immediately. The actual flush result is dropped
// (errors are surfaced via the timer's own logger elsewhere; chat
// flow should not abort on flush failures).
func scheduleFlushDebounced(
	chain *placeholderChain,
	editFn editChunkFn,
	sendFn sendChunkFn,
	chatID string,
	topicID int,
	userMessageID int,
) {
	chain.mu.Lock()
	stopDebounceTimer(chain)
	chain.debounceTimer = time.AfterFunc(250*time.Millisecond, func() {
		// Fresh ctx: the original request that scheduled this
		// flush may have already finished by the time we fire
		// (the 250ms+ window matches that pattern). 5s budget
		// is enough for one Telegram edit round-trip.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = flushChainNow(ctx, chain, chatID, topicID, userMessageID,
			editFn, sendFn)
	})
	chain.mu.Unlock()
}

// renderActiveChunkBody builds the (raw HTML) text content for
// the active chunk: header + entries + footer.
//
// The headerLine is PRE-BAKED HTML (it carries <b>...</b> from
// the status formatters; see heartbeatText). The buf is PLAIN
// TEXT accumulated from formatted Out* events. We must NOT pass
// the whole body through RenderMarkdown: RenderMarkdown calls
// escapeHTML on its input, which would convert <b> to &lt;b&gt;
// and Telegram would render the literal tag. Instead, we route
// each section through the right pipeline:
//
//   - headerLine: written verbatim (it's already safe HTML)
//   - buf:       passed through RenderMarkdown (handles <, &, >
//                escape + light markdown → HTML)
//   - footer:    statusbar.RenderPanel output is already
//                escape-safe (no HTML chars in StatusBar lines)
//
// Note: when a chunk has flushedRenderedLen > 0, only the
// not-yet-emitted tail of buf is rendered. The emitted prefix
// lives on Telegram in the locked preceding chunks.
func renderActiveChunkBody(cur *placeholderChunk, lastFooter []string) string {
	var b strings.Builder
	b.WriteString(cur.headerLine)
	b.WriteByte('\n')
	if cur.charCount > 0 {
		b.WriteString("────────\n")
		var bufSrc string
		if cur.flushedRenderedLen > 0 && cur.flushedRenderedLen < cur.buf.Len() {
			bufSrc = cur.buf.String()[cur.flushedRenderedLen:]
		} else {
			bufSrc = cur.buf.String()
		}
		renderedBuf, err := RenderMarkdown(bufSrc)
		if err != nil {
			renderedBuf = escapeHTML(bufSrc)
		}
		b.WriteString(renderedBuf)
		if !strings.HasSuffix(bufSrc, "\n") {
			b.WriteByte('\n')
		}
	}
	if len(lastFooter) > 0 {
		b.WriteByte('\n')
		b.WriteString(statusbar.RenderPanel(lastFooter))
	}
	return b.String()
}
