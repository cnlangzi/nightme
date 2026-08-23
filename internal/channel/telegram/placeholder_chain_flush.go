package telegram

import (
	"context"
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

	// §11.12.7.2 trigger 1: single oversized segment → SPLIT at
	// append time. Without this, a 5000-char OutReply would push
	// a body over Telegram's 4096 hard limit and the first
	// sendMessage would be rejected by Telegram itself. The split
	// path mints N Telegram messages (one per piece) all carrying
	// the same headerLine (single heartbeatText(nil) call) so the
	// chat shows them as visually continuous, distinguished from
	// ROTATE chunks by their matching timestamps.
	if len(segment) > chainChunkThresholdChars {
		return splitOversizedSegmentLocked(ctx, chain, chatID, topicID, userMessageID, segment, sendFn)
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
		// P0 #1 fix: seed entries so the inlined segment is
		// re-rendered on every subsequent flush. Without this,
		// Compose() at the next flush would only render the
		// cold-create header (and a footer if set) — overwriting
		// the original message content with no body. The banner-
		// hide rule (§11.12.5) means the rendered cold-create
		// body is just the segment (header skipped because
		// entries>0 && !hasHeartbeat), which is what the user sees.
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
	// The new chunk inherits cur's (header, hasHeartbeat) pair via
	// inheritLatestHeader so each frozen chunk reads as a
	// chronological snapshot of the chain's active state at the
	// moment it was born. patchChainHeader continues to update only
	// chain.chunks[cursor] (the active chunk); frozen chunks stay
	// frozen at their snapshot timestamp. This is what gives the
	// chat scrollback a readable timeline of header snapshots
	// instead of a flat latest-banner — the inverse of "fresh
	// timestamp per message" semantics, by design (see §11.12.7.2
	// for the historical "creation time" rationale, superseded by
	// inherit-from-active as of 2026-08-23).
	cur.markFull()
	newChunk := newChunkBody(0, "")
	newChunk.inheritLatestHeader(cur)
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

// appendSegmentLocked is the lock-held variant of appendSegment for
// callers that already hold chain.mu. Used by the OutToolStart path:
// the Adapter needs to "append + record (chunkIdx, entryIdx) in the
// toolPending FIFO" atomically — splitting that across two lock
// windows would let an interleaved appendSegment steal the position
// we'd then record, leaving a Start that we can never rewrite on
// End. Caller MUST hold chain.mu; this function does NOT take it.
//
// Behaviour is byte-for-byte identical to appendSegment. The only
// differences are the missing `chain.mu.Lock(); defer Unlock()` pair
// and the return of the (chunkIdx, entryIdx) of the just-appended
// entry so the caller can stash it in the toolPending FIFO.
//
// (-1, -1) means the segment was rejected: either empty (caller
// pre-checks; mirrors appendSegment's silent drop), oversized so the
// SPLIT path took over and split across N chunks (no single entry
// to record — caller falls back to fresh appendSegment on End), or
// the underlying sendFn failed (returns (-1, -1); the caller's
// popToolStartEntry on End will miss → fall back to fresh append).
func appendSegmentLocked(
	ctx context.Context,
	chain *placeholderChain,
	chatID string,
	topicID int,
	userMessageID int,
	segment string,
	statusBarLines []string,
	sendFn sendChunkFn,
) (chunkIdx, entryIdx int) {
	if statusBarLines != nil {
		chain.lastFooter = statusBarLines
	}

	for chain.cursor >= 0 &&
		chain.chunks[chain.cursor].isChunkFull() &&
		chain.cursor+1 < len(chain.chunks) {
		chain.cursor++
	}

	// §11.12.7.2 trigger 1: oversized segment → SPLIT. Unreachable
	// in practice for OutTool segments (args capped at 100 bytes
	// by toolCallArgsMaxBytes), but defended here so the contract
	// matches appendSegment exactly. The SPLIT path produces
	// entries across multiple chunks — no single (chunkIdx, entryIdx)
	// to record, so we return (-1, -1) and let the caller fall back
	// to a fresh appendSegment on End.
	if len(segment) > chainChunkThresholdChars {
		_ = splitOversizedSegmentLocked(ctx, chain, chatID, topicID, userMessageID, segment, sendFn)
		return -1, -1
	}

	// 1. Empty chain → materialise the first chunk via send.
	if chain.cursor < 0 {
		headerLine := heartbeatText(nil)
		chunk := newChunkBody(0, headerLine)
		chunk.appendEntry(segment)
		if chain.lastFooter != nil {
			chunk.setFooter(statusbar.RenderPanel(chain.lastFooter))
		}
		body := chunk.Compose()
		messageID, err := sendFn(ctx, chatID, topicID, userMessageID, body)
		if err != nil {
			return -1, -1
		}
		chunk.messageID = messageID
		chain.chunks = []*chunkBody{chunk}
		chain.cursor = 0
		chain.dirty = true
		return 0, len(chunk.entries) - 1
	}

	// 2. Try to append on the active chunk.
	cur := chain.chunks[chain.cursor]
	if !cur.isChunkFull() && cur.bufTextSize()+len(segment)+1 <= chainChunkThresholdChars {
		cur.appendEntry(segment)
		chain.dirty = true
		return chain.cursor, len(cur.entries) - 1
	}

	// 3. Overflow → lock current chunk and materialise a fresh one.
	cur.markFull()
	newChunk := newChunkBody(0, "")
	newChunk.inheritLatestHeader(cur)
	newChunk.appendEntry(segment)
	if chain.lastFooter != nil {
		newChunk.setFooter(statusbar.RenderPanel(chain.lastFooter))
	}
	body := newChunk.Compose()
	messageID, err := sendFn(ctx, chatID, topicID, userMessageID, body)
	if err != nil {
		return -1, -1
	}
	newChunk.messageID = messageID
	chain.chunks = append(chain.chunks, newChunk)
	chain.cursor = len(chain.chunks) - 1
	chain.dirty = true
	return chain.cursor, len(newChunk.entries) - 1
}

// flushChunkAt recomposes chain.chunks[idx] and PATCHes its messageID.
// Used by the OutToolStart → OutToolEnd merge path when the rewrite
// lands in an earlier chunk (chain.cursor has advanced past it
// because an intervening event rotated the chain). Mirrors
// flushChainNow's "render + editFn" core but targets a specific
// chunk instead of the active one — flushChainNow assumes the
// cursor's chunk is the dirty one, which isn't true after rotation.
//
// Footer semantics: Compose() reads chain.lastFooter (the CURRENT
// footer snapshot) every time it renders, so flushChunkAt will
// PATCH the historical chunk with whatever footer is current. For
// the cross-chunk tool-merge case this means a chunk that was
// originally sent with footer F1 may get PATCHed with F2 — i.e.
// the user sees the older message's footer "update" to match the
// newer chunks. This is intentional and matches flushChainNow's
// behaviour on the active chunk (which re-renders with the current
// footer on every heartbeat-driven flush, so users already see
// footers tick forward). The end state — all visible chunks show
// the same footer at the same wall time — is actually MORE
// consistent than the pre-fix behaviour, where the earlier chunk
// would stay frozen at F1 while the active chunk advanced to F2.
//
// Caller MUST hold chain.mu; this function does NOT take it. The
// single caller (Adapter.Send's OutToolEnd path) wraps both the
// rewrite and this flush in one chain.mu critical section.
//
// Edge cases:
//   - idx out of range or chunk has no messageID yet (cold-create
//     pending — shouldn't happen since pushToolStartEntry only fires
//     after a successful appendSegmentLocked send, but defensive):
//     silently no-op.
//   - rendered body exceeds maxTelegramTextLength (rare — would
//     require a long start+result pair to push past 4096 after
//     markdown expansion): truncate via splitTelegramText and PATCH
//     the head piece. We avoid the full overflow-rotate dance here
//     because the chunk's history is already shipped to Telegram;
//     truncating preserves the caller's invariant that "later
//     PATCHes never delete earlier shipped content".
func flushChunkAt(
	ctx context.Context,
	chain *placeholderChain,
	idx int,
	chatID string,
	_ int, // topicID unused: editFn is chat-scoped
	_ int, // userMessageID unused: editFn is chat+message-scoped, not thread-scoped
	editFn editChunkFn,
) error {
	if idx < 0 || idx >= len(chain.chunks) {
		return nil
	}
	target := chain.chunks[idx]
	if target.messageID == 0 {
		// Cold-create pending — never sent. Nothing to PATCH;
		// the next flushChainNow will pick up the rewritten entry.
		return nil
	}

	target.setFooter(statusbar.RenderPanel(chain.lastFooter))
	rendered := target.Compose()

	if len(rendered) <= maxTelegramTextLength {
		return editFn(ctx, chatID, target.messageIDValue(), rendered)
	}

	// Overflow during tool-merge PATCH: splitTelegramText preserves
	// line breaks; we PATCH the head onto the existing messageID
	// and the original chunks[] state stays intact so a later
	// rewrite doesn't lose information. Truncating is a soft
	// degradation — args are capped at 100 bytes by
	// toolCallArgsMaxBytes, so reaching this branch means the
	// chunk's accumulated content (not the tool line itself) is
	// the dominant share, and the user's view is no worse than
	// what the existing flushChainNow overflow path already does.
	pieces, err := splitTelegramText(rendered, maxTelegramTextLength)
	if err != nil {
		return err
	}
	if len(pieces) == 0 {
		return nil
	}
	return editFn(ctx, chatID, target.messageIDValue(), pieces[0])
}

// splitOversizedSegmentLocked handles §11.12.7.2 trigger 1: a single
// segment is too long to fit in one Telegram message. The split
// happens at append time (not flush time) because flushChainNow only
// sees the rendered body AFTER sendMessage has already succeeded —
// for an oversized raw segment, sendMessage itself is rejected by
// Telegram's 4096 hard limit and we never reach flush.
//
// Behaviour:
//   - The segment is rendered via RenderMarkdown ONCE, then split via
//     splitTelegramText (line boundaries where possible, hard cut on
//     single lines that exceed maxTelegramTextLength).
//   - Each piece becomes its own Telegram message and its own chain
//     chunk. Pieces 1..N-1 are frozen (markFull); piece N is the new
//     active chunk and accepts subsequent appendSegment calls.
//   - All chunks share the same headerLine from a single
//     heartbeatText(nil) call, distinguishing SPLIT chunks visually
//     from ROTATE chunks (whose timestamps differ).
//   - All chunks share the same chain.lastFooter snapshot taken
//     before the split. lastFooter is NOT refreshed during the split
//     because the same OutboundMessage drives every piece; if the
//     caller wanted to refresh, they would pass new statusBarLines
//     on the next event, not this one.
//
// Partial-failure semantics: sendFn at piece k failing returns the
// error. The chain.chunks slice is unmodified (no chunks appended
// for any piece), so the chain is left in its pre-call state. The
// first k-1 pieces that did reach Telegram remain in chat as orphan
// history; subsequent appendSegment calls fall through to case 2/3
// on the existing active chunk (which was markFull'd at step 3, so
// case 2 misses and case 3 ROTATEs to a fresh chunk).
//
// Caller MUST hold chain.mu.
func splitOversizedSegmentLocked(
	ctx context.Context,
	chain *placeholderChain,
	chatID string,
	topicID int,
	userMessageID int,
	segment string,
	sendFn sendChunkFn,
) error {
	rendered, err := RenderMarkdown(segment)
	if err != nil {
		rendered = escapeHTML(segment)
	}

	pieces, err := splitTelegramText(rendered, maxTelegramTextLength)
	if err != nil {
		return err
	}

	// Capture the active source chunk BEFORE markFull so we can
	// inherit its (header, hasHeartbeat) snapshot onto each split
	// piece. Frozen SPLIT pieces keep their at-creation snapshot
	// forever (see ROTATE semantics in appendSegment case 3).
	var inheritFrom *chunkBody
	if chain.cursor >= 0 {
		inheritFrom = chain.chunks[chain.cursor]
		inheritFrom.markFull()
	}

	footerSnapshot := chain.lastFooter

	newChunks := make([]*chunkBody, 0, len(pieces))
	for i, p := range pieces {
		// Cold-construct with empty header so adopt doesn't
		// overwrite a non-empty header with an empty source.
		ch := newChunkBody(0, "")
		ch.inheritLatestHeader(inheritFrom)
		ch.appendEntryHTML(p)
		if footerSnapshot != nil {
			ch.setFooter(statusbar.RenderPanel(footerSnapshot))
		}
		body := ch.Compose()
		mid, err := sendFn(ctx, chatID, topicID, userMessageID, body)
		if err != nil {
			// Partial failure: don't touch chain.chunks / cursor /
			// dirty. The markFull on the prior active chunk above
			// already happened; that doesn't affect subsequent
			// chain writes (case 3 ROTATE just creates a fresh
			// chunk regardless).
			return err
		}
		ch.messageID = mid
		// pieces 1..N-1 are frozen; piece N stays active.
		if i < len(pieces)-1 {
			ch.markFull()
		}
		newChunks = append(newChunks, ch)
	}

	chain.chunks = append(chain.chunks, newChunks...)
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
		// The tail chunk inherits cur's (header, hasHeartbeat)
		// snapshot so it doesn't restart at the cold "🤖
		// Working..." banner when a real heartbeat has already
		// landed. cur was frozen above; its (header, hasHeartbeat)
		// are still readable for the adopt.
		tail := newChunkBody(int64(mid), "")
		tail.inheritLatestHeader(cur)
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

	// §11.12.7.2 trigger 1 for OutError: an OutError whose
	// rendered body (text + ```fences``` + stderr) exceeds
	// chainChunkThresholdChars is split at append time, mirroring
	// splitOversizedSegmentLocked for plain segments. estimateErrorSize
	// is a rough pre-check — the real ceiling is enforced by
	// Compose() once we know the exact fence overhead.
	if estimateErrorSize(stderr) > chainChunkThresholdChars {
		return splitOversizedErrorSegmentLocked(ctx, chain, chatID, topicID, userMessageID,
			text, stderr, sendFn)
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

	// Overflow: rotate. New chunk inherits cur's (header, hasHeartbeat)
	// so the OutError overflow chunk reads as a chronological snapshot
	// of the chain's active state at the moment the error overflowed
	// — same semantics as the plain appendSegment case 3 ROTATE.
	cur.markFull()
	newChunk := newChunkBody(0, "")
	newChunk.inheritLatestHeader(cur)
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

// splitOversizedErrorSegmentLocked mirrors splitOversizedSegmentLocked
// for the OutError path. Same trigger (single entry body exceeds the
// buffer threshold), same split semantics (N Telegram messages,
// pieces 1..N-1 frozen, piece N active, all sharing the same
// headerLine).
//
// The split happens on the rendered body — that is, the body each
// Telegram message will actually carry after markdown rendering and
// fence wrapping. Each piece becomes its own chunk via
// appendEntryHTML so Compose() doesn't re-render it.
//
// Caller MUST hold chain.mu.
func splitOversizedErrorSegmentLocked(
	ctx context.Context,
	chain *placeholderChain,
	chatID string,
	topicID int,
	userMessageID int,
	text, stderr string,
	sendFn sendChunkFn,
) error {
	// Build the body the same way chunkBody.appendError would:
	// text + fence + stderr + fence, then run RenderMarkdown once.
	body := text
	if stderr != "" {
		body += "\n\n```\n" + stderr + "\n```"
	}
	rendered, err := RenderMarkdown(body)
	if err != nil {
		rendered = escapeHTML(body)
	}

	pieces, err := splitTelegramText(rendered, maxTelegramTextLength)
	if err != nil {
		return err
	}

	// Capture source BEFORE markFull for inheritLatestHeader.
	var inheritFrom *chunkBody
	if chain.cursor >= 0 {
		inheritFrom = chain.chunks[chain.cursor]
		inheritFrom.markFull()
	}

	footerSnapshot := chain.lastFooter

	newChunks := make([]*chunkBody, 0, len(pieces))
	for i, p := range pieces {
		ch := newChunkBody(0, "")
		ch.inheritLatestHeader(inheritFrom)
		ch.appendEntryHTML(p)
		if footerSnapshot != nil {
			ch.setFooter(statusbar.RenderPanel(footerSnapshot))
		}
		chunkBody := ch.Compose()
		mid, err := sendFn(ctx, chatID, topicID, userMessageID, chunkBody)
		if err != nil {
			return err
		}
		ch.messageID = mid
		if i < len(pieces)-1 {
			ch.markFull()
		}
		newChunks = append(newChunks, ch)
	}

	chain.chunks = append(chain.chunks, newChunks...)
	chain.cursor = len(chain.chunks) - 1
	chain.dirty = true
	return nil
}
