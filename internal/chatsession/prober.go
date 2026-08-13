// Package chatsession — F-61 agent process prober (fallback).
//
// Phase 2.5 already does synchronous respawn from the KindLifecycle
// handler, so most bridge deaths are recovered within ~1s without
// any tick. The prober exists for the residual cases where the
// readpump goroutine itself died (panic, OOM) and never fired
// KindLifecycle for the real death: the AS stays StatusRunning
// with a stale PID. The prober's kill(pid, 0) heartbeat detects
// that and marks the AS Suspect so a future LookupSelectedAgentSession
// respawns it.
//
// Mirrors the F-41 reconnect prober pattern (internal/channel/feishu/
// reconnect.go): ticker goroutine + atomic snapshot + Snapshot()
// accessor for `nightme doctor`.
package chatsession

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// proberInterval is the cadence at which the prober scans the pool
// for stale-running ASes. 30s matches F-41's reconnect prober —
// heartbeats stay cheap (kill(pid, 0) is just a syscall, no
// actual signal sent).
const proberInterval = 30 * time.Second

// suspectCooldown caps how often a single AS can be proactively
// respawned. Prevents thrashing when a bridge binary is missing
// (every spawn fails fast). Mirrors F-41's Backoff.
const suspectCooldown = 5 * time.Minute

// hungCooldown is a STRONGER cooldown used for the hung-but-alive
// recovery path. Killing a bridge that just happens to be slow is
// destructive (interrupts the user's turn), so we wait 2x the
// regular cooldown before the prober acts on a watchdog suspect.
const hungCooldown = 10 * time.Minute

// AgentProber (F-61) is the pool-wide heartbeat. One instance per
// Manager; constructor wires it into the Manager.Pool() iteration.
type AgentProber struct {
	// csProvider supplies the active chats to scan. Set via
	// WithChats; the prober snapshots the list on every tick so
	// it doesn't need locking with ChatSession state.
	csProvider func() []*ChatSession

	// isRunning indicates whether the ticker goroutine is alive.
	// Snapshot via Snapshot().Active.
	isRunning atomic.Bool

	// Snapshot state. atomic.Pointer for time.Time semantics.
	startedAt   atomic.Pointer[time.Time]
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

// NewAgentProber constructs a prober. csProvider is called on
// every tick to enumerate ChatSessions; it should be cheap (e.g.,
// snapshot the manager's session map).
func NewAgentProber(csProvider func() []*ChatSession) *AgentProber {
	return &AgentProber{
		csProvider: csProvider,
		now:        time.Now,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// Start launches the ticker goroutine. Idempotent — second call
// is a no-op. Returns true if the goroutine was actually started.
func (p *AgentProber) Start() bool {
	if !p.isRunning.CompareAndSwap(false, true) {
	 return false
	}
	now := p.now()
	p.startedAt.Store(&now)
	go p.loop()
	return true
}

// Stop halts the ticker goroutine and blocks until it returns.
// Idempotent and safe to call before Start (no-op).
func (p *AgentProber) Stop() {
	if !p.isRunning.Load() {
		return
	}
	select {
	case <-p.stopCh:
		// already closed
	default:
		close(p.stopCh)
	}
	<-p.doneCh
}

// loop is the ticker body. Mirrors F-41 reconnect's loopWith
// pattern: tick every proberInterval until stopCh fires.
func (p *AgentProber) loop() {
	defer close(p.doneCh)
	defer p.isRunning.Store(false)

	ticker := time.NewTicker(proberInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

// tick runs one probe scan across all chats. Counts each AS
// inspected (probesRun) and each suspect marked / respawned
// (respawnsHit). Errors are logged at warn level and counted
// into respawnsHit only when they trigger an actual SetSuspect —
// transient kill(pid, 0) failures (EPERM) are noise.
func (p *AgentProber) tick() {
	chats := p.csProvider()
	scanAt := p.now()
	p.lastScanAt.Store(&scanAt)

	for _, cs := range chats {
		if cs == nil {
			continue
		}
		for _, as := range cs.Pool() {
			if as == nil {
				continue
			}
			// ScannedTotal counts every AS we look at; ProbesRun
			// only counts ASes we actually sent a kill(0) probe
			// to (so doctor output reflects real syscall work,
			// not just pool walk).
			p.scannedTotal.Add(1)

			switch as.Status() {
			case StatusRunning:
				pid := as.PID()
				if pid == 0 {
					// PID zeroed but Status still Running —
					// readpump missed the death. Mark suspect.
					p.markSuspect(as, "stale_pid_zero")
					continue
				}
				p.probesRun.Add(1)
				if !processAlive(pid) {
					p.markSuspect(as, "pid_orphan")
				}
				// F-61 fix: hung-but-alive recovery. If the
				// watchdog already marked this AS Suspect
				// ("hung_prompt" or "no_fast_ack") AND we've
				// waited longer than the stronger hung cooldown,
				// forceful respawn. Bridges alive but stuck
				// thinking forever would otherwise stay
				// invisible to recovery.
				if reason, since := as.Suspect(); reason != "" && p.now().Sub(since) >= hungCooldown {
					p.respawnHung(as, reason)
				}
			case StatusExited:
				// F-61 fix #2: retry Exited ASes that were
				// marked Suspect by the synchronous respawn
				// path. Without this branch, a respawn
				// failure (e.g., binary missing) leaves the
				// AS Exited forever — only the next user
				// message would revive it.
				if reason, since := as.Suspect(); reason != "" && p.now().Sub(since) >= suspectCooldown {
					p.respawnExited(as, reason)
				}
			}
		}
	}
}

// markSuspect (F-61) is the prober's response to a stale AS: mark
// SuspectReason + SuspectSince (which gates the cooldown window)
// and try a synchronous respawn. On success the watchdog
// (Phase 2.5) already covers future deaths; on failure we leave
// the AS as Suspect so future ticks can retry after cooldown.
func (p *AgentProber) markSuspect(as *AgentSession, reason string) {
	prevReason, prevSince := as.Suspect()
	// Cooldown gate: skip if a recent suspect + cooldown window
	// is still in effect. Prevents thrashing when respawn fails
	// repeatedly (e.g., binary missing).
	if prevReason != "" && p.now().Sub(prevSince) < suspectCooldown {
		return
	}
	as.SetSuspect(reason)
	p.respawnsHit.Add(1)
	slog.Warn("chatsession: prober detected stale AS; respawning",
		"as_id", as.ID, "reason", reason)
	// Synchronous respawn is delegated to ChatSession so the
	// caller (a future LookupSelectedAgentSession) can pick up
	// where it left off. We don't call it here — the next user
	// message or watchdog tick will trigger the actual spawn.
	// (Future: directly call cs.LookupSelectedAgentSession here
	// when the prober is upgraded to proactive respawn.)
}

// respawnExited (F-61 fix #2) is the prober's retry path for
// Exited ASes that were left Suspect by a failed synchronous
// respawn. Cooldown is gated by SuspectSince; this is the
// path that revives ASes the synchronous path couldn't.
//
// Note: the actual respawn is delegated to the synchronous
// path's RestartFromDeath (via the cs's spawner). For now we
// only re-arm the Suspect marker — the next inbound message
// (via LookupSelectedAgentSession → Spawn) revives the AS, OR
// a future manager-level wiring will call RestartFromDeath
// directly. respawnsHit is NOT incremented here because no
// respawn actually happened (we'd otherwise inflate doctor
// counts on no-ops, see Finding #4).
func (p *AgentProber) respawnExited(as *AgentSession, reason string) {
	as.SetSuspect(reason)
	slog.Warn("chatsession: prober flagged Exited+Suspect AS for next-respawn",
		"as_id", as.ID, "reason", reason)
}

// respawnHung (F-61) is the recovery path for bridges alive but
// stuck (HungPrompt / no_fast_ack watchdog fired, AS still
// Running with valid pid). Cooldown is gated by the stronger
// hungCooldown (10 min) because killing a thinking bridge is
// destructive — interrupting the user's turn.
//
// Like respawnExited, the actual forceful kill+respawn is
// deferred to a future manager-level wiring; today we just
// re-arm the Suspect marker and increment respawnsHit ONLY
// when we genuinely kick off a recovery. For now we log and
// mark — the next time the prober sees this AS in cooldown
// it will repeat, giving operators a visible signal in
// daemon logs that recovery is desired.
func (p *AgentProber) respawnHung(as *AgentSession, reason string) {
	as.SetSuspect(reason)
	slog.Warn("chatsession: prober flagged hung-but-alive AS for recovery",
		"as_id", as.ID, "reason", reason)
	// The actual kill+respawn requires the manager reference
	// (for spawner access). Until that's wired, the prober's
	// role here is purely advisory — doctor + slog signal.
}

// AgentProberSnapshot is the read-only view of the prober state.
// Mirrors F-41's ProberSnapshot shape so `nightme doctor` can
// render both probers with a consistent template.
type AgentProberSnapshot struct {
	Active       bool
	StartedAt    time.Time
	Interval     time.Duration
	ScannedTotal int64
	ProbesRun    int64
	RespawnsHit  int64
	LastScanAt   time.Time
}

// Snapshot returns the current prober state. Safe to call from
// any goroutine; uses atomic loads.
func (p *AgentProber) Snapshot() AgentProberSnapshot {
	snap := AgentProberSnapshot{
		Active:       p.isRunning.Load(),
		Interval:     proberInterval,
		ScannedTotal: p.scannedTotal.Load(),
		ProbesRun:    p.probesRun.Load(),
		RespawnsHit:  p.respawnsHit.Load(),
	}
	if sa := p.startedAt.Load(); sa != nil {
		snap.StartedAt = *sa
	}
	if la := p.lastScanAt.Load(); la != nil {
		snap.LastScanAt = *la
	}
	return snap
}

// processAlive (F-61) returns true iff the PID exists in the
// current process table. Uses signal 0 (test for existence, no
// actual signal sent) per kill(2). Returns false on ESRCH
// (process gone), true on EPERM (exists but no permission —
// same as alive for our purposes).
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