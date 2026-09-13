// health.go — periodic health probe for the shared dsh host.
//
// The main watchdog (lifecycle.go::runWatchdog) only observes
// cmd.Wait — it catches the case where the dsh subprocess crashes,
// but NOT the case where the subprocess is alive yet the HTTP /
// WS server is wedged (event loop stuck, plugin init deadlock,
// cordis frozen). The HealthProbe fills that gap: every interval
// it issues a typed RPC against the shared dsh host (verified
// 2026-09-13 to be the only reliable liveness check — dsh does
// NOT expose a /health route), and after strikesMax consecutive
// failures it force-kills the dsh subprocess so the main
// watchdog's cmd.Wait loop takes over and respawns.
//
// The probe is intentionally simple:
//   - session.list via the cookie-jar'd RPCClient, not a raw
//     GET /health. dsh 0.1.5-rc.1 does not register a /health
//     route — the only HTTP surface is the upgrade /api/remote.mux
//     plus the typed /api/* RPCs. session.list is the cheapest
//     auth-gated call (~2ms when healthy) and is independent of
//     session/agent state, so a healthy dsh with no sessions
//     still answers {result:{ok:true, value:{items:[]}}}. The
//     probe treats any non-OK result (HTTP error, transport
//     error, or result.ok=false) as a strike.
//   - 30s interval, 3s per-probe timeout, 3 strikes → 90s window
//     from "first sign of trouble" to "force-respawn". Tolerates
//     brief load spikes without false-positive respawns.
//   - The probe runs in its own goroutine and reuses the shared
//     RPCClient (which already carries the dsh-auth cookie) so
//     there's no extra HTTP plumbing or auth negotiation.
//
// Concurrency: the probe reads h.Client() each tick (so it follows
// respawns to the new dsh URL and the new RPC transport) and
// calls h.onFailure which lives in the watchdog's goroutine. The
// onFailure callback (forceKill) is safe to call from any
// goroutine — it acquires h.mu to swap / signal the cmd.
package host

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Health probe tuning. Constants rather than options because the
// trade-offs are fixed: 30s × 3 strikes = 90s detection window is
// a deliberate budget that balances "catch hung dsh" against
// "don't false-positive on brief load spikes".
const (
	healthProbeInterval   = 30 * time.Second
	healthProbeTimeout    = 3 * time.Second
	healthProbeStrikesMax = 3
)

// HealthProbe periodically issues session.list via the shared
// RPCClient on the dsh host. After strikesMax consecutive
// failures it invokes onFailure (typically: force-kill the dsh
// subprocess so the main watchdog loop respawns).
//
// Construct via NewHealthProbe, call Start to launch the probe
// goroutine, and Stop (or wait on Done) at shutdown.
//
// The probe reads the live *Client each tick so it follows
// respawns automatically — no reconfiguration needed when a new
// dsh comes up at a different URL.
type HealthProbe struct {
	// clientGetter returns the current *Client (may change across
	// respawns). We dereference each tick to follow the latest
	// transport.
	clientGetter func() *Client

	onFailure func() // invoked once when strikes reach strikesMax

	logger *slog.Logger

	// Tunable via struct fields, default values from NewHealthProbe.
	interval   time.Duration
	timeout    time.Duration
	strikesMax int

	mu      sync.Mutex
	strikes int
	closed  chan struct{}
	done    chan struct{}
}

// NewHealthProbe constructs a probe with default tuning. The
// caller supplies:
//   - clientGetter: returns the live *Client (URL + transport
//     change on respawn)
//   - onFailure: invoked once per strike-out cycle (probe then resets
//     strikes back to 0 so the next probe cycle starts fresh)
//
// onFailure runs in the probe goroutine; keep it short.
func NewHealthProbe(clientGetter func() *Client, onFailure func(), logger *slog.Logger) *HealthProbe {
	if logger == nil {
		logger = slog.Default()
	}
	return &HealthProbe{
		clientGetter: clientGetter,
		onFailure:    onFailure,
		logger:       logger,
		interval:     healthProbeInterval,
		timeout:      healthProbeTimeout,
		strikesMax:   healthProbeStrikesMax,
		closed:       make(chan struct{}),
		done:         make(chan struct{}),
	}
}

// Done returns a channel that closes when Stop completes (or has
// already completed). The watchdog waits on this to ensure the
// probe goroutine fully exits before tearing down the cmd.
func (h *HealthProbe) Done() <-chan struct{} { return h.done }

// Start launches the probe goroutine. Idempotent: calling twice
// is safe (the second call is a no-op because the underlying
// goroutine exits via the closed chan).
func (h *HealthProbe) Start() {
	go h.run()
}

// Stop signals the probe goroutine to exit and waits for it to
// drain. Idempotent (subsequent calls are no-ops).
func (h *HealthProbe) Stop() {
	h.mu.Lock()
	select {
	case <-h.closed:
		// already closed
		h.mu.Unlock()
		<-h.done
		return
	default:
		close(h.closed)
	}
	h.mu.Unlock()
	<-h.done
}

// Strikes returns the current failure-strike count. Used by tests
// to verify the probe accumulates failures correctly. Always under
// the mu lock.
func (h *HealthProbe) Strikes() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.strikes
}

// run is the probe goroutine. One tick per `interval`; on each tick
// we issue GET /health and tally a strike on failure. After
// strikesMax consecutive strikes we call onFailure once and reset
// the counter so a healthy-after-respawn dsh doesn't immediately
// re-trigger.
func (h *HealthProbe) run() {
	defer close(h.done)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.closed:
			return
		case <-ticker.C:
			h.tick()
		}
	}
}

// tick performs one health probe. Exposed as a method so tests can
// invoke it synchronously without waiting for the ticker.
//
// The probe issues a session.list typed RPC through the shared
// RPCClient (which already carries the dsh-auth cookie and
// follows respawns). Anything other than a clean
// {result:{ok:true, value:{items:...}}} counts as a strike —
// transport errors, HTTP 4xx/5xx (including the 401 we'd see if
// the cookie wasn't yet minted), and dsh-side result.ok=false
// all funnel through the same recordFailure path.
func (h *HealthProbe) tick() {
	cli := h.clientGetter()
	if cli == nil {
		h.recordFailure(fmt.Errorf("dsh.host: health probe: client is nil"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	if _, err := cli.RPC.SessionList(ctx); err != nil {
		h.recordFailure(fmt.Errorf("dsh.host: health probe: %w", err))
		return
	}
	// success — reset strike count
	h.mu.Lock()
	if h.strikes > 0 {
		h.logger.Info("dsh.host: health probe recovered",
			"prior_strikes", h.strikes)
		h.strikes = 0
	}
	h.mu.Unlock()
}

// recordFailure increments the strike counter and triggers
// onFailure once when we hit strikesMax. The counter is reset to 0
// after the callback so a respawn's healthy probe cycle starts
// fresh (no immediate re-trigger).
func (h *HealthProbe) recordFailure(err error) {
	h.mu.Lock()
	h.strikes++
	strikes := h.strikes
	trigger := strikes >= h.strikesMax
	h.mu.Unlock()
	h.logger.Warn("dsh.host: health probe failed",
		"strike", strikes,
		"err", err.Error())
	if trigger && h.onFailure != nil {
		h.logger.Error("dsh.host: health probe exhausted strikes; triggering force-respawn",
			"strikes", strikes)
		h.onFailure()
		// Reset so a successful probe after respawn doesn't
		// immediately re-trigger if the new dsh has its own
		// brief hang (rare, but possible). The next 3-failure
		// window has to accumulate fresh.
		h.mu.Lock()
		h.strikes = 0
		h.mu.Unlock()
	}
}
