// Package runtime — agent_sessions.json startup reconciliation.
//
// `ReconcileAgentSessions` walks every persisted AgentSession and
// aligns its on-disk status with the OS's view of the recorded PID.
//
// Why this exists:
//   agent_sessions.json outlives the nightme daemon: a crash, a
//   `kill -9`, or an OS reboot leaves StatusRunning / StatusDetached
//   entries behind without a corresponding live process. Without a
//   reconcile pass, those entries accumulate forever — list shows
//   them as "running", kill silently fails ESRCH, and the operator
//   has no signal that the workspace is actually idle.
//
//   The runtime runs this once on startup, before any channels
//   boot, so that:
//     - the runtime pump never re-attaches to a dead PID;
//     - the daemon's user-visible status (--status, channel
//       "currently active" badges) reflects the real process
//       state;
//     - subsequent `nightme list` calls do not have to re-probe.
//
// Reconciliation policy:
//   - StatusRunning / StatusDetached with PID > 0  → probe OS.
//     If dead, flip to StatusExited, clear PID, set
//     ExitCode = exitedReconcileCode (-3) so list can show
//     "exited(-3)" and operators can distinguish a sweep kill
//     (-2, set by `nightme kill`) from an unreaped death.
//   - StatusExited / nil / unknown                   → left alone.
//   - PID == 0                                       → no probe
//     possible; left alone. Such entries are already
//     semantically "not running".
//
// The probe is injectable via the alive hook so tests can simulate
// a dead PID without actually forking processes; production calls
// use PidAlive.

package runtime

import (
	"io"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/registry"
)

// exitedReconcileCode is the ExitCode recorded for an entry whose
// PID was found dead on reconciliation. Distinct from the runtime's
// -1 (spawn failed / unknown) and the kill command's -2 (operator
// killed). Three disjoint values let list / kill / doctor pinpoint
// the cause without a heuristic.
const exitedReconcileCode = -3

// ReconciledExitCode exposes the sentinel value so the CLI's
// `nightme list` writes the same code when it independently
// reaps a dead PID during a display pass. Keeping a single source
// of truth prevents the daemon and the CLI from drifting.
func ReconciledExitCode() int { return exitedReconcileCode }

// ReconcileResult summarizes one reconcile pass.
type ReconcileResult struct {
	// Probed is the number of entries with a PID that were checked.
	Probed int
	// Reaped is the number of entries flipped to StatusExited.
	Reaped int
}

// ReconcileAgentSessions sweeps asFile, flipping dead entries to
// StatusExited. alive must be a non-nil predicate returning true
// for a live PID; pass PidAlive in production. logger is used for
// one-line per-reaped info logs; pass slog.Default() if you do not
// have one handy. out receives a one-line summary; pass io.Discard
// to silence.
//
// Safe to call on an empty / missing store — returns a zero
// ReconcileResult.
func ReconcileAgentSessions(
	asFile *registry.AgentSessionFile,
	alive func(int) bool,
	logger *slog.Logger,
	out io.Writer,
) (ReconcileResult, error) {
	if asFile == nil {
		return ReconcileResult{}, nil
	}
	if alive == nil {
		alive = PidAlive
	}
	if logger == nil {
		logger = slog.Default()
	}
	if out == nil {
		out = io.Discard
	}

	var result ReconcileResult

	for _, e := range asFile.List() {
		if e == nil {
			continue
		}
		if e.PID <= 0 {
			continue
		}
		if e.Status != registry.StatusRunning && e.Status != registry.StatusDetached {
			continue
		}

		result.Probed++
		if alive(e.PID) {
			continue
		}

		// Dead. Flip to StatusExited in-memory; persist once at the
		// end (batch Upsert is not exposed by AgentSessionFile, so
		// we Upsert per entry — acceptable at this scale; a handful
		// of stale entries per daemon lifetime).
		//
		// F-61: clear suspect state on terminal transition — a dead
		// AS has nothing left to probe. The cooldown window is
		// implicit (next Spawn overwrites SuspectReason anyway).
		// Mirrors agentsession.SetExited.
		code := exitedReconcileCode
		previousStatus := e.Status
		e.Status = registry.StatusExited
		e.PID = 0
		e.ExitCode = &code
		e.SuspectReason = ""
		e.SuspectSince = nil
		if err := asFile.Upsert(e); err != nil {
			return result, err
		}
		result.Reaped++

		logger.Info("reconciled dead agent session",
			"agentSessionId", e.ID,
			"agent", e.Agent,
			"cwd", e.Cwd,
			"previousStatus", string(previousStatus),
			"exitCode", code,
		)
	}

	if result.Reaped > 0 {
		logger.Info("agent_sessions reconciled",
			"probed", result.Probed,
			"reaped", result.Reaped,
		)
	}
	return result, nil
}
