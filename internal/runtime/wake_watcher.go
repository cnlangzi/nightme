// wake_watcher.go — OS wake-from-sleep fan-out.
//
// F-fix-os-sleep-wakeup: detect OS resume-from-sleep via
// internal/syswake and forward to every channel that opts in
// to the WakeKicker interface. Channels that don't implement
// it (telegram long-poll, slack socketmode auto-reconnect,
// discord's discordgo gateway) silently skip — adding the
// hook is a one-line change per channel and can land as a
// follow-up PR.

package runtime

import (
	"context"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/syswake"
)

// WakeKicker is the opt-in interface any channel.Channel can
// implement to receive an OS resume-from-sleep fan-out.
// Matched by type assertion in startWakeWatcher — adding it
// is a one-line change per channel; channels that don't need
// it silently skip. The WakeKick method must be non-blocking;
// the watcher goroutine calls it inline for every wake.
type WakeKicker interface {
	WakeKick()
}

// detectorFactory is the constructor injected by tests so they
// can supply a fake Detector without touching real OS APIs.
// Production code passes syswake.New.
type detectorFactory func(context.Context, syswake.Config) (syswake.Detector, error)

// startWakeWatcher launches a single syswake detector and
// wires its fan-out to every WakeKicker in startedChannels.
//
// Safe to call with an empty slice (no-op). Errors from
// syswake.New are debug-logged, not fatal — a daemon that
// can't detect wake is no worse than the pre-fix behavior.
//
// Lifetime: tied to ctx. SIGINT/SIGTERM cancels the outer
// ctx, the goroutine selects on ctx.Done() and exits cleanly,
// closing the detector in defer.
func startWakeWatcher(
	ctx context.Context,
	startedChannels []channel.Channel,
	logger *slog.Logger,
) {
	anySlice := make([]any, len(startedChannels))
	for i, ch := range startedChannels {
		anySlice[i] = ch
	}
	startWakeWatcherWithFactory(ctx, anySlice, logger, syswake.New)
}

// startWakeWatcherWithFactory is the test-seam form of
// startWakeWatcher. Production callers use startWakeWatcher;
// tests pass a factory that returns a *syswake.fakeDetector so
// they can deterministically fire wake events.
//
// The slice element type is `any` (not channel.Channel) so
// tests don't have to stub the full channel.Channel interface
// (8 methods) just to feed a WakeKicker. Production callers
// pass []channel.Channel which auto-satisfies `[]any`.
func startWakeWatcherWithFactory(
	ctx context.Context,
	startedChannels []any,
	logger *slog.Logger,
	newDetector detectorFactory,
) {
	if logger == nil {
		logger = slog.Default()
	}

	var kickers []WakeKicker
	for _, ch := range startedChannels {
		if ch == nil {
			continue
		}
		k, ok := ch.(WakeKicker)
		if !ok {
			continue
		}
		kickers = append(kickers, k)
	}
	if len(kickers) == 0 {
		logger.Debug("wake: no channels implement WakeKicker; wake watcher not started")
		return
	}

	det, err := newDetector(ctx, syswake.Config{
		Logger: logger.With("component", "syswake"),
	})
	if err != nil {
		logger.Debug("wake: detector factory failed; wake watcher not started", "err", err)
		return
	}

	go func() {
		defer func() { _ = det.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-det.WakeChan():
				if !ok {
					return
				}
				for _, k := range kickers {
					k.WakeKick()
				}
			}
		}
	}()
}