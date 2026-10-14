package runtime

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/syswake"
)

// fakeWakeKicker records every WakeKick call so tests can
// assert fan-out count.
type fakeWakeKicker struct {
	name  string
	kicks atomic.Int32
}

func (f *fakeWakeKicker) WakeKick() { f.kicks.Add(1) }

func TestStartWakeWatcher_FakeDetectsAndForwards(t *testing.T) {
	var det syswake.Detector
	factory := func(_ context.Context, _ syswake.Config) (syswake.Detector, error) {
		det = syswake.NewFake()
		return det, nil
	}

	a := &fakeWakeKicker{name: "feishu"}
	b := &fakeWakeKicker{name: "fake-telegram"}
	chs := []any{a, b}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startWakeWatcherWithFactory(ctx, chs, slog.New(slog.NewTextHandler(io.Discard, nil)), factory)
	time.Sleep(50 * time.Millisecond) // let the goroutine subscribe
	if det == nil {
		t.Fatal("factory never called")
	}

	fd := det.(*syswake.FakeDetector)
	for i := 0; i < 3; i++ {
		fd.Inject()
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.kicks.Load() == 3 && b.kicks.Load() == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := a.kicks.Load(); got != 3 {
		t.Errorf("feishu kicker: got %d, want 3", got)
	}
	if got := b.kicks.Load(); got != 3 {
		t.Errorf("fake-telegram kicker: got %d, want 3", got)
	}
}

func TestStartWakeWatcher_NoKickersNoOp(t *testing.T) {
	// Channels that don't implement WakeKicker — the watcher
	// must silently no-op without spinning up a detector at all.
	var factoryCalls atomic.Int32
	factory := func(_ context.Context, _ syswake.Config) (syswake.Detector, error) {
		factoryCalls.Add(1)
		return nil, nil
	}

	chs := []any{&nonWakeKickerChannel{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startWakeWatcherWithFactory(ctx, chs, slog.New(slog.NewTextHandler(io.Discard, nil)), factory)

	time.Sleep(50 * time.Millisecond)
	if got := factoryCalls.Load(); got != 0 {
		t.Errorf("factory called %d times when no WakeKickers present, want 0", got)
	}
}

func TestStartWakeWatcher_EmptySliceNoOp(t *testing.T) {
	var factoryCalls atomic.Int32
	factory := func(_ context.Context, _ syswake.Config) (syswake.Detector, error) {
		factoryCalls.Add(1)
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startWakeWatcherWithFactory(ctx, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), factory)
	time.Sleep(20 * time.Millisecond)
	if got := factoryCalls.Load(); got != 0 {
		t.Errorf("factory called %d times with empty slice, want 0", got)
	}
}

// nonWakeKickerChannel intentionally does NOT implement
// WakeKicker — used to verify startWakeWatcher silently skips
// non-kickers without failing.
type nonWakeKickerChannel struct{}