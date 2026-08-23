package telegram

import (
	"strings"
)

// ---------------------------------------------------------------------------
// chunkBody — one Telegram message in the rolling-log chain. Encapsulates
// the three pipeline-routed sections (header / entries / footer) and is
// the SOLE producer of safe-HTML wire format via Compose(). Business
// code mutates the fields; Compose() renders.
//
// Layer 1: data model + view formatting, encapsulated.
// Layer 2 (chain) drives mutations through business methods.
// Layer 3 (Sender) ships the rendered bytes to Telegram.
//
// This split keeps adapter.go free of HTML decisions and ensures
// any future change to chunk-body composition happens in one place.
// ---------------------------------------------------------------------------

// chunkEntry is one log line within a chunkBody. isHTML=true means
// the text is already safe HTML (e.g. <pre>...</pre>) and must skip
// RenderMarkdown's escapeHTML pass — that's how OutError's
// ```fences``` content flows through without double-escape.
type chunkEntry struct {
	text   string
	isHTML bool
}

// chunkBody is one Telegram message. Business code mutates header,
// entries, footer; Compose() is the only render path.
//
// flushedLen tracks how many bytes of entries' rendered content
// were emitted on the previous long-text split (overflow path).
// Compose skips this prefix so the tail's renderText isn't
// shorter than the body actually shipped to Telegram.
//
// hasHeartbeat gates Compose's header-rendering decision. Cold-create
// (ensurePlaceholder / appendSegment cold-create / split overflow)
// starts it false; patchChainHeader flips it true on the first
// OutHeartbeat, and Compose then ALWAYS renders the header. The
// "render header iff hasHeartbeat || entries empty" rule is what
// hides the legacy "🤖 Working..." banner on turn paths that produce
// body content but no heartbeat (slash command replies, error-only
// turns, reaction-only clicks). See docs/channel/telegram.md §11.12.5.
type chunkBody struct {
	messageID int64
	isFull    bool
	header    string

	// hasHeartbeat is true once any OutHeartbeat has patched this
	// chunk's header. Compose treats !hasHeartbeat && entries>0 as
	// "render body only" — the cold-create "🤖 Working..." header
	// disappears as soon as a real entry arrives, instead of
	// lingering forever on a turn that has no agent (slash
	// command / shell / WatchMode-rejected / spawn-failed).
	hasHeartbeat bool

	entries []chunkEntry
	footer  string

	flushedLen int
}

// newChunkBody creates a fresh chunkBody with the given header and
// messageID. No entries / footer yet; hasHeartbeat starts false so
// the cold-create "🤖 Working..." banner is rendered until an
// OutHeartbeat (or no entries at all) decides otherwise. Business
// code mutates those via the methods below.
func newChunkBody(messageID int64, header string) *chunkBody {
	return &chunkBody{
		messageID: messageID,
		header:    header,
	}
}

// --- Business mutations ------------------------------------------------
//
// None of these methods touch HTML, parse_mode, or any other view
// concern. They mutate the data model only.

// setHeader replaces the chunk's status header WITHOUT flipping
// hasHeartbeat. Cold-create uses this (heartbeatText(nil) banner)
// so the chunk still respects "render header only if entries
// empty" rule and can be replaced by the first real flush without
// lingering.
func (b *chunkBody) setHeader(h string) { b.header = h }

// setHeaderFromHeartbeat replaces the chunk's status header AND
// flips hasHeartbeat=true. patchChainHeader calls this on every
// OutHeartbeat so subsequent Compose() renders the header
// unconditionally — the agent is alive, show its counts.
// Equivalent to setHeader + a "this is real heartbeat info"
// marker. Cold-create must NOT call this; it would defeat the
// skip-header rule for non-agent turns.
func (b *chunkBody) setHeaderFromHeartbeat(h string) {
	b.header = h
	b.hasHeartbeat = true
}

// header returns the current header. Used by chain rotation to
// inherit the previous chunk's last state on overflow.
func (b *chunkBody) headerText() string { return b.header }

// inheritLatestHeader copies src's (header, hasHeartbeat) pair
// onto this chunk. Used by overflow / split / rotate / tail paths
// in placeholder_chain_flush.go so every newly-created chunk
// reads as a chronological snapshot of the chain's active state
// at the moment it was born — not a cold "🤖 Working..." banner.
//
// E.g. agent emits "💭 5 · 🔧 2" heartbeat (chunk A.active),
// then a long OutThinking overflows: chunk B inherits A's header
// verbatim ("💭 5 · 🔧 2") and hasHeartbeat=true. Subsequent
// heartbeats update A.active to "💭 6 · 🔧 2", but B is frozen at
// "💭 5 · 🔧 2" — that's the intended chronological progression;
// broadcasting on heartbeat would cost N edits per N chunks and
// flatten the timeline.
//
// Caller MUST hold chain.mu (src is typically
// chain.chunks[chain.cursor] pre-markFull).
func (b *chunkBody) inheritLatestHeader(src *chunkBody) {
	if src == nil {
		return
	}
	b.header = src.header
	b.hasHeartbeat = src.hasHeartbeat
}

// appendEntry adds a plain-text segment. The text goes through
// RenderMarkdown at Compose time. Multi-line strings are accepted.
func (b *chunkBody) appendEntry(text string) {
	b.entries = append(b.entries, chunkEntry{text: text})
}

// appendEntryHTML adds an entry whose text is already rendered safe
// HTML (e.g. the output of RenderMarkdown from the SPLIT path in
// appendSegment / appendErrorSegment, §11.12.7.2 trigger 1).
// Compose() must NOT re-run RenderMarkdown on these entries or the
// pre-rendered tags would be double-escaped.
func (b *chunkBody) appendEntryHTML(text string) {
	b.entries = append(b.entries, chunkEntry{text: text, isHTML: true})
}

// replaceEntry overwrites the text of entries[idx]. Used by the
// OutToolStart → OutToolEnd merge path: the start body lands as a
// plain appendEntry, and the matching End mutates that same entry
// in place to `<start>\n<result>` so the two lines render as one
// chunkEntry (and therefore one Telegram message).
//
// Returns false when the rewrite can't be applied:
//   - entries is nil (chunk went through freezeAfterOverflow after
//     a long-text split; entries were cleared to free memory)
//   - idx is out of range (defensive — caller passed a stale index)
//
// The caller falls back to a fresh appendSegment for the result
// body in either case so the data is never silently dropped. We
// do NOT auto-create the entry — that would corrupt Compose's
// invariant that entries reflect only what the user explicitly
// emitted, and could leave the chunk's flushedLen inconsistent
// with what's actually on Telegram.
func (b *chunkBody) replaceEntry(idx int, text string) bool {
	if b.entries == nil {
		return false
	}
	if idx < 0 || idx >= len(b.entries) {
		return false
	}
	b.entries[idx].text = text
	return true
}

// appendError composes an OutError entry. text is the user-facing
// short error (e.g. "tool exit 1"); stderr is the optional
// diagnostic tail. The ```fences``` wrapper is intentionally
// inside this method — the format decision lives with the data
// model, not in adapter.
func (b *chunkBody) appendError(text, stderr string) {
	body := text
	if stderr != "" {
		// ```fences``` → RenderMarkdown → <pre>...</pre>. This is
		// the ONLY place we know stderr is multi-line.
		body += "\n\n```\n" + stderr + "\n```"
	}
	b.entries = append(b.entries, chunkEntry{text: body})
}

// setFooter replaces the statusbar footer snapshot. The next
// Compose() reads this verbatim (statusbar.RenderPanel output is
// already escape-safe).
func (b *chunkBody) setFooter(f string) { b.footer = f }

// markFull locks the chunk (no further appendEntry / appendError).
// chain.rotate() and long-text overflow paths use this.
func (b *chunkBody) markFull() { b.isFull = true }

// freezeAfterOverflow is invoked by the long-text split path:
//   - emit the rendered text up through flushedLen as pieces[0]
//   - clear the buf but keep headerLine and messageID
//   - record flushedLen so the next Compose() knows to skip the
//     emitted prefix
func (b *chunkBody) freezeAfterOverflow(emittedBytes int) {
	b.entries = nil
	b.flushedLen = emittedBytes
}

// markFlushedLen records how many bytes of rendered entries were
// emitted during a long-text split (P0 #2 infrastructure).
func (b *chunkBody) markFlushedLen(n int) { b.flushedLen = n }

// --- View rendering ---------------------------------------------------

// Compose returns the safe-HTML body for parse_mode=HTML send.
//
// Pipeline routing (do not change without re-reading §11.12):
//   - header:    verbatim (already HTML, e.g. "<b>...</b>")
//   - entries:   RenderMarkdown per entry (escape + light md →
//                HTML); isHTML entries write verbatim
//   - footer:    verbatim (statusbar.RenderPanel output)
//
// Inter-entry separator: every entry is followed by '\n'. If the
// entry already ends with '\n' the trailing-newline guard keeps
// the output tidy; if not, Compose adds it. Consecutive entries
// always have a single '\n' between them so the rendered body
// reads cleanly even when formatReply / formatTool produce
// single-line outputs without trailing newlines.
//
// P1 fix (2026-08-23): the prior "if i > startIdx && byteOffset
// == 0" block was dead code — byteOffset was always 0 from
// skipFlushedPrefix. Removed.
//
// Overflow handling: skipFlushedPrefix returns the entry index
// to start from. When flushedLen > 0 with no entries (the
// overflow path cleared entries via freezeAfterOverflow), we
// skip to the end and render only header + footer. The tail
// chunk of an overflow carries pieces[N-1] as its first entry
// (see flushChainNow) so the next flush re-renders with the
// long-text content intact — without that, the next editMessageText
// would overwrite pieces[N-1] with an empty body (P0 regression
// guard).
//
// Header-skip rule (§11.12.5): render the header IFF
// `hasHeartbeat || len(entries) == 0`.
//   - Cold-create, body empty → render "🤖 Working..." as the
//     alive signal (hasHeartbeat=false but entries empty).
//   - First entry arrives before any heartbeat → header is
//     "🤖 Working..." but hasHeartbeat=false AND entries>0, so
//     header is SKIPPED and the body renders on its own. This is
//     the case that fixes the legacy bug where slash commands /
//     reaction-only / WatchMode-rejected turns carried a frozen
//     "🤖 Working..." banner forever.
//   - First OutHeartbeat arrives (with or without entries) →
//     hasHeartbeat flips true → header renders thereafter.
//   - Footer (statusbar) renders IFF entries or header did
//     (skip the orphan-footer case — there's no header separator
//     line for it to flank against).
func (b *chunkBody) Compose() string {
	var out strings.Builder

	renderHeader := b.hasHeartbeat || len(b.entries) == 0
	if renderHeader {
		out.WriteString(b.header)
		out.WriteByte('\n')
	}

	if len(b.entries) > 0 {
		if renderHeader {
			// Separator is 16 chars of U+2500 box-drawing horizontal,
			// matching statusbar.PanelMaxWidth so the divider line
			// aligns with the footer's left/right brackets
			// ┌─...─› / └─...─› on either side of the chunk body.
			out.WriteString("────────────────\n")
		}
		startIdx := b.skipFlushedPrefix()
		for i := startIdx; i < len(b.entries); i++ {
			e := b.entries[i]
			text := e.text
			if !e.isHTML {
				rendered, err := RenderMarkdown(text)
				if err != nil {
					rendered = escapeHTML(text)
				}
				text = rendered
			}
			out.WriteString(text)
			// Trailing newline guard: ensure every entry ends
			// with '\n' so the next entry (or the separator
			// before footer) reads cleanly. With this guard the
			// inter-entry separator is implicitly '\n'.
			if !strings.HasSuffix(text, "\n") {
				out.WriteByte('\n')
			}
		}
	}
	// Footer: only render when at least one of (header, entries) did.
	// Otherwise the footer floats with no chunk context (cold-create
	// before any heartbeat or entry would emit an orphan trailing
	// panel — ugly).
	if b.footer != "" && (renderHeader || len(b.entries) > 0) {
		out.WriteByte('\n')
		out.WriteString(b.footer)
	}
	return out.String()
}

// skipFlushedPrefix returns the entry index to start rendering
// from. flushedLen is reserved for future byte-aware overflow
// tracking; today the overflow path clears entries via
// freezeAfterOverflow and re-seeds the tail chunk's entries
// with the pieces[N-1] content, so this is startIdx=len(entries)
// when the chunk is a frozen overflow tail.
func (b *chunkBody) skipFlushedPrefix() int {
	if b.flushedLen == 0 {
		return 0
	}
	// If entries is non-empty the chunk has been re-seeded
	// with new content; render from the beginning. If entries
	// is empty (frozen overflow tail), skip everything.
	if len(b.entries) == 0 {
		return len(b.entries)
	}
	return 0
}

// --- Convenience accessors ------------------------------------------

func (b *chunkBody) messageIDValue() int64 { return b.messageID }
func (b *chunkBody) isChunkFull() bool     { return b.isFull }

// entriesJoined returns the raw entries concatenated with \n
// separators — equivalent to the legacy `cur.buf.String()`.
// Used by tests and adapter-internal probes that want to inspect
// accumulated content without going through Compose.
func (b *chunkBody) entriesJoined() string {
	var sb strings.Builder
	for _, e := range b.entries {
		sb.WriteString(e.text)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// entriesSize returns the byte length of the entries (sum of
// entry text + separator). Equivalent to the legacy
// `cur.charCount`.
func (b *chunkBody) entriesSize() int { return b.bufTextSize() }
