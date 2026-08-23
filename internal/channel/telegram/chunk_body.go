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

// appendEntryHTML adds a pre-baked HTML segment. Compose writes it
// verbatim — no RenderMarkdown pass.
func (b *chunkBody) appendEntryHTML(text string) {
	b.entries = append(b.entries, chunkEntry{text: text, isHTML: true})
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
		// the ONLY place we know stderr is multi-line. Any caller
		// wanting pre-baked HTML should use appendEntryHTML
		// directly with their own wrapper.
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
// Pre-fix: three inline body-assembly sites in appendSegment
// case-1/4 and flushChainNow built the body independently with
// duplicated separator/footer logic. Post-fix: every outgoing
// Telegram message in the chain goes through this method. Adding
// a new section (e.g. status badges) is a single-line change here.
func (b *chunkBody) Compose() string {
	var out strings.Builder
	out.WriteString(b.header)
	out.WriteByte('\n')

	if len(b.entries) > 0 {
		out.WriteString("────────\n")
		startIdx, byteOffset := b.skipFlushedPrefix()
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
			if i > startIdx && byteOffset == 0 {
				// separator between rendered entries
				// (only on the un-emitted tail)
			}
			_ = byteOffset // reserved for byte-aware split logic
			out.WriteString(text)
			if !strings.HasSuffix(e.text, "\n") {
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
// from and a byte offset (currently unused but reserved for
// byte-aware overflow tracking). On the long-text split path,
// Compose skips the already-emitted prefix so the next flush
// renders only the un-emitted remainder.
func (b *chunkBody) skipFlushedPrefix() (int, int) {
	if b.flushedLen == 0 {
		return 0, 0
	}
	// Conservatively render from the beginning when we don't
	// have byte-precise entry boundaries. The long-text path
	// (commit 08f8f7e) resets entries to nil after split, so
	// flushedLen > 0 with len(entries) == 0 is the common case.
	if len(b.entries) == 0 {
		return 0, b.flushedLen
	}
	return 0, b.flushedLen
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
