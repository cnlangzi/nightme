package session

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// textBlock is a tiny constructor for the most common test
// fixture — a single-text-block payload. Tests use it so they
// don't have to repeat the struct literal every time.
func textBlock(s string) []agent.ContentBlock {
	return []agent.ContentBlock{{Type: agent.ContentText, Text: s}}
}

// --- Idle / Busy state tests ---

func TestInputBuffer_IdleState_SendsImmediately(t *testing.T) {
	var flushed [][]agent.ContentBlock
	b := NewInputBuffer(func(blocks []agent.ContentBlock, _ []string) error {
		flushed = append(flushed, blocks)
		return nil
	}, 50, 1024)

	if err := b.Add(textBlock("hello"), "m1"); err != nil {
		t.Fatalf("Add returned err: %v", err)
	}
	if got := b.Pending(); got != 0 {
		t.Errorf("Pending = %d, want 0 (idle bypasses buffer)", got)
	}
	if len(flushed) != 1 {
		t.Fatalf("flushed = %d, want 1", len(flushed))
	}
	if got := flushed[0]; len(got) != 1 || got[0].Type != agent.ContentText || got[0].Text != "hello" {
		t.Errorf("flushed[0] = %+v, want [{ContentText, hello}]", got)
	}
}

func TestInputBuffer_BusyState_Buffers(t *testing.T) {
	var flushed atomic.Int32
	b := NewInputBuffer(func(_ []agent.ContentBlock, _ []string) error {
		flushed.Add(1)
		return nil
	}, 50, 1024)

	b.SetState(StateBusy)

	if err := b.Add(textBlock("first"), "m1"); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(textBlock("second"), "m2"); err != nil {
		t.Fatal(err)
	}
	if got := b.Pending(); got != 2 {
		t.Errorf("Pending = %d, want 2", got)
	}
	if got := flushed.Load(); got != 0 {
		t.Errorf("flushed = %d, want 0 (busy should buffer)", got)
	}
}

func TestInputBuffer_OnTurnEnded_FlushesCombined(t *testing.T) {
	var captured struct {
		mu       sync.Mutex
		combined []agent.ContentBlock
		ids      []string
		calls    int
	}
	b := NewInputBuffer(func(blocks []agent.ContentBlock, ids []string) error {
		captured.mu.Lock()
		defer captured.mu.Unlock()
		captured.combined = blocks
		captured.ids = ids
		captured.calls++
		return nil
	}, 50, 1024)

	b.SetState(StateBusy)
	b.Add(textBlock("foo"), "id_foo")
	b.Add(textBlock("bar"), "id_bar")
	b.Add(textBlock("baz"), "id_baz")

	if err := b.OnTurnEnded(); err != nil {
		t.Fatalf("OnTurnEnded: %v", err)
	}

	captured.mu.Lock()
	defer captured.mu.Unlock()
	// The blocks slice is the concatenation of all three entries
	// in order: foo, bar, baz.
	want := []agent.ContentBlock{
		{Type: agent.ContentText, Text: "foo"},
		{Type: agent.ContentText, Text: "bar"},
		{Type: agent.ContentText, Text: "baz"},
	}
	if len(captured.combined) != len(want) {
		t.Fatalf("combined length = %d, want %d", len(captured.combined), len(want))
	}
	for i, blk := range captured.combined {
		if blk.Type != want[i].Type || blk.Text != want[i].Text {
			t.Errorf("combined[%d] = %+v, want %+v", i, blk, want[i])
		}
	}
	if len(captured.ids) != 3 || captured.ids[0] != "id_foo" {
		t.Errorf("ids = %v", captured.ids)
	}
	if captured.calls != 1 {
		t.Errorf("calls = %d, want 1 (single flush)", captured.calls)
	}
	if got := b.Pending(); got != 0 {
		t.Errorf("Pending after flush = %d, want 0", got)
	}
}

func TestInputBuffer_OnTurnEnded_EmptyBuffer_NoOp(t *testing.T) {
	var calls atomic.Int32
	b := NewInputBuffer(func(_ []agent.ContentBlock, _ []string) error {
		calls.Add(1)
		return nil
	}, 50, 1024)

	b.SetState(StateBusy)
	if err := b.OnTurnEnded(); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("hook calls = %d, want 0 (empty buffer is no-op)", got)
	}
}

func TestInputBuffer_Flush_FailedHook_MessageLost(t *testing.T) {
	// Per F-25 spec: onFlush failure is NOT retried. User re-sends.
	// The buffer drains anyway (we cleared before invoking hook).
	b := NewInputBuffer(func(_ []agent.ContentBlock, _ []string) error {
		return errors.New("simulated flush failure")
	}, 50, 1024)

	b.SetState(StateBusy)
	b.Add(textBlock("hello"), "m1")

	err := b.OnTurnEnded()
	if err == nil {
		t.Fatal("OnTurnEnded should propagate hook error")
	}

	// Buffer drained even though hook failed.
	if got := b.Pending(); got != 0 {
		t.Errorf("Pending after failed flush = %d, want 0 (drained)", got)
	}
}

func TestInputBuffer_Flush_AfterIdleState_PassesThrough(t *testing.T) {
	// Add while idle bypasses buffer; flush still works (no-op).
	var calls atomic.Int32
	b := NewInputBuffer(func(_ []agent.ContentBlock, _ []string) error {
		calls.Add(1)
		return nil
	}, 50, 1024)

	b.Add(textBlock("direct"), "m1")
	if err := b.OnTurnEnded(); err != nil {
		t.Fatal(err)
	}
	// Add bypassed (called hook directly), OnTurnEnded had nothing
	// in buffer → no second call.
	if got := calls.Load(); got != 1 {
		t.Errorf("hook calls = %d, want 1", got)
	}
}

func TestInputBuffer_Clear_Discards(t *testing.T) {
	b := NewInputBuffer(nil, 50, 1024)

	b.SetState(StateBusy)
	b.Add(textBlock("a"), "m1")
	b.Add(textBlock("b"), "m2")
	b.Add(textBlock("c"), "m3")

	if n := b.Clear(); n != 3 {
		t.Errorf("Clear returned %d, want 3", n)
	}
	if got := b.Pending(); got != 0 {
		t.Errorf("Pending after Clear = %d, want 0", got)
	}

	// After clear, OnTurnEnded is no-op.
	if err := b.OnTurnEnded(); err != nil {
		t.Errorf("OnTurnEnded after Clear should be no-op, got %v", err)
	}
}

func TestInputBuffer_Clear_Empty_ReturnsZero(t *testing.T) {
	b := NewInputBuffer(nil, 50, 1024)
	if n := b.Clear(); n != 0 {
		t.Errorf("Clear on empty = %d, want 0", n)
	}
}

// --- Capacity limits ---

func TestInputBuffer_MaxMsgs(t *testing.T) {
	b := NewInputBuffer(nil, 2, 1024)
	b.SetState(StateBusy)

	if err := b.Add(textBlock("a"), "m1"); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(textBlock("b"), "m2"); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(textBlock("c"), "m3"); !errors.Is(err, ErrBufferFull) {
		t.Fatalf("expected ErrBufferFull, got %v", err)
	}
}

func TestInputBuffer_MaxBytes(t *testing.T) {
	b := NewInputBuffer(nil, 100, 10) // 10-byte cap
	b.SetState(StateBusy)

	if err := b.Add(textBlock("12345"), "m1"); err != nil { // 5 bytes
		t.Fatal(err)
	}
	// 5 + 6 = 11 > 10 → should fail
	if err := b.Add(textBlock("678901"), "m2"); !errors.Is(err, ErrBufferFull) {
		t.Errorf("expected ErrBufferFull on byte overflow, got %v", err)
	}
}

// --- Concurrency ---

func TestInputBuffer_ConcurrentAdd_NoRace(t *testing.T) {
	b := NewInputBuffer(nil, 1000, 1024*1024)
	b.SetState(StateBusy)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = b.Add(textBlock("msg"), "id")
		}(i)
	}
	wg.Wait()

	if got := b.Pending(); got != 100 {
		t.Errorf("Pending = %d, want 100", got)
	}
}

// --- Defaults ---

func TestInputBuffer_DefaultsForZeroLimits(t *testing.T) {
	b := NewInputBuffer(nil, 0, 0) // both zero → use defaults
	if b.maxMsgs != 50 {
		t.Errorf("maxMsgs default = %d, want 50", b.maxMsgs)
	}
	if b.maxBytes != 100*1024 {
		t.Errorf("maxBytes default = %d, want %d", b.maxBytes, 100*1024)
	}
}

func TestSessionState_String(t *testing.T) {
	if got := StateIdle.String(); got != "idle" {
		t.Errorf("StateIdle.String = %q", got)
	}
	if got := StateBusy.String(); got != "busy" {
		t.Errorf("StateBusy.String = %q", got)
	}
}

// --- Image / file block buffering ---

func TestInputBuffer_ImageBlockPreservedThroughFlush(t *testing.T) {
	// An image attachment must survive the busy→idle flush
	// without losing the path. Verifies the v0.2 attachments
	// contract.
	var captured []agent.ContentBlock
	b := NewInputBuffer(func(blocks []agent.ContentBlock, _ []string) error {
		captured = blocks
		return nil
	}, 50, 1024)

	b.SetState(StateBusy)
	b.Add([]agent.ContentBlock{
		{Type: agent.ContentText, Text: "看这张图"},
		{Type: agent.ContentImage, Path: "/tmp/a.png", MediaType: "image/png"},
	}, "id_img")
	if err := b.OnTurnEnded(); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 2 {
		t.Fatalf("captured length = %d, want 2", len(captured))
	}
	if captured[0].Type != agent.ContentText || captured[0].Text != "看这张图" {
		t.Errorf("captured[0] = %+v, want text \"看这张图\"", captured[0])
	}
	if captured[1].Type != agent.ContentImage || captured[1].Path != "/tmp/a.png" {
		t.Errorf("captured[1] = %+v, want image /tmp/a.png", captured[1])
	}
}