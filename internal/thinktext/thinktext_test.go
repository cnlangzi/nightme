// Tests for the inline <think>…</think> handling.
//
// The splitter is shared: the Pi bridge streams through it and the
// personal agent unwraps with it. These tests lock the semantics both
// of them rely on.
package thinktext

import "testing"

func TestSplit_PlainTextUnchanged(t *testing.T) {
	got := Split("hello world")
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

func TestSplit_SingleBlock(t *testing.T) {
	got := Split("hello <think>reasoning</think> world")
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

func TestSplit_MultipleBlocks(t *testing.T) {
	got := Split("a<think>first</think>b<think>second</think>c")
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

func TestSplit_TrailingPartialOpen(t *testing.T) {
	got := Split("hello <think>half-baked")
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

// TestSplit_HeldCompletesAcrossCalls simulates the streaming protocol
// that keeps a split-boundary block whole: the next call prepends the
// Held partial before rescanning.
func TestSplit_HeldCompletesAcrossCalls(t *testing.T) {
	first := Split("hello <think>the user wants")
	if first.Held == "" {
		t.Fatalf("first call did not hold a partial; Held = %q", first.Held)
	}
	if first.Thinking != "" {
		t.Errorf("first call Thinking = %q, want empty", first.Thinking)
	}

	second := Split(first.Held + " me to switch</think> world")
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

// TestSplit_StrayCloseTagKept guards against an overzealous
// splitter: a bare </think> with no preceding <think> in the input
// must be preserved as ordinary text.
func TestSplit_StrayCloseTagKept(t *testing.T) {
	got := Split("user typed </think> here")
	if got.Kept != "user typed </think> here" {
		t.Errorf("Kept = %q, want %q", got.Kept, "user typed </think> here")
	}
	if got.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", got.Thinking)
	}
}

func TestUnwrap_KeepsTheContentAndDropsTheTags(t *testing.T) {
	got := Unwrap("<think>reasoning</think>")
	if got != "reasoning" {
		t.Errorf("Unwrap = %q, want %q", got, "reasoning")
	}
}

func TestUnwrap_PlainTextUnchanged(t *testing.T) {
	got := Unwrap("hello world")
	if got != "hello world" {
		t.Errorf("Unwrap = %q, want %q", got, "hello world")
	}
}

func TestUnwrap_KeepsTheTextAroundTheBlock(t *testing.T) {
	got := Unwrap("before <think>reasoning</think> after")
	if got != "before reasoning after" {
		t.Errorf("Unwrap = %q, want %q", got, "before reasoning after")
	}
}

func TestUnwrap_UnterminatedBlockLosesItsTag(t *testing.T) {
	got := Unwrap("hello <think>half-baked")
	if got != "hello half-baked" {
		t.Errorf("Unwrap = %q, want %q", got, "hello half-baked")
	}
}

func TestUnwrap_StrayCloseTagKept(t *testing.T) {
	got := Unwrap("user typed </think> here")
	if got != "user typed </think> here" {
		t.Errorf("Unwrap = %q, want %q", got, "user typed </think> here")
	}
}
