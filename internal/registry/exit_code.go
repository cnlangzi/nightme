// Package registry — operator-visible AgentSession exit codes.
//
// The codes are disjoint so `nightme list` can tell the outcomes
// apart without a heuristic:
//
//	 0 — the agent CLI exited on its own.
//	-1 — the spawn failed, or the death is not attributable.
//	-2 — `nightme kill` terminated the session.
//	-3 — the recorded PID was already dead when the registry was
//	     reconciled.
//	-4 — nightme's own lifecycle sweep terminated the session.
package registry

const (
	// ExitCodeTerminatedByNightme is the exit code recorded for a
	// session that nightme itself terminated: the daemon's
	// shutdown sweep, or the startup sweep that collects the
	// orphans a crashed daemon left behind.
	//
	// It keeps the one bit of information that matters to an
	// operator — "nightme cleaned this up" as opposed to "the CLI
	// fell over on its own" (0) — and it is deliberately distinct
	// from the -2 that `nightme kill` writes, because that one
	// claims an operator action that did not happen.
	ExitCodeTerminatedByNightme = -4

	// ExitCodeReconciledDead is the exit code recorded for an entry
	// whose PID was already dead when the registry was reconciled.
	// Distinct from the -1 the runtime writes for a failed spawn
	// and from the -2 `nightme kill` writes for an operator kill.
	ExitCodeReconciledDead = -3
)
