// Package chatsession — AgentReaper (idle agent CLI reaper).
//
// Every agent session owns a live CLI child process. When the user
// switches agents or directories, the AgentSession is parked in the
// daemon-wide warm pool and its CLI keeps running — a long-lived
// daemon accumulates idle CLIs that hold memory and CPU.
//
// The reaper walks the daemon-wide AgentSessionPool once an hour and
// reclaims the CLIs that are no longer worth keeping:
//
//   - the CLI process is gone (the readpump never noticed the death)
//   - the CLI is alive but has sent no bridge event for over an hour
//
// Both cases end the same way: the bridge is closed through
// AgentSession.Close (so the CLI's own process group goes down and
// its tool subprocesses do not become orphans) and the session is
// reset to exited. The session identity survives — the next user
// message resumes the CLI with the same resume id.
package chatsession

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/registry"
)

const (
	// reaperInterval is the cadence of the reaper's scan.
	reaperInterval = time.Hour

	// reaperIdleThreshold is how long a live CLI may stay silent
	// before it is reclaimed. Equal to reaperInterval, so an idle
	// CLI actually lives for 1h–2h.
	reaperIdleThreshold = time.Hour

	// reaperCloseBudget bounds one batch of closes. Each bridge's
	// own Close has a 2–5s upper bound; the batch runs them
	// concurrently, so 15s covers every bridge's bound.
	reaperCloseBudget = 15 * time.Second
)

// AgentReaper is the daemon-wide idle-CLI reaper. One instance per
// daemon; it scans the process-wide AgentSessionPool.
type AgentReaper struct {
	// pool is the daemon-wide AS registry — the reaper's scan
	// source. The warm (parked) sessions live here and nowhere
	// else, so a per-chat scan would miss exactly the sessions
	// worth reclaiming.
	pool *AgentSessionPool

	// logger receives the reaper's records. A reaper run is
	// silent towards the chats: no channel ever learns that its
	// CLI was reclaimed.
	logger *slog.Logger

	// isRunning indicates whether the ticker goroutine is alive.
	// Snapshot via Snapshot().Active.
	isRunning atomic.Bool

	// Snapshot state. atomic.Pointer for time.Time semantics.
	startedAt    atomic.Pointer[time.Time]
	scannedTotal atomic.Int64
	probesRun    atomic.Int64
	respawnsHit  atomic.Int64
	lastScanAt   atomic.Pointer[time.Time]

	// Lifecycle. started flips true on Start; stopCh stops the
	// ticker; doneCh is closed when the goroutine returns.
	stopCh chan struct{}
	doneCh chan struct{}

	// now is overridable for tests; production uses time.Now.
	now func() time.Time
}

// NewAgentReaper constructs a reaper over the daemon-wide AS pool.
// A nil logger falls back to slog.Default.
func NewAgentReaper(pool *AgentSessionPool, logger *slog.Logger) *AgentReaper {
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentReaper{
		pool:   pool,
		logger: logger,
		now:    time.Now,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// Start launches the ticker goroutine. Idempotent — second call
// is a no-op. Returns true if the goroutine was actually started.
func (r *AgentReaper) Start() bool {
	if !r.isRunning.CompareAndSwap(false, true) {
		return false
	}
	now := r.now()
	r.startedAt.Store(&now)
	go r.loop()
	return true
}

// Stop halts the ticker goroutine and blocks until it returns.
// Idempotent and safe to call before Start (no-op).
func (r *AgentReaper) Stop() {
	if !r.isRunning.Load() {
		return
	}
	select {
	case <-r.stopCh:
		// already closed
	default:
		close(r.stopCh)
	}
	<-r.doneCh
}

// loop is the ticker body. Mirrors F-41 reconnect's loopWith
// pattern: tick every reaperInterval until stopCh fires.
func (r *AgentReaper) loop() {
	defer close(r.doneCh)
	defer r.isRunning.Store(false)

	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.tick()
		}
	}
}

// reapCandidate is one AgentSession the reaper decided to reclaim.
type reapCandidate struct {
	as  *AgentSession
	pid int

	// exitCode is the code recorded after the close: 0 when the CLI
	// was already gone (it exited on its own), -4 when the reaper
	// terminated it.
	exitCode int

	// reason labels the branch that selected this candidate.
	reason string

	// idle is how long the CLI had been silent. Zero for the
	// already-gone branch.
	idle time.Duration
}

// tick runs one scan over the daemon-wide AS pool and reclaims
// whatever it finds. Every candidate is closed; the batch runs
// concurrently under a single time budget.
func (r *AgentReaper) tick() {
	now := r.now()
	r.lastScanAt.Store(&now)

	candidates := r.collect(now)
	if len(candidates) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, c := range candidates {
		wg.Add(1)
		go func(c reapCandidate) {
			defer wg.Done()
			r.reap(c, now)
		}(c)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(reaperCloseBudget):
		r.logger.Warn("chatsession: reaper close batch exceeded its budget",
			"budget", reaperCloseBudget,
			"candidates", len(candidates))
	}
}

// collect walks the pool and returns the sessions worth reclaiming.
//
// A session qualifies only when it is running a CLI this daemon
// owns (status running, a PID, a live handle). Two conditions
// select it: the CLI process is gone, or the CLI has been silent
// for longer than the idle threshold. The idle branch additionally
// requires that no turn is in flight — a silent CLI may simply be
// working on a long task.
func (r *AgentReaper) collect(now time.Time) []reapCandidate {
	ases := r.pool.List()
	seen := make(map[string]bool, len(ases))
	out := make([]reapCandidate, 0, len(ases))

	for _, as := range ases {
		if as == nil {
			continue
		}
		if as.ID != "" {
			if seen[as.ID] {
				continue
			}
			seen[as.ID] = true
		}

		r.scannedTotal.Add(1)

		if as.Status() != StatusRunning {
			continue
		}
		pid := as.PID()
		if pid <= 0 {
			continue
		}
		if as.Handle() == nil {
			continue
		}

		r.probesRun.Add(1)
		if !processAlive(pid) {
			out = append(out, reapCandidate{
				as:       as,
				pid:      pid,
				exitCode: 0,
				reason:   "process_gone",
			})
			continue
		}

		idle := now.Sub(as.LastEventAt())
		if idle <= reaperIdleThreshold {
			continue
		}
		if !as.IsReady() {
			// A turn is in flight — the CLI may be working on a
			// long task. Leave it alone; the next scan decides.
			continue
		}
		out = append(out, reapCandidate{
			as:       as,
			pid:      pid,
			exitCode: registry.ExitCodeTerminatedByNightme,
			reason:   "idle",
			idle:     idle,
		})
	}
	return out
}

// reap closes one candidate's CLI and resets the session.
//
// The candidate conditions are re-checked first: the session may
// have taken a turn or died between the scan and the close.
func (r *AgentReaper) reap(c reapCandidate, now time.Time) {
	as := c.as
	if as.Status() != StatusRunning || as.PID() != c.pid || as.Handle() == nil {
		return
	}
	if c.reason == "idle" {
		if !as.IsReady() {
			return
		}
		if now.Sub(as.LastEventAt()) <= reaperIdleThreshold {
			return
		}
	}

	// Close() stops the readpump before it touches the bridge, so
	// no KindLifecycle event is emitted and the chat layer's
	// respawn-on-death path stays out of the way. The bridge's own
	// Close then takes the CLI's process group down.
	if err := as.Close(); err != nil {
		r.logger.Warn("chatsession: reaper failed to close agent CLI",
			"as_id", as.ID,
			"agent", as.Agent,
			"cwd", as.Cwd,
			"pid", c.pid,
			"reason", c.reason,
			"err", err)
		if processAlive(c.pid) {
			r.logger.Error("chatsession: reaper left agent CLI running",
				"as_id", as.ID,
				"pid", c.pid)
		}
		return
	}

	// Close() records exit code 0; override it with the code that
	// says why this session ended.
	as.SetExited(c.exitCode)
	r.respawnsHit.Add(1)

	r.logger.Info("chatsession: reaper reclaimed agent CLI",
		"as_id", as.ID,
		"agent", as.Agent,
		"cwd", as.Cwd,
		"pid", c.pid,
		"reason", c.reason,
		"idle", c.idle,
		"exit_code", c.exitCode)
}

// AgentReaperSnapshot is the read-only view of the reaper state.
// Mirrors F-41's ProberSnapshot shape so `nightme doctor` can
// render both with a consistent template.
type AgentReaperSnapshot struct {
	Active       bool
	StartedAt    time.Time
	Interval     time.Duration
	ScannedTotal int64
	ProbesRun    int64
	// RespawnsHit counts the AgentSessions this reaper closed and
	// reset — the sessions whose CLI it reclaimed.
	RespawnsHit int64
	LastScanAt  time.Time
}

// Snapshot returns the current reaper state. Safe to call from
// any goroutine; uses atomic loads.
func (r *AgentReaper) Snapshot() AgentReaperSnapshot {
	snap := AgentReaperSnapshot{
		Active:       r.isRunning.Load(),
		Interval:     reaperInterval,
		ScannedTotal: r.scannedTotal.Load(),
		ProbesRun:    r.probesRun.Load(),
		RespawnsHit:  r.respawnsHit.Load(),
	}
	if sa := r.startedAt.Load(); sa != nil {
		snap.StartedAt = *sa
	}
	if la := r.lastScanAt.Load(); la != nil {
		snap.LastScanAt = *la
	}
	return snap
}

// processAlive returns true iff the PID exists in the current
// process table. Uses signal 0 (test for existence, no actual
// signal sent) per kill(2). Returns false on ESRCH (process gone),
// true on EPERM (exists but no permission — same as alive for our
// purposes).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// kill(pid, 0) without sending a signal. Errors are converted
	// to (alive, exists) by inspecting the syscall errno:
	//   nil            → process exists and we own it
	//   EPERM          → process exists, owned by another user
	//   ESRCH          → process does not exist
	// We treat both nil and EPERM as "alive".
	err := kill0(pid)
	return err == nil || err == errEPERM
}

// kill0 is a thin wrapper around the platform kill syscall,
// extracted so the test can stub it.
var kill0 = platformKill0

// errEPERM is the sentinel for "exists but not ours" returned by
// the platform kill wrapper.
var errEPERM = errEPERMValue
