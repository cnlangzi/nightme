package telegram

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// newTestLogger returns a slog logger that drops all output. Used
// by draft streamer tests so logger.Warn calls don't pollute test
// output.
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDraftStreamer_FirstAppend_AllocatesDraftID(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)
	if err := s.appendEvent(context.Background(), "hello"); err != nil {
		t.Fatalf("appendEvent: %v", err)
	}
	if got := len(api.Calls); got != 1 {
		t.Fatalf("call count = %d, want 1", got)
	}
	c := api.Calls[0]
	if c.Method != "sendMessageDraft" {
		t.Fatalf("method = %q, want sendMessageDraft", c.Method)
	}
	if c.Params["chat_id"] != int64(100) {
		t.Fatalf("chat_id = %v, want 100", c.Params["chat_id"])
	}
	draftID, ok := c.Params["draft_id"].(int32)
	if !ok || draftID == 0 {
		t.Fatalf("draft_id = %v, want non-zero int32", c.Params["draft_id"])
	}
	if c.Params["text"] != "hello" {
		t.Fatalf("text = %v", c.Params["text"])
	}
}

func TestDraftStreamer_ReusesDraftID(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)
	_ = s.appendEvent(context.Background(), "first")
	_ = s.appendEvent(context.Background(), "second")
	_ = s.appendEvent(context.Background(), "third")
	if got := len(api.Calls); got != 3 {
		t.Fatalf("call count = %d, want 3", got)
	}
	id0 := api.Calls[0].Params["draft_id"]
	for i, c := range api.Calls {
		if c.Params["draft_id"] != id0 {
			t.Fatalf("call %d draft_id=%v, want %v", i, c.Params["draft_id"], id0)
		}
	}
}

func TestDraftStreamer_ReplacesTextOnEachEvent(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)
	_ = s.appendEvent(context.Background(), "alpha")
	_ = s.appendEvent(context.Background(), "beta")
	_ = s.appendEvent(context.Background(), "gamma")
	// Each call REPLACES the draft body — the user sees only the
	// latest event's text, animated in place via same draft_id.
	got, _ := api.Calls[2].Params["text"].(string)
	if got != "gamma" {
		t.Fatalf("text = %q, want %q (REPLACE semantics: each event overwrites the prior)", got, "gamma")
	}
}

func TestDraftStreamer_FailedLatch_PreventsRetry(t *testing.T) {
	api := &fakeAPI{Errors: []error{errors.New("simulated 400")}}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)
	err1 := s.appendEvent(context.Background(), "first")
	if !errors.Is(err1, errDraftFallback) {
		t.Fatalf("first err = %v, want errDraftFallback", err1)
	}
	// Second append must NOT hit the API (latch).
	err2 := s.appendEvent(context.Background(), "second")
	if !errors.Is(err2, errDraftFallback) {
		t.Fatalf("second err = %v, want errDraftFallback", err2)
	}
	if got := len(api.Calls); got != 1 {
		t.Fatalf("call count = %d, want 1 (second append should have been latched)", got)
	}
}

func TestDraftStreamer_ResetState_ClearsLatch(t *testing.T) {
	// First API call fails → latch engaged.
	api := &fakeAPI{Errors: []error{errors.New("first fails")}}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)
	_ = s.appendEvent(context.Background(), "first")

	s.resetState()

	// After reset the latch is cleared; subsequent appends hit the
	// API again. The queue is empty so the second call succeeds.
	_ = s.appendEvent(context.Background(), "second")
	if got := len(api.Calls); got != 2 {
		t.Fatalf("call count = %d, want 2 (resetState should have cleared the latch)", got)
	}
}

func TestDraftStreamer_AppendEvent_EmptyText(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)
	if err := s.appendEvent(context.Background(), ""); err != nil {
		t.Fatalf("appendEvent empty: %v", err)
	}
	got, _ := api.Calls[0].Params["text"].(string)
	if got != "" {
		t.Fatalf("text = %q, want empty", got)
	}
}

func TestDraftStreamer_AppendEvent_NoParseMode(t *testing.T) {
	// parse_mode is intentionally never set — text is rendered as
	// plain text on the server side. Enabling parse_mode=HTML would
	// require escaping literal "&" and "<" in LLM output
	// ("AT&T", "type <T>") which silently mangles content.
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)
	_ = s.appendEvent(context.Background(), "plain ascii")
	_ = s.appendEvent(context.Background(), "<b>bold</b> tool call")
	_ = s.appendEvent(context.Background(), "AT&T thinking")
	for i, c := range api.Calls {
		if _, has := c.Params["parse_mode"]; has {
			t.Fatalf("call %d has parse_mode = %v (should never be set)", i, c.Params["parse_mode"])
		}
	}
}

func TestDraftIndex_GetOrCreate_ReturnsSameInstance(t *testing.T) {
	idx := newDraftIndex()
	api := &fakeAPI{}
	a := idx.getOrCreate(api, newTestLogger(), 100, 0)
	b := idx.getOrCreate(api, newTestLogger(), 100, 0)
	if a != b {
		t.Fatalf("expected same streamer for same (chat, thread)")
	}
	c := idx.getOrCreate(api, newTestLogger(), 100, 5)
	if a == c {
		t.Fatalf("expected different streamer for different thread_id")
	}
	d := idx.getOrCreate(api, newTestLogger(), 200, 0)
	if a == d {
		t.Fatalf("expected different streamer for different chat_id")
	}
}

func TestDraftIndex_Reset_OnlyAffectsMatchingKey(t *testing.T) {
	idx := newDraftIndex()
	api := &fakeAPI{}
	a := idx.getOrCreate(api, newTestLogger(), 100, 0)
	b := idx.getOrCreate(api, newTestLogger(), 100, 5)

	_ = a.appendEvent(context.Background(), "x")
	_ = b.appendEvent(context.Background(), "x")
	if got := len(api.Calls); got != 2 {
		t.Fatalf("setup: call count = %d, want 2", got)
	}

	// Reset only (100, 0) — b should NOT be touched.
	idx.reset(100, 0)

	// Subsequent append on b should still send (b's state intact).
	// And its draft_id should be the SAME as before the reset
	// (reset only clears state.ChatType=private's streamer).
	_ = b.appendEvent(context.Background(), "y")
	if got := len(api.Calls); got != 3 {
		t.Fatalf("call count = %d, want 3 (2 setup + 1 b append)", got)
	}
	idBefore := api.Calls[1].Params["draft_id"]
	idAfter := api.Calls[2].Params["draft_id"]
	if idBefore != idAfter {
		t.Fatalf("b's draft_id changed after reset of (100, 0): before=%v after=%v", idBefore, idAfter)
	}
}
