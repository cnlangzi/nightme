package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// newTestLogger returns a slog logger that drops all output. Used
// by draft streamer tests so logger.Warn / Info calls don't
// pollute test output.
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDraftStreamer_AppendsThinkingToStack verifies OutThinking
// events feed the thinkingStack (no wire call until flush). The
// streamer's streamDraftEvent must NOT call the API per event —
// events buffer silently and a flush sends the latest 5+5.
func TestDraftStreamer_AppendsThinkingToStack(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)

	for i := 1; i <= 3; i++ {
		handled, err := s.streamDraftEvent(context.Background(), fmt.Sprintf("💭 thought %d", i), messages.OutThinking)
		if err != nil || !handled {
			t.Fatalf("event %d: handled=%v err=%v", i, handled, err)
		}
	}
	if len(api.Calls) != 0 {
		t.Fatalf("expected 0 wire calls (events should buffer); got %d (calls=%+v)", len(api.Calls), api.Calls)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.thinkingStack) != 3 {
		t.Fatalf("thinkingStack = %d, want 3", len(s.thinkingStack))
	}
	for i, e := range s.thinkingStack {
		want := fmt.Sprintf("💭 thought %d", i+1)
		if e.body != want {
			t.Fatalf("thinkingStack[%d].body = %q, want %q", i, e.body, want)
		}
	}
}

// TestDraftStreamer_AppendsToolStartEnd verifies the slot model:
// OutToolStart opens a new slot; OutToolEnd attaches to the LAST
// slot's ends slice. Mirrors group_draft.go's slot logic.
func TestDraftStreamer_AppendsToolStartEnd(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)

	if _, err := s.streamDraftEvent(context.Background(), "● Read(/tmp/foo.go)", messages.OutToolStart); err != nil {
		t.Fatalf("ToolStart: %v", err)
	}
	if _, err := s.streamDraftEvent(context.Background(), "⎿  📄 Read → 47 lines", messages.OutToolEnd); err != nil {
		t.Fatalf("ToolEnd: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.toolsStack) != 1 {
		t.Fatalf("toolsStack len = %d, want 1", len(s.toolsStack))
	}
	slot := s.toolsStack[0]
	if slot.start.body != "● Read(/tmp/foo.go)" {
		t.Fatalf("slot.start.body = %q, want ToolStart body", slot.start.body)
	}
	if len(slot.ends) != 1 {
		t.Fatalf("slot.ends len = %d, want 1", len(slot.ends))
	}
	if slot.ends[0].body != "⎿  📄 Read → 47 lines" {
		t.Fatalf("slot.ends[0].body = %q, want ToolEnd body", slot.ends[0].body)
	}
}

// TestDraftStreamer_OrphanEndBecomesSlot verifies that an
// OutToolEnd with no open slot becomes the implicit Start of a new
// slot (body enters slot.start).
func TestDraftStreamer_OrphanEndBecomesSlot(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)

	if _, err := s.streamDraftEvent(context.Background(), "orphan end body", messages.OutToolEnd); err != nil {
		t.Fatalf("orphan ToolEnd: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.toolsStack) != 1 {
		t.Fatalf("toolsStack len = %d, want 1 (orphan End opens new slot)", len(s.toolsStack))
	}
	if s.toolsStack[0].start.body != "orphan end body" {
		t.Fatalf("orphan slot.start.body = %q, want %q", s.toolsStack[0].start.body, "orphan end body")
	}
	if len(s.toolsStack[0].ends) != 0 {
		t.Fatalf("orphan slot.ends should be empty; got %d", len(s.toolsStack[0].ends))
	}
}

// TestDraftStreamer_FIFOEvictAtCap verifies each stack evicts the
// oldest entry when exceeding cap 50.
func TestDraftStreamer_FIFOEvictAtCap(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)

	// 55 thinking events → thinkingStack should hold the latest 50.
	for i := 1; i <= 55; i++ {
		_, _ = s.streamDraftEvent(context.Background(), fmt.Sprintf("thought %d", i), messages.OutThinking)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.thinkingStack) != dmThinkingStackCap {
		t.Fatalf("thinkingStack = %d, want cap %d", len(s.thinkingStack), dmThinkingStackCap)
	}
	// Oldest should be thought 6 (events 1..5 evicted FIFO).
	if s.thinkingStack[0].body != "thought 6" {
		t.Fatalf("oldest thinking = %q, want %q", s.thinkingStack[0].body, "thought 6")
	}
	if s.thinkingStack[len(s.thinkingStack)-1].body != "thought 55" {
		t.Fatalf("newest thinking = %q, want %q", s.thinkingStack[len(s.thinkingStack)-1].body, "thought 55")
	}
}

// TestDraftStreamer_FlushPicksLatestFive verifies the flush
// window: latest 5 from each stack (or fewer if shorter). The
// remaining entries stay in the buffer.
func TestDraftStreamer_FlushPicksLatestFive(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)

	// 10 thinking + 10 tool starts → flush should send latest 5+5.
	for i := 1; i <= 10; i++ {
		_, _ = s.streamDraftEvent(context.Background(), fmt.Sprintf("💭 thought %d", i), messages.OutThinking)
		_, _ = s.streamDraftEvent(context.Background(), fmt.Sprintf("● Tool%d(args)", i), messages.OutToolStart)
	}

	if _, err := s.flushLocked(context.Background(), "test"); err != nil {
		t.Fatalf("flushLocked: %v", err)
	}
	if len(api.Calls) != 1 {
		t.Fatalf("flushLocked wire calls = %d, want 1", len(api.Calls))
	}
	call := api.Calls[0]
	if call.Method != "sendRichMessageDraft" {
		t.Fatalf("wire method = %q, want sendRichMessageDraft", call.Method)
	}

	blocks := richMessageBlocks(call.Params["rich_message"])
	// 5 thinking + 5 tool starts = 10 blocks.
	if len(blocks) != 10 {
		t.Fatalf("blocks = %d, want 10 (latest 5 thinking + latest 5 tools)", len(blocks))
	}
	// Thinking blocks first (events 6..10), then tool blocks (events 6..10).
	wantThinking := []string{
		"💭 thought 6", "💭 thought 7", "💭 thought 8", "💭 thought 9", "💭 thought 10",
	}
	for i, want := range wantThinking {
		if blocks[i] != want {
			t.Fatalf("block[%d] = %q, want thinking %q", i, blocks[i], want)
		}
	}
	wantTools := []string{
		"● Tool6(args)", "● Tool7(args)", "● Tool8(args)", "● Tool9(args)", "● Tool10(args)",
	}
	for i, want := range wantTools {
		if blocks[5+i] != want {
			t.Fatalf("block[%d] = %q, want tool %q", 5+i, blocks[5+i], want)
		}
	}

	// Buffer retains the older 5 + 5.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.thinkingStack) != 5 {
		t.Fatalf("thinkingStack after flush = %d, want 5 (10 - 5)", len(s.thinkingStack))
	}
	if len(s.toolsStack) != 5 {
		t.Fatalf("toolsStack after flush = %d, want 5", len(s.toolsStack))
	}
}

// TestDraftStreamer_FlushAllocatesDraftID verifies the first
// flush assigns a fresh draftID via the process-global counter.
func TestDraftStreamer_FlushAllocatesDraftID(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)
	_, _ = s.streamDraftEvent(context.Background(), "💭 first thought", messages.OutThinking)

	s.mu.Lock()
	if s.draftID != 0 {
		s.mu.Unlock()
		t.Fatalf("draftID before flush = %d, want 0 (unallocated)", s.draftID)
	}
	s.mu.Unlock()

	if _, err := s.flushLocked(context.Background(), "test"); err != nil {
		t.Fatalf("flushLocked: %v", err)
	}
	draftID, ok := api.Calls[0].Params["draft_id"].(int32)
	if !ok || draftID == 0 {
		t.Fatalf("draft_id = %v, want non-zero int32", api.Calls[0].Params["draft_id"])
	}

	s.mu.Lock()
	allocated := s.draftID == draftID
	s.mu.Unlock()
	if !allocated {
		t.Fatalf("streamer.draftID != allocated value %d", draftID)
	}

	// Second flush reuses the same draftID (server animates in
	// place by draft_id). Note: must NOT hold mu while calling
	// streamDraftEvent — the deferred unlock would deadlock.
	_, _ = s.streamDraftEvent(context.Background(), "💭 second thought", messages.OutThinking)
	if _, err := s.flushLocked(context.Background(), "test"); err != nil {
		t.Fatalf("flushLocked 2: %v", err)
	}
	if id := api.Calls[1].Params["draft_id"]; id != draftID {
		t.Fatalf("second flush draft_id = %v, want %d (reuse)", id, draftID)
	}
}

// TestDraftStreamer_FlushFailureRestoresBuffer verifies the
// issue #391 contract: a wire failure restores the popped entries
// to the buffer so the next flush retries on the same draftID
// without re-sending.
func TestDraftStreamer_FlushFailureRestoresBuffer(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)

	// Seed 10 thinking events.
	for i := 1; i <= 10; i++ {
		_, _ = s.streamDraftEvent(context.Background(), fmt.Sprintf("thought %d", i), messages.OutThinking)
	}

	// First flush: poison → fails. Buffer should be restored to 10.
	api.Errors = []error{errors.New("simulated 429")}
	if _, err := s.flushLocked(context.Background(), "test-fail"); err != nil {
		t.Fatalf("flushLocked: %v", err)
	}
	if len(api.Calls) != 1 {
		t.Fatalf("wire calls = %d, want 1 (the failing one)", len(api.Calls))
	}

	s.mu.Lock()
	restored := len(s.thinkingStack)
	s.mu.Unlock()
	if restored != 10 {
		t.Fatalf("thinkingStack after failed flush = %d, want 10 (restored)", restored)
	}

	// Clear poison; second flush should succeed and pop 5.
	api.Errors = nil
	if _, err := s.flushLocked(context.Background(), "test-recover"); err != nil {
		t.Fatalf("flushLocked recover: %v", err)
	}
	if len(api.Calls) != 2 {
		t.Fatalf("wire calls after recovery = %d, want 2", len(api.Calls))
	}
	s.mu.Lock()
	remaining := len(s.thinkingStack)
	s.mu.Unlock()
	if remaining != 5 {
		t.Fatalf("thinkingStack after recovery = %d, want 5 (10 - 5 popped)", remaining)
	}
}

// TestDraftStreamer_EndProcess_FlushesRemaining verifies the
// turn-end contract: endProcess flushes any non-empty buffer as
// the final surface, then resets draftID. No deleteMessage call
// (server handles draft disposal on OutResult landing).
func TestDraftStreamer_EndProcess_FlushesRemaining(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)

	// Buffer some events but don't fire the timer.
	for i := 1; i <= 3; i++ {
		_, _ = s.streamDraftEvent(context.Background(), fmt.Sprintf("thought %d", i), messages.OutThinking)
	}

	s.endProcess(context.Background())

	if len(api.Calls) != 1 {
		t.Fatalf("wire calls after endProcess = %d, want 1 (final flush)", len(api.Calls))
	}
	if api.Calls[0].Method != "sendRichMessageDraft" {
		t.Fatalf("method = %q, want sendRichMessageDraft", api.Calls[0].Method)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.thinkingStack) != 0 {
		t.Fatalf("thinkingStack after endProcess = %d, want 0 (drained)", len(s.thinkingStack))
	}
	if s.draftID != 0 {
		t.Fatalf("draftID after endProcess = %d, want 0 (reset for next turn)", s.draftID)
	}
	if del := findCallByMethod(api.Calls, "deleteMessage"); del != nil {
		t.Fatalf("endProcess must NOT call deleteMessage (server handles DM draft disposal); got %+v", del)
	}
}

// TestDraftStreamer_EndProcess_NoOpWhenEmpty verifies that
// endProcess on a streamer with no buffered events does not call
// the wire (no empty flush, no draftID allocation).
func TestDraftStreamer_EndProcess_NoOpWhenEmpty(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)

	s.endProcess(context.Background())

	if len(api.Calls) != 0 {
		t.Fatalf("wire calls on empty endProcess = %d, want 0", len(api.Calls))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draftID != 0 {
		t.Fatalf("draftID on empty endProcess = %d, want 0", s.draftID)
	}
}

// TestDraftStreamer_StopFlushTimer verifies endProcess stops the
// 10s debounce timer before the final flush, so a late timer
// firing doesn't double-flush.
func TestDraftStreamer_StopFlushTimer(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)

	_, _ = s.streamDraftEvent(context.Background(), "thought", messages.OutThinking)
	s.mu.Lock()
	timerActive := s.flushTimer != nil
	s.mu.Unlock()
	if !timerActive {
		t.Fatal("expected flushTimer active after streamDraftEvent")
	}

	s.endProcess(context.Background())

	// Wait briefly to give any (incorrectly-still-armed) timer a
	// chance to fire. With the timer stopped, no extra call should
	// appear.
	time.Sleep(50 * time.Millisecond)
	if len(api.Calls) != 1 {
		t.Fatalf("wire calls = %d, want 1 (timer should be stopped after endProcess)", len(api.Calls))
	}
}

// TestDraftStreamer_DebounceTimer_Fires verifies the 10s timer
// triggers a flush when no endProcess arrives. Production code
// arms the timer via startFlushTimerIfIdleLocked at 10s
// (dmDraftBatchInterval); the test swaps in a 20ms timer to keep
// runtime fast while exercising the same callback shape.
func TestDraftStreamer_DebounceTimer_Fires(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)

	_, _ = s.streamDraftEvent(context.Background(), "💭 ticker thought", messages.OutThinking)

	// streamDraftEvent already armed the production 10s timer via
	// startFlushTimerIfIdleLocked. Swap it out for a 20ms timer
	// so the test completes quickly without waiting 10s.
	s.mu.Lock()
	if s.flushTimer != nil {
		s.flushTimer.Stop()
		s.flushTimer = nil
	}
	s.flushTimer = time.AfterFunc(20*time.Millisecond, func() {
		s.mu.Lock()
		if len(s.thinkingStack) == 0 && len(s.toolsStack) == 0 {
			s.flushTimer = nil
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.flushLocked(ctx, "timer")
	})
	s.mu.Unlock()

	time.Sleep(100 * time.Millisecond)

	api.mu.Lock()
	calls := append([]fakeCall(nil), api.Calls...)
	api.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("timer-driven wire calls = %d, want 1", len(calls))
	}
	if calls[0].Method != "sendRichMessageDraft" {
		t.Fatalf("method = %q, want sendRichMessageDraft", calls[0].Method)
	}
}

// TestDraftStreamer_LockDiscipline_NetworkRoundtripReleasesMu
// verifies that during a wire call, a new streamDraftEvent can
// still append to the buffer (mu is released before flushMu is
// taken). This is the bug class that hung group_draft.go in
// 2026-09-15 21:00; the DM path must follow the same fix.
func TestDraftStreamer_LockDiscipline_NetworkRoundtripReleasesMu(t *testing.T) {
	api := &slowAPI{delay: 200 * time.Millisecond}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)

	// Seed one event and trigger a flush via a goroutine that
	// blocks in the wire call.
	_, _ = s.streamDraftEvent(context.Background(), "thought 1", messages.OutThinking)

	flushDone := make(chan struct{})
	go func() {
		_, _ = s.flushLocked(context.Background(), "test")
		close(flushDone)
	}()

	// Give the goroutine time to acquire flushMu (i.e. reach the
	// wire call). Then append a new event — this MUST succeed even
	// though the wire call is still in flight.
	time.Sleep(50 * time.Millisecond)

	handled, err := s.streamDraftEvent(context.Background(), "thought 2", messages.OutThinking)
	if !handled || err != nil {
		t.Fatalf("streamDraftEvent during wire call: handled=%v err=%v (mu should be released)", handled, err)
	}

	<-flushDone

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.thinkingStack) != 1 {
		t.Fatalf("thinkingStack = %d, want 1 (the second thought, popped from the flushed batch)", len(s.thinkingStack))
	}
}

// TestDraftStreamer_ForwardsMessageThreadID verifies forum topic
// routing: topicID > 0 carries message_thread_id on the wire.
func TestDraftStreamer_ForwardsMessageThreadID(t *testing.T) {
	api := &fakeAPI{}
	s := newDraftStreamer(api, newTestLogger(), 100, 0, 1)
	s.topicID = 42 // simulate forum topic
	_, _ = s.streamDraftEvent(context.Background(), "💭 topic-scoped thought", messages.OutThinking)

	if _, err := s.flushLocked(context.Background(), "test"); err != nil {
		t.Fatalf("flushLocked: %v", err)
	}
	if got := api.Calls[0].Params["message_thread_id"]; got != 42 {
		t.Fatalf("message_thread_id = %v, want 42", got)
	}
}

// slowAPI is a fakeAPI variant whose call blocks for `delay` so
// the lock-discipline test can race a network roundtrip against
// concurrent streamDraftEvent calls.
type slowAPI struct {
	mu    sync.Mutex
	Calls []fakeCall
	delay time.Duration
}

func (s *slowAPI) call(ctx context.Context, method string, params map[string]any, result any) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	s.Calls = append(s.Calls, fakeCall{Method: method, Params: params})
	s.mu.Unlock()
	return nil
}

func (s *slowAPI) download(_ context.Context, _ string) ([]byte, error) { return nil, nil }

// TestDraftIndex_GetOrCreate_ReturnsSameInstance verifies the
// per-(chat, thread, userMsgID) instance key.
func TestDraftIndex_GetOrCreate_ReturnsSameInstance(t *testing.T) {
	idx := newDraftIndex()
	api := &fakeAPI{}
	a := idx.getOrCreate(api, newTestLogger(), 100, 0, 1000)
	b := idx.getOrCreate(api, newTestLogger(), 100, 0, 1000)
	if a != b {
		t.Fatalf("expected same streamer for same (chat, thread, userMsgID)")
	}
	c := idx.getOrCreate(api, newTestLogger(), 100, 5, 1000)
	if a == c {
		t.Fatalf("expected different streamer for different thread_id")
	}
	d := idx.getOrCreate(api, newTestLogger(), 200, 0, 1000)
	if a == d {
		t.Fatalf("expected different streamer for different chat_id")
	}
	e := idx.getOrCreate(api, newTestLogger(), 100, 0, 1001)
	if a == e {
		t.Fatalf("expected different streamer for different userMsgID (per-turn isolation)")
	}
}

// TestDraftIndex_EndProcess_EvictsFromIndex verifies the per-turn
// eviction: endProcess removes the streamer from the index so the
// next turn allocates a fresh one (with draftID == 0).
func TestDraftIndex_EndProcess_EvictsFromIndex(t *testing.T) {
	idx := newDraftIndex()
	api := &fakeAPI{}
	first := idx.getOrCreate(api, newTestLogger(), 100, 0, 1000)
	_, _ = first.streamDraftEvent(context.Background(), "turn 1", messages.OutThinking)

	idx.endProcess(context.Background(), 100, 0, 1000)

	if _, ok := idx.streamers[draftIndexKey(100, 0, 1000)]; ok {
		t.Fatal("expected turn 1000 evicted from index")
	}
	second := idx.getOrCreate(api, newTestLogger(), 100, 0, 1000)
	if first == second {
		t.Fatal("expected fresh streamer after endProcess")
	}
	if second.draftID != 0 {
		t.Fatalf("fresh streamer draftID = %d, want 0", second.draftID)
	}
}

// TestDraftIndex_EndProcess_OnlyAffectsMatchingKey verifies that
// endProcess on one turn doesn't evict another turn's streamer.
func TestDraftIndex_EndProcess_OnlyAffectsMatchingKey(t *testing.T) {
	idx := newDraftIndex()
	api := &fakeAPI{}
	a := idx.getOrCreate(api, newTestLogger(), 100, 0, 1000)
	b := idx.getOrCreate(api, newTestLogger(), 100, 0, 1001)

	_, _ = a.streamDraftEvent(context.Background(), "x", messages.OutThinking)
	_, _ = b.streamDraftEvent(context.Background(), "y", messages.OutThinking)

	idx.endProcess(context.Background(), 100, 0, 1000)

	if _, ok := idx.streamers[draftIndexKey(100, 0, 1000)]; ok {
		t.Fatal("expected turn 1000 evicted")
	}
	if _, ok := idx.streamers[draftIndexKey(100, 0, 1001)]; !ok {
		t.Fatal("expected turn 1001 still present")
	}
}

// Compile-time guard: slowAPI must implement apiClient.
var _ apiClient = (*slowAPI)(nil)
