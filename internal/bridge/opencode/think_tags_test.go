// Tests for inline <think>...</think> extraction.
//
// opencode normally surfaces reasoning via the structured `reasoning`
// Part type (handled in translate_test.go). This file covers the
// fallback path: when opencode emits reasoning inline as raw text
// wrapped in <think>...</think> tags inside a streamed
// `session.next.text.delta` event, the bridge must strip the tags
// and route the content to the [思考] surface instead of letting it
// leak into the reply text. Mirrors the pi bridge's think_tags_test.go
// because the wire-level behaviour is the same family (splitThinking
// is the same shape as pi's, only the routing event differs).
//
// Coverage:
//
//   - splitThinking: pure-function unit tests for the splitter
//     itself (no translator state). Locks the Held protocol that
//     keeps split-boundary blocks whole.
//
// The end-to-end text_delta / text_end paths (with the translate
// table wired up to the splitter) live in translate_test.go,
// alongside the buffer-flush / Done-trigger tests.
package opencode

import (
	"strings"
	"testing"
)

func TestSplitThinking_PlainTextUnchanged(t *testing.T) {
	got := splitThinking("hello world")
	if got.Kept != "hello world" {
		t.Errorf("Kept = %q, want %q", got.Kept, "hello world")
	}
	if got.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", got.Thinking)
	}
	if got.Held != "" {
		t.Errorf("Held = %q, want empty", got.Held)
	}
}

func TestSplitThinking_SingleBlock(t *testing.T) {
	got := splitThinking("hello <think>reasoning</think> world")
	if got.Kept != "hello  world" {
		t.Errorf("Kept = %q, want %q", got.Kept, "hello  world")
	}
	if got.Thinking != "reasoning" {
		t.Errorf("Thinking = %q, want %q", got.Thinking, "reasoning")
	}
	if got.Held != "" {
		t.Errorf("Held = %q, want empty", got.Held)
	}
}

func TestSplitThinking_MultipleBlocks(t *testing.T) {
	got := splitThinking("a<think>first</think>b<think>second</think>c")
	if got.Kept != "abc" {
		t.Errorf("Kept = %q, want %q", got.Kept, "abc")
	}
	if got.Thinking != "first\nsecond" {
		t.Errorf("Thinking = %q, want %q", got.Thinking, "first\nsecond")
	}
	if got.Held != "" {
		t.Errorf("Held = %q, want empty", got.Held)
	}
}

func TestSplitThinking_TrailingPartialOpen(t *testing.T) {
	got := splitThinking("hello <think>half-baked")
	if got.Kept != "hello " {
		t.Errorf("Kept = %q, want %q", got.Kept, "hello ")
	}
	if got.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", got.Thinking)
	}
	if got.Held != "<think>half-baked" {
		t.Errorf("Held = %q, want %q", got.Held, "<think>half-baked")
	}
}

// TestSplitThinking_HeldCompletesAcrossCalls simulates the
// streaming protocol that keeps a split-boundary block whole: the
// next call prepends the Held partial before rescanning.
func TestSplitThinking_HeldCompletesAcrossCalls(t *testing.T) {
	first := splitThinking("hello <think>the user wants")
	if first.Held == "" {
		t.Fatalf("first call did not hold a partial; Held = %q", first.Held)
	}
	if first.Thinking != "" {
		t.Errorf("first call Thinking = %q, want empty", first.Thinking)
	}

	second := splitThinking(first.Held + " me to switch</think> world")
	if second.Thinking != "the user wants me to switch" {
		t.Errorf("second call Thinking = %q, want %q", second.Thinking, "the user wants me to switch")
	}
	if second.Kept != " world" {
		t.Errorf("second call Kept = %q, want %q", second.Kept, " world")
	}
	if second.Held != "" {
		t.Errorf("second call Held = %q, want empty", second.Held)
	}
}

// TestSplitThinking_StrayCloseTagKept guards against an overzealous
// splitter: a bare </think> with no preceding <think> in the input
// must be preserved as ordinary text. opencode 1.18 does not emit
// stray close tags today, but the bridge must not eat user content
// if some future variant does.
func TestSplitThinking_StrayCloseTagKept(t *testing.T) {
	got := splitThinking("user typed </think> here")
	if got.Kept != "user typed </think> here" {
		t.Errorf("Kept = %q, want %q", got.Kept, "user typed </think> here")
	}
	if got.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", got.Thinking)
	}
}

// TestSplitThinking_NestedTagsTreatedAsText verifies the splitter
// takes the FIRST </think> as the boundary — nested <think>
// inside an outer <think> is just text (the inline convention is
// flat; recursive scans produce ambiguous Held across deltas).
//
// Behaviour: the outer block's inner is `outer<think>inner`
// (everything until the first </think>). The trailing
// `still outer` is ordinary Kept text — the second close tag
// is preserved because the splitter has already exited the
// first block by then. This matches the pi bridge exactly;
// both bridges are forgiving on stray close tags so they never
// eat user content if some model variant emits them.
func TestSplitThinking_NestedTagsTreatedAsText(t *testing.T) {
	got := splitThinking("a<think>outer<think>inner</think>still outer</think> b")
	if got.Kept != "astill outer</think> b" {
		t.Errorf("Kept = %q, want %q", got.Kept, "astill outer</think> b")
	}
	if got.Thinking != "outer<think>inner" {
		t.Errorf("Thinking = %q, want %q", got.Thinking, "outer<think>inner")
	}
	if got.Held != "" {
		t.Errorf("Held = %q, want empty", got.Held)
	}
}

// TestSplitThinking_HeldAcrossCallsCarriesOpenTagReattached checks
// the Held format is exactly <think><rest> with the open tag
// preserved so the next call's first pass re-enters the open branch.
// (This is what makes the Held protocol semantic — the Held value
// must be passed verbatim to splitThinking again.)
func TestSplitThinking_HeldAcrossCallsCarriesOpenTagReattached(t *testing.T) {
	got := splitThinking("<think>half")
	want := "<think>half"
	if got.Held != want {
		t.Errorf("Held = %q, want %q (open tag must be re-attached)", got.Held, want)
	}
	// And the same Held string, fed back in, produces an empty
	// Kept and the same partial Thinking semantics.
	round := splitThinking(got.Held)
	if round.Kept != "" {
		t.Errorf("round-trip Kept = %q, want empty", round.Kept)
	}
	if !strings.HasPrefix(round.Held, thinkOpenTag) {
		t.Errorf("round-trip Held = %q, want prefix %q", round.Held, thinkOpenTag)
	}
}
