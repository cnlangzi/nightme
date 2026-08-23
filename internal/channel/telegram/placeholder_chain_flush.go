package telegram

import (
	"context"
	"fmt"
	"time"

	"github.com/cnlangzi/nightme/internal/statusbar"
)

// ---------------------------------------------------------------------------
// v9 chain rolling log — flush / debounce / append primitives.
//
// These functions operate on a *placeholderChain retrieved via
// chainLRU.getOrCreate. They do NOT talk to the Telegram API directly
// — that responsibility lives on the Adapter (which owns the
// apiClient). The Adapter supplies send / edit closures (loose fns)
// that this file consumes; the network abstraction stays in the
// Adapter.
// ---------------------------------------------------------------------------

// sendChunkFn / editChunkFn are the loose function types the chain
// primitives consume. The Adapter wraps sendTelegramMessage /
// editTelegramMessage + rate-limiter + retry in closures that
// satisfy this contract. (A telegramSender interface wrapper was
// prototyped in commit d4349c1 but reverted — loose fns are
// adequate for the current Layer 5 surface.)
type sendChunkFn func(ctx context.Context, chatID string, topicID int, replyToMessageID int, text string) (int64, error)
type editChunkFn func(ctx context.Context, chatID string, messageID int64, text string) error

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
// bufTextSize returns the byte length of the chunk's rendered
// entries (post-separator). Used to decide if a new segment
// would push the chunk past the 3500-char threshold. O(n) on
// entries; fine since chains are short.
func (b *chunkBody) bufTextSize() int {
	n := 0
	for _, e := range b.entries {
		n += len(e.text) + 1 // +1 for trailing newline separator
	}
	return n
}

func appendSegment(
	ctx context.Context,
	chain *placeholderChain,
	chatID string,
	topicID int,
	userMessageID int,
	segment string,
	statusBarLines []string,
	sendFn sendChunkFn,
	_ editChunkFn,
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
		chain.chunks[chain.cursor].isChunkFull() &&
		chain.cursor+1 < len(chain.chunks) {
		chain.cursor++
	}

	// 1. Empty chain → materialise the first chunk via send.
	if chain.cursor < 0 {
		headerLine := heartbeatText(nil)
		chunk := newChunkBody(0, headerLine)
		// P0 #1 fix: seed entries so subsequent renders include
		// the inlined segment. Without this, Compose() produces
		// header + ──── + footer (no segment) on the next flush
		// and overwrites the original message content.
		chunk.appendEntry(segment)
		// Set the footer before computing the body so the cold-
		// create render matches what subsequent flushes produce.
		if chain.lastFooter != nil {
			chunk.setFooter(statusbar.RenderPanel(chain.lastFooter))
		}
		body := chunk.Compose()
		messageID, err := sendFn(ctx, chatID, topicID, userMessageID, body)
		if err != nil {
			return err
		}
		chunk.messageID = messageID
		chain.chunks = []*chunkBody{chunk}
		chain.cursor = 0
		chain.dirty = true
		return nil
	}

	// 2. Try to append on the active chunk.
	cur := chain.chunks[chain.cursor]
	if !cur.isChunkFull() && cur.bufTextSize()+len(segment)+1 <= chainChunkThresholdChars {
		cur.appendEntry(segment)
		chain.dirty = true
		return nil
	}

	// 3. Overflow → lock current chunk and materialise a fresh one.
	// The new chunk's header is heartbeatText(nil) — its creation
	// time, NOT cur.headerText(). This matches the SPLIT path
	// (§11.12.7.2 trigger 1) which also uses heartbeatText(nil) for
	// all its chunks. Each Telegram message's header reflects the
	// time the message was actually sent, so users scrolling to the
	// bottom of the chat see timestamps that monotonically advance.
	// The next OutHeartbeat will refresh the new chunk's header
	// forward to a fresh snapshot as usual.
	cur.markFull()
	newChunk := newChunkBody(0, heartbeatText(nil))
	newChunk.appendEntry(segment)
	if chain.lastFooter != nil {
		newChunk.setFooter(statusbar.RenderPanel(chain.lastFooter))
	}
	body := newChunk.Compose()
	messageID, err := sendFn(ctx, chatID, topicID, userMessageID, body)
	if err != nil {
		return err
	}
	newChunk.messageID = messageID
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
	fmt.Println("FLUSH active chunk header:", cur.headerText(), "nchunks:", len(chain.chunks))

	// renderActiveChunkBody returns the final HTML-ready body
	// (header as raw HTML, buf through RenderMarkdown, footer as
	// plain text). Do NOT pass it through RenderMarkdown again
	// — that would re-escape the header's <b>...</b> tags and
	// Telegram would render them as literal text.
	cur.setFooter(statusbar.RenderPanel(chain.lastFooter))
	rendered := renderActiveChunkBody(cur)

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
	if err := editFn(ctx, chatID, cur.messageIDValue(), pieces[0]); err != nil {
		return err
	}
	cur.markFull()
	// Wipe entries but keep the chunk's messageID + flushedLen
	// so subsequent renders on overflow chunks render only the
	// remaining (new appended) content. After reset, Compose()
	// emits just header + separator + footer (empty buf body),
	// which matches what was sent in pieces[0].
	cur.freezeAfterOverflow(len(pieces[0]))
	chain.dirty = false

	// pieces[1..N-1] → each becomes a fresh locked chain chunk.
	for _, p := range pieces[1 : len(pieces)-1] {
		mid, err := sendFn(ctx, chatID, topicID, userMessageID, p)
		if err != nil {
			return err
		}
		next := newChunkBody(int64(mid), "")
		next.markFull()
		next.markFlushedLen(len(p))
		chain.chunks = append(chain.chunks, next)
	}

	// pieces[len(pieces)-1] → if there are ≥ 2 pieces, this
	// becomes the new active chunk. CRITICAL: the tail carries
	// pieces[N-1] as its first entry so subsequent flushes
	// re-render the long-text content. Pre-fix (P0 data loss)
	// the tail had empty entries, so the next OutHeartbeat
	// patch would render "<header>\n<footer>" and editMessageText
	// would erase the pieces[N-1] content from Telegram. Locked
	// in by TestChain_OverflowTailRetainsContent regression.
	if len(pieces) > 1 {
		lastPiece := pieces[len(pieces)-1]
		mid, err := sendFn(ctx, chatID, topicID, userMessageID, lastPiece)
		if err != nil {
			return err
		}
		// Header reflects the time the tail chunk is being sent —
		// matches the SPLIT path and appendSegment case 3, so all
		// new chunks produced by the chain get a fresh timestamp
		// rather than inheriting cur's potentially stale header.
		tail := newChunkBody(int64(mid), heartbeatText(nil))
		// Seed entries with the lastPiece content as a plain-text
		// entry. flushedLen=0 because the entire content is in
		// entries — Compose will render it. Future appends land
		// alongside via appendSegment path #2.
		tail.appendEntry(lastPiece)
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

// renderActiveChunkBody is a render-only wrapper around
// chunkBody.Compose(). flushChainNow sets the footer before
// calling this — renderActiveChunkBody does NOT mutate the
// chunk (Layer 4 separation: renders don't touch the data
// model).
func renderActiveChunkBody(cur *chunkBody) string {
	return cur.Compose()
}

// appendErrorSegment is the OutError-specific path. It mirrors
// appendSegment's cold-create / append / overflow logic but
// delegates to chunkBody.appendError so the ```fences```
// wrapping decision stays in the data layer (Layer 3). Adapter
// calls this from the OutError case instead of building the
// fence string inline.
//
// IMPORTANT: this function does NOT schedule the debounce
// flush. The caller (adapter OutError case) must schedule it
// after the call returns, so the flush runs against the
// up-to-date chain state. (Same contract as appendSegment.)
func appendErrorSegment(
	ctx context.Context,
	chain *placeholderChain,
	chatID string,
	topicID int,
	userMessageID int,
	text, stderr string,
	statusBarLines []string,
	sendFn sendChunkFn,
	_ editChunkFn,
) error {
	chain.mu.Lock()
	defer chain.mu.Unlock()

	if statusBarLines != nil {
		chain.lastFooter = statusBarLines
	}

	for chain.cursor >= 0 &&
		chain.chunks[chain.cursor].isChunkFull() &&
		chain.cursor+1 < len(chain.chunks) {
		chain.cursor++
	}

	// Cold-create.
	if chain.cursor < 0 {
		headerLine := heartbeatText(nil)
		chunk := newChunkBody(0, headerLine)
		chunk.appendError(text, stderr)
		if chain.lastFooter != nil {
			chunk.setFooter(statusbar.RenderPanel(chain.lastFooter))
		}
		body := chunk.Compose()
		messageID, err := sendFn(ctx, chatID, topicID, userMessageID, body)
		if err != nil {
			return err
		}
		chunk.messageID = messageID
		chain.chunks = []*chunkBody{chunk}
		chain.cursor = 0
		chain.dirty = true
		return nil
	}

	// Append in place.
	cur := chain.chunks[chain.cursor]
	if !cur.isChunkFull() && cur.bufTextSize()+estimateErrorSize(stderr) <= chainChunkThresholdChars {
		cur.appendError(text, stderr)
		chain.dirty = true
		return nil
	}

	// Overflow: rotate.
	cur.markFull()
	inheritedHeader := cur.headerText()
	newChunk := newChunkBody(0, inheritedHeader)
	newChunk.appendError(text, stderr)
	if chain.lastFooter != nil {
		newChunk.setFooter(statusbar.RenderPanel(chain.lastFooter))
	}
	body := newChunk.Compose()
	messageID, err := sendFn(ctx, chatID, topicID, userMessageID, body)
	if err != nil {
		return err
	}
	newChunk.messageID = messageID
	chain.chunks = append(chain.chunks, newChunk)
	chain.cursor = len(chain.chunks) - 1
	chain.dirty = true
	return nil
}

// estimateErrorSize gives a rough budget for an OutError entry:
// plain text length plus the ```fence``` wrapping overhead. The
// fence adds ~10 chars regardless of stderr content, so the
// chunk.bufTextSize check stays meaningful at the threshold
// boundary.
func estimateErrorSize(stderr string) int {
	return 10 + len(stderr) + 1 // ~"```\n" + stderr + "\n```" + trailing newline
}
