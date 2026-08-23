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
type chunkBody struct {
	messageID int64
	isFull    bool
	header    string

	entries []chunkEntry
	footer  string

	flushedLen int
}

// newChunkBody creates a fresh chunkBody with the given header and
// messageID. No entries / footer yet — business code mutates those
// via the methods below.
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

// setHeader replaces the chunk's status header. Used by
// patchChainHeader on every OutHeartbeat and by cold-create.
func (b *chunkBody) setHeader(h string) { b.header = h }

// header returns the current header. Used by chain rotation to
// inherit the previous chunk's last state on overflow.
func (b *chunkBody) headerText() string { return b.header }

// appendEntry adds a plain-text segment. The text goes through
// RenderMarkdown at Compose time. Multi-line strings are accepted.
func (b *chunkBody) appendEntry(text string) {
	b.entries = append(b.entries, chunkEntry{text: text})
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
func (b *chunkBody) Compose() string {
	var out strings.Builder
	out.WriteString(b.header)
	out.WriteByte('\n')

	if len(b.entries) > 0 {
		out.WriteString("────────\n")
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
	if b.footer != "" {
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
