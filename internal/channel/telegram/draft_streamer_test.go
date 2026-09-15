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
	if err := s.appendEvent(context.Background(), "hello", true); err != nil {
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
	_ = s.appendEvent(context.Background(), "first", true)
	_ = s.appendEvent(context.Background(), "second", true)
	_ = s.appendEvent(context.Background(), "third", true)
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
	// REPLACE semantics: each appendEvent with replace=true wipes
	// the draft body and writes only the new text. The user sees
	// a single event at a time, animated in place via same draft_id.
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)
	_ = s.appendEvent(context.Background(), "alpha", true)
	_ = s.appendEvent(context.Background(), "beta", true)
	_ = s.appendEvent(context.Background(), "gamma", true)
	got, _ := api.Calls[2].Params["text"].(string)
	if got != "gamma" {
		t.Fatalf("text = %q, want %q (REPLACE: each event overwrites the prior)", got, "gamma")
	}
}

func TestDraftStreamer_FailedLatch_PreventsRetry(t *testing.T) {
	api := &fakeAPI{Errors: []error{errors.New("simulated 400")}}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)
	err1 := s.appendEvent(context.Background(), "first", true)
	if !errors.Is(err1, errDraftFallback) {
		t.Fatalf("first err = %v, want errDraftFallback", err1)
	}
	// Second append must NOT hit the API (latch).
	err2 := s.appendEvent(context.Background(), "second", true)
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
	_ = s.appendEvent(context.Background(), "first", true)

	s.resetState()

	// After reset the latch is cleared; subsequent appends hit the
	// API again. The queue is empty so the second call succeeds.
	_ = s.appendEvent(context.Background(), "second", true)
	if got := len(api.Calls); got != 2 {
		t.Fatalf("call count = %d, want 2 (resetState should have cleared the latch)", got)
	}
}

func TestDraftStreamer_AppendEvent_EmptyText(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)
	if err := s.appendEvent(context.Background(), "", true); err != nil {
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
	_ = s.appendEvent(context.Background(), "plain ascii", true)
	_ = s.appendEvent(context.Background(), "<b>bold</b> tool call", true)
	_ = s.appendEvent(context.Background(), "AT&T thinking", true)
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

	_ = a.appendEvent(context.Background(), "x", true)
	_ = b.appendEvent(context.Background(), "x", true)
	if got := len(api.Calls); got != 2 {
		t.Fatalf("setup: call count = %d, want 2", got)
	}

	// Reset only (100, 0) — b should NOT be touched.
	idx.reset(100, 0)

	// Subsequent append on b should still send (b's state intact).
	// And its draft_id should be the SAME as before the reset
	// (reset only clears state.ChatKind=private's streamer).
	_ = b.appendEvent(context.Background(), "y", true)
	if got := len(api.Calls); got != 3 {
		t.Fatalf("call count = %d, want 3 (2 setup + 1 b append)", got)
	}
	idBefore := api.Calls[1].Params["draft_id"]
	idAfter := api.Calls[2].Params["draft_id"]
	if idBefore != idAfter {
		t.Fatalf("b's draft_id changed after reset of (100, 0): before=%v after=%v", idBefore, idAfter)
	}
}

func TestDraftStreamer_ToolStartReplaceToolEndAccumulate(t *testing.T) {
	// User 2026-09-15 model: REPLACE on every event EXCEPT the
	// tool's End, which ACCUMULATEs onto its matching Start to
	// render "🔧 call / ✅ result" as one draft body. Within one
	// tool call: Start REPLACE → End ACCUMULATE. Across events:
	// REPLACE wipes prior body.
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)

	_ = s.appendEvent(context.Background(), "💭 considering whether to invoke Read", true)
	_ = s.appendEvent(context.Background(), "🔧 ● Read(/tmp/foo.go)", true)
	_ = s.appendEvent(context.Background(), "✅ Read → 47 lines", false) // accumulate onto start
	_ = s.appendEvent(context.Background(), "💭 now considering Bash", true)
	_ = s.appendEvent(context.Background(), "🔧 ● Bash(go build ./...)", true)
	_ = s.appendEvent(context.Background(), "✅ Bash done", false)

	if got := len(api.Calls); got != 6 {
		t.Fatalf("call count = %d, want 6", got)
	}

	// Each call's resulting text:
	// 1. "💭 considering..." (REPLACE — alone)
	// 2. "🔧 ● Read(/tmp/foo.go)" (REPLACE — alone)
	// 3. "🔧 ● Read(/tmp/foo.go)\n\n✅ Read → 47 lines" (ACCUMULATE)
	// 4. "💭 now considering Bash" (REPLACE — tool record gone)
	// 5. "🔧 ● Bash(go build ./...)" (REPLACE)
	// 6. "🔧 ● Bash(go build ./...)\n\n✅ Bash done" (ACCUMULATE)
	wantTexts := []string{
		"💭 considering whether to invoke Read",
		"🔧 ● Read(/tmp/foo.go)",
		"🔧 ● Read(/tmp/foo.go)\n\n✅ Read → 47 lines",
		"💭 now considering Bash",
		"🔧 ● Bash(go build ./...)",
		"🔧 ● Bash(go build ./...)\n\n✅ Bash done",
	}
	for i, want := range wantTexts {
		got, _ := api.Calls[i].Params["text"].(string)
		if got != want {
			t.Fatalf("call %d text = %q, want %q", i, got, want)
		}
	}

	// All calls share the same draft_id (animation in place).
	for i, c := range api.Calls {
		if c.Params["draft_id"] != api.Calls[0].Params["draft_id"] {
			t.Fatalf("call %d draft_id = %v, want %v", i, c.Params["draft_id"], api.Calls[0].Params["draft_id"])
		}
	}
}

func TestDraftStreamer_ResetProcess_ClearsDraftIDAndTextBuf_PreservesFailed(t *testing.T) {
	// resetProcess is the production path called from OutResult /
	// OnPromptEnded. It must clear draftID + textBuf so the next
	// process starts fresh, but preserve the failed latch (a Bot
	// API < 10.3 bot should keep dropping events until daemon
	// restart, not be silently healed by a successful process
	// end).
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)

	// Latch on a transient failure.
	api.Errors = []error{errors.New("simulated 400")}
	_ = s.appendEvent(context.Background(), "first", true)

	// Manually inspect: draftID != 0, textBuf != "", failed = true
	if s.draftID == 0 {
		t.Fatal("expected draftID allocated after first append")
	}
	if s.textBuf.Len() == 0 {
		t.Fatal("expected textBuf populated")
	}
	if !s.failed {
		t.Fatal("expected failed latch engaged")
	}
	draftIDBefore := s.draftID

	// End the process.
	s.resetProcess()

	if s.draftID != 0 {
		t.Fatalf("expected draftID reset to 0, got %d", s.draftID)
	}
	if s.textBuf.Len() != 0 {
		t.Fatalf("expected textBuf empty, got %q", s.textBuf.String())
	}
	if !s.failed {
		t.Fatal("expected failed latch PRESERVED across resetProcess (Bot API compatibility)")
	}

	// Failed latch PERSISTS across resetProcess by design (so a
	// Bot API < 10.3 bot keeps dropping events across turns until
	// daemon restart). Verify that:
	api.Errors = nil
	err := s.appendEvent(context.Background(), "next process", true)
	if !errors.Is(err, errDraftFallback) {
		t.Fatalf("appendEvent after resetProcess: got %v, want errDraftFallback (failed latch preserved)", err)
	}
	// The latch check returns BEFORE the draft_id allocation
	// path, so a latched streamer never bumps its draft_id. This
	// keeps the failure window contained (no new draft_id
	// consumption on persistent failures) and the failed latch
	// (failed=true) is what blocks future work until resetState.
	if s.draftID != 0 {
		t.Fatalf("expected draft_id to remain 0 after latched appendEvent, got %d", s.draftID)
	}
	if !s.failed {
		t.Fatal("expected failed latch to remain true")
	}
	_ = draftIDBefore // silence unused warning

	// Now resetState (NOT resetProcess) — this clears the latch too.
	s.resetState()
	if s.failed {
		t.Fatal("expected failed latch cleared by resetState (admin / test utility)")
	}

	// After full reset, next event allocates a fresh draft_id.
	if err := s.appendEvent(context.Background(), "next process", true); err != nil {
		t.Fatalf("appendEvent after resetState: %v", err)
	}
	if s.draftID == 0 {
		t.Fatal("expected new draft_id allocated after resetState")
	}
	if got := api.Calls[len(api.Calls)-1].Params["text"]; got != "next process" {
		t.Fatalf("next event text = %q, want \"next process\" (textBuf cleared)", got)
	}
}

func TestDraftStreamer_AppendEventWithThread_ForwardsMessageThreadID(t *testing.T) {
	// For forum topic routing, appendEventWithThread(..., topicID>0)
	// must include message_thread_id in the sendMessageDraft call.
	// For topicID==0 (DM), no message_thread_id is sent.
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0)

	// topicID=0 → no message_thread_id
	_ = s.appendEventWithThread(context.Background(), "first", true, 0)
	if _, has := api.Calls[0].Params["message_thread_id"]; has {
		t.Fatalf("topicID=0 should NOT include message_thread_id; got %v", api.Calls[0].Params)
	}

	// topicID=42 → message_thread_id=42
	_ = s.appendEventWithThread(context.Background(), "second", true, 42)
	if got := api.Calls[1].Params["message_thread_id"]; got != 42 {
		t.Fatalf("topicID=42 should set message_thread_id=42; got %v", got)
	}
}
