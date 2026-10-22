// channel_start.go — per-channel startup budget and isolation.
//
// A channel adapter's Start is the only part of daemon startup that
// talks to the outside world: telegram validates its bot token with a
// synchronous getMe, slack with auth.test, discord with a retried
// GetMe. Those round trips have their own timeouts — 45s, 45s and up
// to ~91s respectively — so an adapter on a host that cannot reach its
// API blocks the whole daemon for minutes. Because readiness used to
// be declared only after every channel had started, one unreachable
// host took the daemon down with it.
//
// Two properties fix that, and they are independent:
//
//   - readiness is declared before the loop (runDaemon), and
//   - each Start gets ChannelStartBudget of wall clock, after which
//     the adapter is abandoned.
//
// Abandoning means the adapter may still be running when we walk
// away: it is not required to honour its context. Two consequences
// are handled here. The context handed to Start is cancelled only on
// the abandon path — adapters derive their long-lived receive loops
// from it (telegram derives its poll loop from it), so cancelling it
// on success would kill a channel that just connected. And an adapter
// that finishes starting after we gave up on it is stopped, otherwise
// it would run forever with no pump draining its Incoming channel.
//
// Channels start concurrently, so a host that cannot reach one IM API
// costs ChannelStartBudget once rather than once per broken channel.

package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
)

// ChannelStartBudget is the wall clock a single Channel.Start may
// consume before the daemon gives up on it and carries on without it.
//
// Generous on purpose. Every adapter's own credential check is a
// network round trip to a third party (telegram 45s, slack 45s,
// discord up to ~91s with retries), and a link that is merely slow is
// indistinguishable from a bad token until you wait it out. Cutting a
// channel at 5s throws away ones that would have connected at 8s; at
// 10s the false-positive margin is worth the wait, because channels
// start concurrently and after readiness is declared — so this budget
// delays nothing but the startup report `nightme start` prints.
//
// Its ceiling is that report wait: `startupReportWait` in
// cmd/nightme has to outlast the whole phase.
const ChannelStartBudget = 10 * time.Second

// Status values reported through ChannelStatus.Status.
const (
	// ChannelStatusConnected means Start returned nil and the
	// channel's pump was attached.
	ChannelStatusConnected = "connected"

	// ChannelStatusSkipped means Start returned an error (or
	// panicked); the channel is left unattached and Error says why.
	ChannelStatusSkipped = "skipped"

	// ChannelStatusTimeout means Start outran ChannelStartBudget.
	ChannelStatusTimeout = "timeout"
)

// ChannelStatus is the startup outcome of one channel.
type ChannelStatus struct {
	Name   string
	Status string
	Error  string
}

// ChannelStartup is the outcome of the whole channel startup phase.
// Total counts the channels that were constructed from cfg; Results
// has one entry per constructed channel, in the order they were
// started. Channels absent from Results are the ones whose builder
// reported no credentials — not configured is not a failure, so they
// are neither counted nor reported.
type ChannelStartup struct {
	Total   int
	Results []ChannelStatus
}

// startChannels runs every channel's Start concurrently under
// ChannelStartBudget and returns one status per channel, indexed the
// way chs was given — never in completion order, so the startup
// report and `nightme start` output stay stable across runs.
//
// Only Start is concurrent. Everything the daemon does afterwards
// (buildStack, AttachPumps, health registration) stays in one pass
// over chs, which keeps allMgrs ordering, the health provider choice
// and the operator-facing output deterministic.
func startChannels(ctx context.Context, chs []channel.Channel) []ChannelStatus {
	statuses := make([]ChannelStatus, len(chs))
	var wg sync.WaitGroup
	for i, ch := range chs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = startChannel(ctx, ch)
		}()
	}
	wg.Wait()
	return statuses
}

// startChannel runs ch.Start under ChannelStartBudget and reports how
// it went. Anything other than ChannelStatusConnected carries the
// reason the channel could not be used.
//
// ctx is the daemon's own context, not a deadline-carrying child: see
// the file header for why the budget is enforced by the caller rather
// than by a context deadline.
func startChannel(ctx context.Context, ch channel.Channel) ChannelStatus {
	status := ChannelStatus{Name: ch.Name(), Status: ChannelStatusConnected}

	// The adapter keeps startCtx for the lifetime of its receive loop,
	// so cancelling it once Start returns would kill a channel that
	// just connected. Only the abandon path cancels; otherwise the
	// context lives on until the daemon's own context ends, which is
	// exactly the lifetime the adapter wants.
	startCtx, cancelStart := context.WithCancel(ctx)
	adapterKeptStartCtx := false
	defer func() {
		if !adapterKeptStartCtx {
			cancelStart()
		}
	}()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- ch.Start(startCtx)
	}()

	timer := time.NewTimer(ChannelStartBudget)
	defer timer.Stop()

	select {
	case err := <-done:
		adapterKeptStartCtx = err == nil
		if err != nil {
			status.Status = ChannelStatusSkipped
			status.Error = err.Error()
		}
	case <-timer.C:
		cancelStart()
		status.Status = ChannelStatusTimeout
		status.Error = fmt.Sprintf("start exceeded %s budget", ChannelStartBudget)
		stopAbandonedChannel(ch, done)
	}
	return status
}

// stopAbandonedChannel tears down an adapter that finished starting
// after we stopped waiting for it. Without this a slow adapter that
// eventually succeeds would keep a receive loop (and its outbound
// socket) alive with no pump attached.
//
// Stop runs only after Start has returned, never concurrently with
// it, so adapters that guard their own state see a consistent view.
func stopAbandonedChannel(ch channel.Channel, done <-chan error) {
	go func() {
		if err := <-done; err == nil {
			_ = ch.Stop(context.Background())
		}
	}()
}
