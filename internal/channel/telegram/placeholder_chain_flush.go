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
// apiClient). The Adapter supplies a telegramSender so this file
// stays pure data-structures.
// ---------------------------------------------------------------------------

// telegramSender is the network abstraction Layer 5 (sender) of
// the v9 chain OOP layering. The chain primitives (Layer 1 view +
// Layer 2 chain) call into this interface; the Adapter supplies
// the concrete impl that wraps sendTelegramMessage +
// editTelegramMessage with the rate-limiter + retry pipeline.
//
// Send emits a new message; Edit replaces an existing message. Both
// accept a fresh ctx per call (no closure capture) so the debounce
// timer — which fires 250ms+ after the original request may have
// returned — can use a context.Background()-derived ctx instead of
// the cancelled Request ctx.
type telegramSender interface {
	Send(ctx context.Context, chatID string, topicID int, replyToMessageID int, text string) (int64, error)
	Edit(ctx context.Context, chatID string, messageID int64, text string) error
}

// telegramSenderImpl is a small adapter that satisfies the
// telegramSender interface from two loose function values. Used
// at the chain / appendSegment call sites so the chain primitives
// depend on the interface (not loose fns) — keeps the Layer 5
// abstraction coherent even though the existing function
// signatures still take loose fns for legacy reasons.
type telegramSenderImpl struct {
	send sendChunkFn
	edit editChunkFn
}

func (s telegramSenderImpl) Send(ctx context.Context, chatID string, topicID int, replyToMessageID int, text string) (int64, error) {
	return s.send(ctx, chatID, topicID, replyToMessageID, text)
}

func (s telegramSenderImpl) Edit(ctx context.Context, chatID string, messageID int64, text string) error {
	return s.edit(ctx, chatID, messageID, text)
}

// sendChunkFn / editChunkFn remain as the underlying function
// types — the existing appendSegment / flushChainNow signatures
// take these loose fns. New code paths (test doubles, future
// refactors) can wrap them with telegramSenderImpl and depend
// on the interface. Adapter.chainSendFn / chainEditFn continue
// to satisfy this contract.
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
	// The new chunk INHERITS cur.headerLine (the previous chunk's
	// last headerLine — the latest heartbeat snapshot or the
	// cold-create banner). The user's chat view therefore stays
	// visually continuous across the page break: the new chunk's
	// first line is the same status line that was just rendered
	// on the previous chunk, and the next OutHeartbeat will
	// patch it forward to a fresh snapshot. We intentionally do
	// NOT use a '📄 (continued)' sentinel — the user perceives the
	// chain as one timeline, not a paginated document.
	cur.markFull()
	inheritedHeader := cur.headerText()
	newChunk := newChunkBody(0, inheritedHeader)
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
	// becomes the new active chunk. entries stays empty until
	// the next append; flushedLen tracks how much of the
	// post-render (buf unknown) was already emitted so future
	// renders over the new tail don't include it.
	if len(pieces) > 1 {
		lastPiece := pieces[len(pieces)-1]
		mid, err := sendFn(ctx, chatID, topicID, userMessageID, lastPiece)
		if err != nil {
			return err
		}
		tail := newChunkBody(int64(mid), cur.headerText())
		tail.markFlushedLen(len(lastPiece))
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

// renderChunkBody is the unified chunk-body composer used by every
// outgoing Telegram path that needs a chunk's full body. It is a
// thin wrapper around chunkBody.Compose() that takes string
// components instead of *chunkBody (used by tests that don't need
// the OOP wrapper). Production code calls chunkBody.Compose()
// directly — this wrapper stays for legacy callers and tests.
//
// The three inputs are routed through different pipelines so
// each section lands in the right form for parse_mode=HTML:
//
//   - headerLine: pre-baked HTML (e.g. '<b>💭 N · 🔧 M</b> · ⏱
//     HH:MM:SS' from heartbeatText). Written verbatim. If we
//     escape it, Telegram would render the tags as literal text.
//
//   - buf: plain text accumulated from formatted Out* events
//     (formatTool / formatReply / formatTaskList / etc.). Run
//     through RenderMarkdown which escapes '<', '>', '&' and
//     converts light markdown (headings, lists, code fences,
//     blockquotes) to HTML.
//
//   - footer: statusbar.RenderPanel output is already escape-safe
//     (no HTML chars in StatusBar lines). Written verbatim.
//
// When flushedRenderedLen > 0 (overflow chain), only the
// un-emitted tail of buf is rendered.
func renderChunkBody(headerLine, buf, footer string, flushedRenderedLen int) string {
	var b strings.Builder
	b.WriteString(headerLine)
	b.WriteByte('\n')
	if buf != "" {
		b.WriteString("────────\n")
		var bufSrc string
		if flushedRenderedLen > 0 && flushedRenderedLen < len(buf) {
			bufSrc = buf[flushedRenderedLen:]
		} else {
			bufSrc = buf
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
	if footer != "" {
		b.WriteByte('\n')
		b.WriteString(footer)
	}
	return b.String()
}

// renderActiveChunkBody is the wrapper used inside the chain
// package. Pass-through to chunkBody.Compose() with the footer
// rendered via statusbar.RenderPanel. Kept as a separate function
// so flushChainNow (and any chain-aware caller) doesn't need to
// know the composition rules — they just hand over the chunk.
func renderActiveChunkBody(cur *chunkBody, lastFooter []string) string {
	if len(lastFooter) > 0 {
		cur.setFooter(statusbar.RenderPanel(lastFooter))
	}
	return cur.Compose()
}
