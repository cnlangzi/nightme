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
	// Factory closes `started` once the detector has been
	// built — the test waits on that instead of guessing a
	// sleep duration.
	started := make(chan struct{})
	var det syswake.Detector
	factory := func(_ context.Context, _ syswake.Config) (syswake.Detector, error) {
		det = syswake.NewFake()
		close(started)
		return det, nil
	}

	a := &fakeWakeKicker{name: "feishu"}
	b := &fakeWakeKicker{name: "fake-telegram"}
	kickers := []WakeKicker{a, b}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startWakeWatcherWithFactory(ctx, kickers, slog.New(slog.NewTextHandler(io.Discard, nil)), factory)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
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
	// startWakeWatcherWithFactory must return synchronously
	// before touching the factory when the kicker slice is
	// empty — so the assertion can run without any sleep.
	var factoryCalls atomic.Int32
	factory := func(_ context.Context, _ syswake.Config) (syswake.Detector, error) {
		factoryCalls.Add(1)
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startWakeWatcherWithFactory(ctx, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), factory)

	if got := factoryCalls.Load(); got != 0 {
		t.Errorf("factory called %d times when no kickers present, want 0", got)
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
	startWakeWatcherWithFactory(ctx, []WakeKicker{}, slog.New(slog.NewTextHandler(io.Discard, nil)), factory)
	if got := factoryCalls.Load(); got != 0 {
		t.Errorf("factory called %d times with empty slice, want 0", got)
	}
}
