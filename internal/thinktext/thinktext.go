// Package thinktext — the inline reasoning a model writes into its
// own text.
//
// Some models answer with their scratchpad wrapped in <think>…</think>
// tags instead of on a channel of their own: the tags arrive in the
// same text the reply carries, and the chat shows the scratchpad
// verbatim where the answer should be. The Pi bridge and the personal
// agent both meet that shape, and both have to take the tags apart the
// same way — two copies of a tag parser drift.
//
// Split serves the streaming case: the caller feeds it one chunk at a
// time and carries the Held remainder over to the next chunk, so a
// block that straddles a chunk boundary is still recognised.
//
// Unwrap serves the complete-text case: the text arrives whole and
// the caller wants the reasoning back with the tags taken out.
package thinktext

import "strings"

const (
	// openTag / closeTag are the literal wire strings emitted inline.
	// Case-sensitive — the models that emit them write them lowercase,
	// and the convention is the same everywhere.
	openTag  = "<think>"
	closeTag = "</think>"
)

// Result is what one Split call found.
type Result struct {
	// Kept is the text outside the reasoning blocks.
	Kept string

	// Thinking is the concatenation (separated by '\n') of every
	// complete block's inner text, in document order.
	Thinking string

	// Held is a trailing block that was opened but not closed yet.
	// Empty when the input ended cleanly. The caller MUST prepend it
	// to the next chunk for the same content so a block that straddled
	// two chunks still parses.
	Held string
}

// Split scans s for <think>…</think> blocks and partitions it into
// the text outside them (Kept), the reasoning inside them (Thinking)
// and a trailing block that was never closed (Held).
//
// Behavior:
//
//   - Every complete block has its inner text appended to Thinking,
//     separated by '\n' so a multi-block stream still reads as
//     discrete paragraphs on the reasoning surface.
//   - Anything between blocks (and outside any block) goes to Kept.
//   - A trailing <think> with no matching </think> in s returns the
//     remainder (including the open tag itself) as Held.
//   - A bare </think> with no preceding <think> in s is preserved as
//     Kept text — a model never emits a stray close tag, but the
//     parser must not eat user content if some future variant does.
//   - A nested <think> inside <think> is treated as ordinary text
//     (the inline convention is flat; nested tags are ambiguous).
//
// The Held/Held + next-chunk protocol is what makes the splitter safe
// across chunk boundaries: the wire can break the open tag's bytes
// across two chunks, and as long as both halves land in the same
// content the block is recovered on the next call.
//
// Cost: linear in len(s) — strings.Index is a single pass per block,
// and the number of blocks per chunk is small in practice.
func Split(s string) Result {
	var (
		kept     strings.Builder
		thinking strings.Builder
	)

	for {
		block, ok := nextBlock(s)
		if !ok {
			// No opening tag in the remainder. Whatever is left
			// (text or stray close tags) is plain content.
			kept.WriteString(s)
			return Result{
				Kept:     kept.String(),
				Thinking: thinking.String(),
			}
		}

		// Emit the prefix as ordinary text and slice past the open
		// tag.
		kept.WriteString(s[:block.open])
		if !block.closed {
			// Unclosed open tag — hand the remainder (with the
			// open tag re-attached) to the caller so it can be
			// prepended to the next chunk.
			return Result{
				Kept:     kept.String(),
				Thinking: thinking.String(),
				Held:     s[block.open:],
			}
		}

		// Complete block: append inner text to Thinking, then
		// resume scanning after the close tag.
		if thinking.Len() > 0 {
			thinking.WriteByte('\n')
		}
		thinking.WriteString(block.content)
		s = s[block.end:]
	}
}

// Unwrap returns s with the reasoning tags taken out and the content
// of the blocks left where it was.
//
// The reasoning stays in the text: the caller asked for the tags to
// go, not for the scratchpad to disappear.
//
// A block that was never closed loses its opening tag too: the rest
// of the text is still the reasoning.
func Unwrap(s string) string {
	var b strings.Builder

	for {
		block, ok := nextBlock(s)
		if !ok {
			// No opening tag in the remainder: whatever is left
			// (text or stray close tags) is plain content.
			b.WriteString(s)
			return b.String()
		}

		b.WriteString(s[:block.open])
		b.WriteString(block.content)
		if !block.closed {
			// Unclosed open tag: the content stays, the tag goes.
			return b.String()
		}
		s = s[block.end:]
	}
}

// block is one reasoning block of a text.
type block struct {
	// open is the index of the opening tag.
	open int
	// end is the index just past the closing tag.
	end int
	// content is the text between the tags.
	content string
	// closed reports whether the block has a closing tag.
	closed bool
}

// nextBlock finds the first reasoning block of s.
//
// It reports false when s carries no opening tag at all: the rest of
// the text is plain content.
func nextBlock(s string) (block, bool) {
	open := strings.Index(s, openTag)
	if open < 0 {
		return block{}, false
	}
	rest := s[open+len(openTag):]
	closeIdx := strings.Index(rest, closeTag)
	if closeIdx < 0 {
		return block{open: open, end: len(s), content: rest}, true
	}
	return block{
		open:    open,
		end:     open + len(openTag) + closeIdx + len(closeTag),
		content: rest[:closeIdx],
		closed:  true,
	}, true
}
