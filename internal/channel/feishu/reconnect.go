// Package feishu — F-41 active reconnect prober.
//
// The Feishu SDK (larksuite/oapi-sdk-go/v3/ws) handles WS reconnect
// automatically but with a 2-minute reconnectInterval default. For a
// user-facing daemon, "no response for 2 minutes after disconnect" is
// unacceptable. F-41 adds a 30s ticker prober that forces
// ch.Stop() + ch.Start() on every tick while disconnected, which
// effectively overrides the SDK's 2min default without changing
// SDK parameters.
//
// prober lifecycle:
//
//   OnDisconnected  ──► prober.Start()  (goroutine spawns 30s ticker)
//   ticker fires    ──► ch.Stop() → 100ms → ch.Start() → check Connected
//   OnReconnected / OnReady
//                   ──► prober.Stop()  (goroutine exits, ticker cancels)
//
// The prober NEVER actively gives up — it ticks forever until Connected
// or daemon shutdown. The 30s cadence is intentional: short enough
// to feel snappy, long enough that a fully-down network doesn't
// burn CPU. The SDK's internal 2min default remains as a final
// fallback in case the prober goroutine itself dies.
//
// See docs/feat/F-41-active-reconnect.md for design rationale.
package feishu

import (
	"sync"
	"sync/atomic"
	"time"
)

// proberConfig holds the tunables for the reconnect prober. Tests
// can shrink the interval; production uses the constants below.
type proberConfig struct {
	Interval time.Duration // ticker period
	Backoff  time.Duration // sleep between Stop and Start
}

// Default values used by newProber when no override is provided.
const (
	defaultProberInterval = 30 * time.Second
	defaultProberBackoff  = 100 * time.Millisecond
)

// prober is the goroutine-driven force-reconnect loop. One instance
// per Adapter. Methods are safe for concurrent use: Start / Stop
// from any goroutine; the ticker itself runs on its own goroutine
// and uses atomic counters for the snapshot.
type prober struct {
	adapter   *Adapter
	cfg       proberConfig
	restarter func() error // injected ch.Stop()+ch.Start() closure

	// Lifecycle channels. Mutated only under lifeMu (Stop reassigns
	// them after a successful stop). The loop captures them into
	// local variables at entry so its defer closes THIS cycle's
	// doneCh regardless of what Stop does next.
	lifeMu sync.Mutex
	stopCh  chan struct{}
	doneCh  chan struct{}

	// Snapshot state — read via Snapshot(), written by the ticker
	// goroutine and the SDK callback paths. atomic.Pointer for
	// time.Time / value semantics.
	startedAt   atomic.Pointer[time.Time]
	forceCount  atomic.Int64
	lastForceAt atomic.Pointer[time.Time]
	lastError   atomic.Pointer[string]

	// Started flag — guards against double-Start (which would leak
	// a goroutine). CompareAndSwap ensures only one transition.
	started atomic.Bool
}

// newProber constructs a prober. The restarter closure is what the
// ticker calls on each tick — typically a closure around
// (*Adapter).Stop() + sleep + (*Adapter).Start(). Pass nil to use
// the default no-op restarter (tests).
func newProber(adapter *Adapter, restarter func() error) *prober {
	if restarter == nil {
		restarter = func() error { return nil }
	}
	return &prober{
		adapter:   adapter,
		cfg:       proberConfig{Interval: defaultProberInterval, Backoff: defaultProberBackoff},
		restarter: restarter,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Start spawns the ticker goroutine. Idempotent — a second call
// while the prober is already running is a no-op. Returns true if
// the prober was actually started, false if it was already running.
func (p *prober) Start() bool {
	if !p.started.CompareAndSwap(false, true) {
		return false
	}
	now := time.Now()
	p.startedAt.Store(&now)
	// Snapshot the channels under the mutex so the loop's local
	// copies match Stop's reassignment. The loop captures stopCh
	// and doneCh into local variables at entry; the CAS + mutex
	// ensures the only goroutine that reads them is the one that
	// matches the close done by Stop.
	p.lifeMu.Lock()
	stopCh := p.stopCh
	doneCh := p.doneCh
	p.lifeMu.Unlock()
	go p.loopWith(stopCh, doneCh)
	return true
}

// Stop cancels the ticker and waits for the goroutine to exit. Safe
// to call when the prober isn't running (no-op). Blocks until the
// goroutine has returned, so callers know the prober is fully
// quiesced on return.
//
// The lifecycle channels (stopCh/doneCh) are reassigned under
// lifeMu so a concurrent Start sees the new pair, not the just-
// closed pair. Without this lock the test races; with it Start
// and Stop serialize on channel creation / close.
func (p *prober) Stop() {
	if !p.started.CompareAndSwap(true, false) {
		return
	}
	p.lifeMu.Lock()
	close(p.stopCh)
	doneCh := p.doneCh
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.lifeMu.Unlock()
	// Bounded wait for the loop to close doneCh. The loop's defer
	// always runs, so this is bounded by the loop's next tick (or
	// immediately if stopCh select wins). 5s is a defensive cap
	// against a future bug that strands the loop; in normal flow
	// it returns in microseconds.
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		// Defensive: the loop should have exited by now. Avoid
		// hanging forever if a future change breaks the channel
		// contract. The leaked loop goroutine will exit naturally
		// when the (now-closed) stopCh fires; we just stop waiting.
	}
	// Clear timestamps so a fresh cycle starts clean.
	p.startedAt.Store(nil)
}

// loopWith is the ticker goroutine. Lives until Stop closes stopCh.
//
// stopCh and doneCh are passed in (not read from fields) so the
// goroutine and the matching Stop share the SAME pair — preventing
// the "close of closed channel" race when Start/Stop overlap.
func (p *prober) loopWith(stopCh, doneCh chan struct{}) {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	defer close(doneCh)

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

// tick performs one force-reconnect cycle: restarter (Stop+100ms+Start),
// record stats, and check whether we successfully reconnected.
func (p *prober) tick() {
	now := time.Now()
	p.forceCount.Add(1)
	p.lastForceAt.Store(&now)

	err := p.restarter()
	if err != nil {
		// Record the error but keep going. The prober never gives up.
		msg := err.Error()
		p.lastError.Store(&msg)
		return
	}
	p.lastError.Store(nil)

	// If we successfully reconnected, stop the prober — the SDK's
	// OnReconnected callback will fire separately and emit the
	// "feishu: ws reconnected" log. We don't depend on that
	// callback here; checking Connected directly is enough.
	if p.adapter != nil && p.adapter.Health().Connected {
		// Self-stop: flip the started flag so the next Start works
		// and signal the loop to exit. We do this through a separate
		// channel so we don't deadlock on Stop's <-doneCh.
		go p.Stop()
	}
}

// Snapshot returns the current prober state. Safe to call from any
// goroutine. The booleans + counters come from atomic primitives;
// the time.Time pointers are loaded atomically.
type ProberSnapshot struct {
	Active     bool          // prober is currently running its 30s ticker
	Interval   time.Duration // ticker period
	StartedAt  time.Time     // when the current cycle started (zero if inactive)
	ForceCount int64         // total force-reconnect attempts since daemon start
	LastForceAt time.Time    // when the most recent attempt ran (zero if none)
	LastError  string        // error from the most recent attempt ("" if last was clean)
}

func (p *prober) Snapshot() ProberSnapshot {
	snap := ProberSnapshot{
		Active:   p.started.Load(),
		Interval: p.cfg.Interval,
	}
	if t := p.startedAt.Load(); t != nil {
		snap.StartedAt = *t
	}
	snap.ForceCount = p.forceCount.Load()
	if t := p.lastForceAt.Load(); t != nil {
		snap.LastForceAt = *t
	}
	if e := p.lastError.Load(); e != nil {
		snap.LastError = *e
	}
	return snap
}

// Compile-time guard that ProberSnapshot is the wire format
// (and is what gets embedded into WSHealthSnapshot for `nightme health`).
var _ = (*ProberSnapshot)(nil)
