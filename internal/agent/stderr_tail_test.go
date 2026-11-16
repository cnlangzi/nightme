package agent

import (
	"errors"
	"strings"
	"testing"
)

func TestWithStderrTail(t *testing.T) {
	t.Run("NilErrorStaysNil", func(t *testing.T) {
		if got := WithStderrTail(nil, "boom"); got != nil {
			t.Fatalf("WithStderrTail(nil, ...) = %v, want nil", got)
		}
	})

	t.Run("EmptyTailLeavesTextUnchanged", func(t *testing.T) {
		base := errors.New("omp: get_state: session closed")
		got := WithStderrTail(base, "")
		if got == nil {
			t.Fatal("WithStderrTail(err, \"\") = nil, want the original error")
		}
		if got.Error() != base.Error() {
			t.Errorf("Error() = %q, want %q", got.Error(), base.Error())
		}
		if strings.Contains(got.Error(), "--- stderr tail ---") {
			t.Errorf("Error() = %q, want no tail section for an empty tail", got.Error())
		}
	})

	t.Run("AppendsTailAndKeepsWrapChain", func(t *testing.T) {
		base := errors.New("omp: get_state: session closed")
		got := WithStderrTail(base, "boom")
		want := base.Error() + "\n--- stderr tail ---\nboom"
		if got.Error() != want {
			t.Errorf("Error() = %q, want %q", got.Error(), want)
		}
		if !errors.Is(got, base) {
			t.Errorf("errors.Is(result, err) = false, want true")
		}
	})

	t.Run("TruncatesLongTail", func(t *testing.T) {
		base := errors.New("boom")
		got := WithStderrTail(base, strings.Repeat("x", 4096))
		if !strings.Contains(got.Error(), " ...[truncated]") {
			t.Errorf("Error() = %q, want the truncation marker", got.Error())
		}
		if len(got.Error()) > len(base.Error())+1024+len("\n--- stderr tail ---\n") {
			t.Errorf("Error() length = %d, want the tail capped at 1024 bytes", len(got.Error()))
		}
	})
}
