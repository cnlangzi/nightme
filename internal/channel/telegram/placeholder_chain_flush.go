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
// chainLRU.getOrCreate. They do NOT talk to the Telegram API directly —
// that responsibility lives on the Adapter (which owns the apiClient).
// The Adapter is expected to pass a sender closure into appendSegment
// so this file stays pure data-structures.
// ---------------------------------------------------------------------------

// sendChunkFn is the abstraction over the Telegram sendMessage call.
// Adapter supplies the concrete implementation in commit #5.
//
// Returns the new message ID. Errors propagate up through appendSegment.
type sendChunkFn func(
	ctx context.Context,
	chatID string,
	topicID int,
	replyToMessageID int,
	text string,
) (int64, error)

// editChunkFn is the abstraction over the Telegram editMessageText call.
// The Adapter implementation handles debounce timing and rate limiting.
type editChunkFn func(
	ctx context.Context,
	chatID string,
	messageID int64,
	text string,
) error

// chainChunkThresholdChars = raw-buffer ceiling per chunk.
// Above this, the next segment goes on a freshly-created chunk instead of
// the active one. See docs/channel/telegram.md §11.12.3.
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
// User-messageID is the user message that triggered this turn; every new
// chunk is reply_to=userMessageID so the chain hangs under the user's
// own message (per §11.12.11).
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

	// 1. Empty chain → materialise the first chunk via send.
	if chain.cursor < 0 {
		headerLine := placeholderInitialText(time.Now().UTC())
		body := headerLine + "\n" + segment
		if statusBarLines != nil {
			chain.lastFooter = statusBarLines
		}
		if chain.lastFooter != nil {
			body += "\n\n" + statusbar.RenderPanel(chain.lastFooter)
		}
		messageID, err := sendFn(ctx, chatID, topicID, userMessageID, body)
		if err != nil {
			return err
		}
		chain.chunks = []*placeholderChunk{{
			messageID:  messageID,
			headerLine: headerLine,
		}}
		chain.cursor = 0
		chain.dirty = false // freshly created; already aligned with Telegram
		return nil
	}

	// 2. Try to append on the active chunk.
	cur := chain.chunks[chain.cursor]
	if !cur.isFull && cur.charCount+len(segment)+1 <= chainChunkThresholdChars {
		cur.buf.WriteString(segment)
		cur.buf.WriteByte('\n')
		cur.charCount += len(segment) + 1
		if statusBarLines != nil {
			chain.lastFooter = statusBarLines
		}
		chain.dirty = true
		return nil
	}

	// 3. Active chunk is full → lock it and either recycle the next
	// slot in the existing slice or materialise a fresh chunk.
	cur.isFull = true

	if chain.cursor+1 < len(chain.chunks) {
		// Existing next chunk (rare — only after a forced hydrate
		// pre-seeds multiple chunks). Advance cursor; segment is now
		// appended below in the recursive case.
		chain.cursor++
		return appendSegment(
			ctx, chain,
			chatID, topicID, userMessageID,
			segment, statusBarLines,
			sendFn, editFn,
		)
	}

	// 4. Materialise a new chunk (the common cold case).
	header := "📄 (continued)"
	body := header + "\n────────\n" + segment
	if statusBarLines != nil {
		chain.lastFooter = statusBarLines
	}
	if chain.lastFooter != nil {
		body += "\n\n" + statusbar.RenderPanel(chain.lastFooter)
	}
	messageID, err := sendFn(ctx, chatID, topicID, userMessageID, body)
	if err != nil {
		// Roll back the cursor if we already advanced; otherwise the
		// chain.cursor points at the freshly-locked chunk. (Defensive —
		// the appendSegment contract guarantees cursor hasn't advanced
		// yet at this point in the cold path.)
		return err
	}
	chain.chunks = append(chain.chunks, &placeholderChunk{
		messageID:  messageID,
		headerLine: header,
	})
	chain.cursor = len(chain.chunks) - 1
	chain.dirty = false // fresh chunk is already aligned
	return nil
}

// flushChainNow synchronously renders the active chunk's text and
// pushes it via editChunkFn. No-op when the chain is clean.
//
// Long-text guard: if the rendered body exceeds maxTelegramTextLength
// (3900 chars), the body is split via splitTelegramText into multiple
// messages. The first piece is edited onto the active chunk; the
// remainder becomes NEW messages (sent via sendFn-style logic — but
// inline here since we don't need the reply-chain semantics for the
// tail pieces; they hang off the active chunk directly via consecutive
// sends). The active chunk's messageID is updated to point at the last
// piece so subsequent PATCHes continue on the tail.
//
// This is the safety valve for the rare single-segment overflow path.
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

	body := renderActiveChunkBody(cur, chain.lastFooter)
	rendered, err := RenderMarkdown(body)
	if err != nil {
		// Mirror the existing adapter escapeHTML fallback (see
		// renderBodyWithStatusBar in adapter.go).
		rendered = escapeHTML(body)
	}

	if len(rendered) <= maxTelegramTextLength {
		if err := editFn(ctx, chatID, cur.messageID, rendered); err != nil {
			return err
		}
		chain.dirty = false
		return nil
	}

	// Long-text overflow path. Split, edit first, send the rest as
	// continuation messages after the active chunk in the chat.
	pieces, err := splitTelegramText(rendered, maxTelegramTextLength)
	if err != nil {
		return err
	}
	var lastID int64 = cur.messageID
	for i, p := range pieces {
		if i == 0 {
			if err := editFn(ctx, chatID, cur.messageID, p); err != nil {
				return err
			}
			lastID = cur.messageID
			continue
		}
		// Continuation: a fresh message hanging off the previous one
		// via reply_to_message_id. We use userMessageID for the FIRST
		// continuation (so it stays in the reply chain under the user's
		// original message) and the previous piece's id for the rest.
		replyTo := userMessageID
		if i > 1 {
			replyTo = int(lastID)
		}
		mid, err := sendFn(ctx, chatID, topicID, replyTo, p)
		if err != nil {
			return err
		}
		lastID = mid
	}
	cur.messageID = lastID
	chain.dirty = false
	return nil
}

// scheduleFlushDebounced arms (or resets) a 250ms debounce timer that
// will invoke flushChainNow. Bursts of appendSegment calls within the
// window coalesce into a single editMessageText — see docs §11.12.7.
//
// editFn and sendFn are passed in by the caller (the Adapter wraps its
// apiClient + rate-limiter pipeline; tests supply test doubles). The
// closures do NOT capture the request's context.Background() ctx because
// the debounce fires 250ms+ after the request may have returned
// (request ctx cancelled). The timer creates a fresh background ctx
// with a 5s timeout.
//
// chain.debounceTimer access is serialised under chain.mu so the
// Stop/Replace pair is atomic with respect to other appends.
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
	if chain.debounceTimer != nil {
		chain.debounceTimer.Stop()
	}
	chain.debounceTimer = time.AfterFunc(250*time.Millisecond, func() {
		// Fresh ctx: the original request that scheduled this
		// flush may have already finished (timing window is
		// 250ms+; Send returns synchronously after the in-memory
		// append, but the timer is keyed off that exact moment).
		// The 5s budget is enough for one Telegram edit round-trip.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = flushChainNow(ctx, chain, chatID, topicID, userMessageID,
			editFn, sendFn)
	})
	chain.mu.Unlock()
}

// renderActiveChunkBody builds the (raw, pre-RenderMarkdown) text
// content for the active chunk: header + entries + footer.
func renderActiveChunkBody(cur *placeholderChunk, lastFooter []string) string {
	var b strings.Builder
	b.WriteString(cur.headerLine)
	b.WriteByte('\n')
	if cur.charCount > 0 {
		b.WriteString("────────\n")
		b.WriteString(cur.buf.String())
		if !strings.HasSuffix(cur.buf.String(), "\n") {
			b.WriteByte('\n')
		}
	}
	if len(lastFooter) > 0 {
		b.WriteByte('\n')
		b.WriteString(statusbar.RenderPanel(lastFooter))
	}
	return b.String()
}
