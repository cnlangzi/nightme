// fix-git-lock-file 2026-08-29: stampGitStatus must NOT call
// gitStatusLookup on a fixed set of OutboundKinds where either
// (a) a Bash tool is mid-flight holding .git/index.lock
// (OutToolStart / OutToolEnd) or (b) the event is reasoning
// metadata the user doesn't see (OutThinking). Stamping on
// every other user-visible kind still happens, so the footer
// stays fresh at every state boundary.
package outbound

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
)

// stampRecorder wraps a fakeChannel and counts how many times
// the Emitter's gitStatusLookup closure is invoked, plus a
// thread-safe getter for assertions.
type stampRecorder struct {
	lookups atomic.Int64
}

func (r *stampRecorder) lookup(_ context.Context, _ string) *messages.GitStatus {
	r.lookups.Add(1)
	return &messages.GitStatus{Workspace: "/repo"}
}

func (r *stampRecorder) calls() int64 { return r.lookups.Load() }

// newStampEmitter wires a fresh stampRecorder into a fresh
// Emitter. fakeChannel is reused from emitter_test.go.
func newStampEmitter() (*fakeChannel, *stampRecorder, messages.Emitter) {
	fc := &fakeChannel{name: "test"}
	r := &stampRecorder{}
	em := New(fc, Options{GitStatusLookup: r.lookup})
	return fc, r, em
}

func TestStampGitStatus_Skips_OutToolStart(t *testing.T) {
	fc, r, em := newStampEmitter()
	in := messages.OutboundMessage{
		ChatID: "c1", Kind: messages.OutToolStart,
		Tool: &messages.ToolInfo{Name: "Bash", Args: "ls"},
	}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if got := r.calls(); got != 0 {
		t.Fatalf("lookups = %d, want 0 (OutToolStart should skip)", got)
	}
	if fc.lastSent.GitStatus != nil {
		t.Fatalf("fc.lastSent.GitStatus = %+v, want nil", fc.lastSent.GitStatus)
	}
}

func TestStampGitStatus_Skips_OutToolEnd(t *testing.T) {
	fc, r, em := newStampEmitter()
	in := messages.OutboundMessage{
		ChatID: "c1", Kind: messages.OutToolEnd,
		Tool: &messages.ToolInfo{Name: "Bash", Args: "ls"},
	}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if got := r.calls(); got != 0 {
		t.Fatalf("lookups = %d, want 0 (OutToolEnd should skip)", got)
	}
	if fc.lastSent.GitStatus != nil {
		t.Fatalf("fc.lastSent.GitStatus = %+v, want nil", fc.lastSent.GitStatus)
	}
}

func TestStampGitStatus_Skips_OutThinking(t *testing.T) {
	_, r, em := newStampEmitter()
	in := messages.OutboundMessage{
		ChatID: "c1", Kind: messages.OutThinking, Text: "hmm...",
	}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if got := r.calls(); got != 0 {
		t.Fatalf("lookups = %d, want 0 (OutThinking should skip)", got)
	}
}

func TestStampGitStatus_Keeps_OutReply(t *testing.T) {
	_, r, em := newStampEmitter()
	in := messages.OutboundMessage{
		ChatID: "c1", Kind: messages.OutReply, Text: "hi",
	}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if got := r.calls(); got != 1 {
		t.Fatalf("lookups = %d, want 1 (OutReply must stamp)", got)
	}
}

func TestStampGitStatus_Keeps_OutResult(t *testing.T) {
	_, r, em := newStampEmitter()
	in := messages.OutboundMessage{
		ChatID: "c1", Kind: messages.OutResult, Text: "final",
	}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if got := r.calls(); got != 1 {
		t.Fatalf("lookups = %d, want 1 (OutResult must stamp)", got)
	}
}

func TestStampGitStatus_Skips_OutHeartbeat(t *testing.T) {
	// OutHeartbeat is a per-turn progress tick (ThinkCount /
	// ToolCount / LastBeatAt). Long turns emit N heartbeats —
	// stamping git status on each would multiply git status
	// calls by N. Pure internal state; skip.
	_, r, em := newStampEmitter()
	in := messages.OutboundMessage{
		ChatID: "c1", Kind: messages.OutHeartbeat,
	}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if got := r.calls(); got != 0 {
		t.Fatalf("lookups = %d, want 0 (OutHeartbeat should skip git stamp)", got)
	}
}

func TestStampGitStatus_NilLookup_NoCall(t *testing.T) {
	fc := &fakeChannel{name: "test"}
	em := New(fc, Options{}) // No GitStatusLookup — wiring absent.
	in := messages.OutboundMessage{
		ChatID: "c1", Kind: messages.OutReply, Text: "hi",
	}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if fc.lastSent.GitStatus != nil {
		t.Fatalf("fc.lastSent.GitStatus = %+v, want nil (no lookup wired)", fc.lastSent.GitStatus)
	}
}

func TestStampGitStatus_PreSet_NoCall(t *testing.T) {
	// Regression: pre-stamped messages must NOT be overwritten.
	// This guard predates the kind-based filter; ensure the
	// new filter doesn't accidentally re-trigger lookup when
	// msg.GitStatus is non-nil.
	fc := &fakeChannel{name: "test"}
	r := &stampRecorder{}
	em := New(fc, Options{GitStatusLookup: r.lookup})

	pre := &messages.GitStatus{Workspace: "/already-set"}
	in := messages.OutboundMessage{
		ChatID: "c1", Kind: messages.OutReply, Text: "hi",
		GitStatus: pre,
	}
	if err := em.Send(context.Background(), in); err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if got := r.calls(); got != 0 {
		t.Fatalf("lookups = %d, want 0 (pre-stamped must not be re-looked-up)", got)
	}
	if fc.lastSent.GitStatus != pre {
		t.Fatalf("fc.lastSent.GitStatus = %p, want pre-stamped %p", fc.lastSent.GitStatus, pre)
	}
}
