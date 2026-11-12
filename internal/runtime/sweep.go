// sweep.go — startup cleanup of the agent child processes a
// previous, crashed daemon left behind.
//
// agent_sessions.json outlives the nightme daemon. A daemon that was
// `kill -9`ed — or that crashed — leaves its agent children running:
// there is no context to cancel, so nothing terminates them. The
// next daemon finds their registry entries still claiming
// "running" with a PID that belongs to a process it does not own.
//
// SweepOrphanAgents is that cleanup. It runs at startup, right after
// ReconcileAgentSessions and before any channel starts, so it can
// never touch a process the current daemon owns.
//
// Two guards keep the sweep from killing something it must not:
//
//   - PID-ownership verification: the PID's running binary is
//     compared against the agent's configured command. A positive
//     mismatch means the OS recycled the PID onto an unrelated
//     process — that process is left alone and the mismatch is
//     logged. The check fails open when the expected command is
//     unknown or the binary name cannot be read; the same policy
//     `nightme kill` uses.
//   - The resume id (SessionID) is preserved, so the next user
//     message respawns the agent with `--resume <id>` and the
//     conversation continues where it stopped.
//
// A positively identified agent CLI is terminated together with its
// process group: the CLI leads its own group, and the tool
// subprocesses it spawned live in it. Killing only the CLI would
// orphan them.

package runtime

import (
	"log/slog"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/procsweep"
	"github.com/cnlangzi/nightme/internal/registry"
)

// SweepOrphanAgents terminates the agent child processes a previous,
// crashed daemon left behind, and records them as exited.
//
// Every entry whose status is running / detached and whose PID is
// still alive is an orphan: the daemon that spawned it is gone, and
// nothing else would ever terminate it. The process is stopped with
// the shared sweep policy (SIGTERM → grace → SIGKILL) and the entry
// is rewritten as exited / registry.ExitCodeTerminatedByNightme /
// pid 0, with the session id preserved.
//
// Entries whose PID is already dead are left alone —
// ReconcileAgentSessions has already flipped those to exited with
// registry.ExitCodeReconciledDead.
//
// alive is the liveness predicate; pass nil for PidAlive. agents
// resolves an agent name to the binary it spawns, for the
// ownership check; a nil registry means every entry is treated as
// "unknown command" and the ownership check fails open.
//
// Returns the number of entries swept.
func SweepOrphanAgents(
	asFile *registry.AgentSessionFile,
	agents *agent.Registry,
	alive func(int) bool,
	logger *slog.Logger,
) (int, error) {
	if asFile == nil {
		return 0, nil
	}
	if alive == nil {
		alive = PidAlive
	}
	if logger == nil {
		logger = slog.Default()
	}

	commands := agentCommands(agents)
	swept := 0

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
		if !alive(e.PID) {
			// Dead already — ReconcileAgentSessions owns that
			// transition (exit code -3).
			continue
		}

		want := commands[e.Agent]
		owner := procsweep.VerifyOwner(e.PID, want)
		if !owner.Match {
			logger.Warn("skipped orphan agent process: pid runs a different binary",
				"agentSessionId", e.ID,
				"agent", e.Agent,
				"pid", e.PID,
				"actual", owner.Binary,
				"expected", want)
			continue
		}

		// A positively identified agent CLI leads its own process
		// group, so the group is signalled: the tool subprocesses it
		// spawned die with it instead of being orphaned. An
		// unverified PID is signalled on its own — its group may
		// belong to an unrelated process.
		kill := procsweep.Kill
		if owner.Verified {
			kill = procsweep.KillGroup
		}

		if err := kill(e.PID, false); err != nil && !procsweep.IsProcessGone(err) {
			logger.Warn("failed to terminate orphan agent process",
				"agentSessionId", e.ID,
				"agent", e.Agent,
				"pid", e.PID,
				"err", err)
			continue
		}

		code := registry.ExitCodeTerminatedByNightme
		e.Status = registry.StatusExited
		e.PID = 0
		e.ExitCode = &code
		e.SuspectReason = ""
		e.SuspectSince = nil
		if err := asFile.Upsert(e); err != nil {
			return swept, err
		}
		swept++

		logger.Info("terminated orphan agent process",
			"agentSessionId", e.ID,
			"agent", e.Agent,
			"cwd", e.Cwd,
			"exitCode", code)
	}

	if swept > 0 {
		logger.Info("orphan agent processes swept", "swept", swept)
	}
	return swept, nil
}

// agentCommands maps an agent name to the binary that agent spawns
// (e.g. "claude" → "claude"), used to prove a persisted PID has not
// been recycled.
//
// An agent with no configured command is absent from the map; the
// ownership check then fails open for it, which is the same policy
// `nightme kill` applies.
func agentCommands(agents *agent.Registry) map[string]string {
	out := map[string]string{}
	if agents == nil {
		return out
	}
	for _, s := range agents.List() {
		if s == nil {
			continue
		}
		info := s.Info()
		if info.Name != "" && info.Command != "" {
			out[info.Name] = info.Command
		}
	}
	return out
}
