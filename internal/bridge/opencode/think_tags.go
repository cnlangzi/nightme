// Inline <think>...</think> extraction for opencode text content.
//
// opencode normally surfaces reasoning through the structured
// `reasoning` Part type that the translate table routes to
// EventAgentText with the [思考] prefix —
// gateway.outbound.Translate then routes those to OutThinking. In
// that flow the bridge never touches the wire's text_delta events
// for reasoning.
//
// But opencode can also emit reasoning inline as raw text wrapped in
// <think>...</think> tags inside a streamed `session.next.text.*`
// event, the same way Pi does (see commit 892bef3 + the F-OPENCODE
// trace that surfaced this; the chat client was showing the model's
// scratchpad verbatim in the reply bubble because session.next.text.*
// carried the tags through to OutReply). Two flavours observed:
//
//   - Single-block: one <think>...</think> inside one text_delta /
//     one text.ended snapshot. Easy to strip.
//   - Split-boundary: <think> opens at the tail of one
//     `session.next.text.delta` and </think> closes mid-way through
//     a later delta. The block straddles the wire's token boundary;
//     a naive per-delta scan misses it.
//
// splitThinking handles both. The caller (translate.go's
// session.next.text.delta handler) feeds deltas one at a time,
// prepending any Held partial from the previous call, and the
// splitter extracts whatever complete blocks it can while holding
// the trailing partial open for the next call. The text_delta
// handler routes the extracted thinking to the same [思考] surface
// the structured `reasoning` Part type uses, so downstream code
// sees one consistent channel.
//
// The same helper runs over a `text.started`/`text.ended` snapshot
// (rare in practice, but seen on non-streamed / replayed text
// blocks). handlePart's Type=="text" branch also handles
// contentBlock.Type=="reasoning" as a separate branch (opencode's
// wire-level type for fully-formed reasoning blocks).
package opencode

import "strings"

const (
	// thinkOpenTag / thinkCloseTag are the literal wire strings
	// emitted inline. Case-sensitive — opencode 1.18's prompt
	// transformers always emit lowercase, and the documented
	// upstream convention is the same.
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

// thinkSplitResult is the output of one splitThinking call. See
// splitThinking for the semantics of each field.
type thinkSplitResult struct {
	// Kept is the input minus every complete <think>...</think>
	// block. Goes to the reply textBuf.
	Kept string

	// Thinking is the concatenation (separated by '\n') of every
	// complete block's inner text, in document order. Emitted as
	// one EventAgentText per call, with the [思考] prefix the
	// gateway keys on for OutThinking.
	Thinking string

	// Held is a trailing <think>... with no matching </think> yet.
	// Empty when the input ended cleanly. The caller MUST prepend
	// it to the next delta for the same PartID so a block that
	// straddled two deltas still parses.
	Held string
}

// splitThinking scans s for <think>...</think> blocks and partitions
// it into reply text (Kept), extracted reasoning (Thinking), and
// any trailing partial open tag (Held).
//
// Behavior:
//
//   - Every complete block has its inner text appended to Thinking,
//     separated by '\n' so a multi-block stream still reads as
//     discrete paragraphs on the reasoning surface.
//   - Anything between blocks (and outside any block) goes to Kept.
//   - A trailing <think> with no matching </think> in s returns the
//     remainder (including the open tag itself) as Held.
//   - A bare </think> with no preceding <think> in s is preserved
//     as Kept text — opencode 1.18 does not emit stray close tags,
//     but the bridge must not eat user content if some future
//     variant does.
//   - Nested <think> inside <think> is treated as ordinary text
//     (the inline convention is flat; nested tags are ambiguous).
//
// The Held/Held + next-delta protocol is what makes the splitter
// safe across token-boundary splits: the wire can break the open
// tag's bytes across two deltas, and as long as both halves land
// in the same PartID we recover the block on the next call.
//
// Cost: linear in len(s) — strings.Index is a single pass per
// block, and the number of blocks per delta is small in practice
// (zero or one, occasionally two). Allocating two strings.Builder
// per call is the dominant cost; both stay small because the held
// remainder is bounded by the size of one think block.
func splitThinking(s string) thinkSplitResult {
	var (
		kept     strings.Builder
		thinking strings.Builder
	)

	for {
		openIdx := strings.Index(s, thinkOpenTag)
		if openIdx < 0 {
			// No opening tag in the remainder. Whatever is left
			// (text or stray close tags) is plain content.
			kept.WriteString(s)
			return thinkSplitResult{
				Kept:     kept.String(),
				Thinking: thinking.String(),
			}
		}

		// Emit the prefix as ordinary text and slice past the open
		// tag.
		kept.WriteString(s[:openIdx])
		rest := s[openIdx+len(thinkOpenTag):]

		closeIdx := strings.Index(rest, thinkCloseTag)
		if closeIdx < 0 {
			// Unclosed open tag — hand the remainder (with the
			// open tag re-attached) to the caller so it can be
			// prepended to the next chunk.
			return thinkSplitResult{
				Kept:     kept.String(),
				Thinking: thinking.String(),
				Held:     thinkOpenTag + rest,
			}
		}

		// Complete block: append inner text to Thinking, then
		// resume scanning after the close tag.
		block := rest[:closeIdx]
		if thinking.Len() > 0 {
			thinking.WriteByte('\n')
		}
		thinking.WriteString(block)
		s = rest[closeIdx+len(thinkCloseTag):]
	}
}
