// Package main — `nightme kill` subcommand.
//
// `nightme kill` terminates every agent child process that
// `nightme list` reports as alive (StatusRunning / StatusDetached
// with a usable PID) and marks the corresponding entries in
// agent_sessions.json as exited.
//
//	CHAT   AGENT   PID    RESULT            SID
//	oc_x1  claude  12345  killed            as_01HF8XXX
//	oc_x2  codex   23456  already exited    as_01HF9XXX
//	oc_x3  pi      -      skipped (no pid)  as_01HFAXXX
//
// Policy notes:
//   - Signals: SIGTERM first, then up to killGrace for the child to
//     exit on its own (agent CLIs flush their session files on
//     SIGTERM — killing them outright can lose the resume id the
//     CLI itself persists), escalating to SIGKILL when the grace
//     window expires. `--force` skips straight to SIGKILL.
//   - Registry: killed entries are marked StatusExited with PID=0 and
//     ExitCode=killedExitCode. SessionID (the resume id) is preserved
//     verbatim so the next spawn of the same (chat, agent, cwd) tuple
//     can still replay `--resume <id>` — same rule `nightme list`
//     follows when it refuses to GC an exited entry that carries one.
//   - Identity: a persisted PID is only signalled after `ps` confirms
//     it still runs the agent's binary. Entries outlive daemon
//     crashes and the OS recycles PIDs, so an unverified sweep could
//     kill an unrelated process. Mismatches are skipped and named.
//   - Scope: agent child processes only. The nightme daemon itself is
//     untouched; use `nightme stop` for that. If the daemon is running
//     it owns these children and will observe their death through its
//     own reap path.
package main

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/registry"
)

const (
	// killGrace is how long a child gets to exit after SIGTERM
	// before SIGKILL. Two seconds matches the per-bridge grace
	// window the runtime uses in its own Close() paths, so a
	// manual `nightme kill` is no harsher than a normal shutdown.
	killGrace = 2 * time.Second

	// killPollInterval is how often we re-check liveness while
	// waiting out killGrace. 50ms mirrors the daemon lifecycle
	// polling in daemon_lifecycle.go.
	killPollInterval = 50 * time.Millisecond

	// killedExitCode is the ExitCode recorded for a session that
	// this command terminated.
	//
	// -2, not -1: the runtime already writes -1 for "spawn failed /
	// died for reasons we don't know" (internal/agentsession
	// session.go respawn path), so reusing it would make
	// `list` print exited(-1) for both and destroy the one bit of
	// information worth keeping — whether a human killed this
	// session or it fell over on its own.
	killedExitCode = -2
)

// killCmdFlags captures every flag the kill subcommand accepts.
type killCmdFlags struct {
	force bool
}

// killOutcome is one row of the report printed after the sweep.
type killOutcome struct {
	row    listRow
	result string
	err    error
}

func newKillCmd() *cobra.Command {
	var f killCmdFlags

	cmd := &cobra.Command{
		Use:   "kill",
		Short: "Kill every agent process listed by `nightme list`",
		Long: "Terminate all live agent child processes (the running +\n" +
			"detached rows `nightme list` prints) and mark their\n" +
			"entries in agent_sessions.json as exited. Resume ids are\n" +
			"preserved so the sessions can be respawned later.\n\n" +
			"Each child gets SIGTERM first and 2s to exit before\n" +
			"SIGKILL; --force skips the grace window. A PID is only\n" +
			"signalled if it still runs that agent's binary, so a\n" +
			"stale entry whose PID the OS recycled is skipped rather\n" +
			"than killing an unrelated process. The nightme daemon\n" +
			"itself is not affected — use `nightme stop`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runKill(cmd, f)
		},
	}

	cmd.Flags().BoolVarP(&f.force, "force", "f", false, "SIGKILL immediately instead of SIGTERM + grace period")
	return cmd
}

// runKill loads the v1.2 stores, kills every alive agent process and
// prints one report row per session it touched.
func runKill(cmd *cobra.Command, f killCmdFlags) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	csFile, asFile, err := openV12Stores(cfg, cmd.ErrOrStderr())
	if err != nil {
		return fmt.Errorf("kill: %w", err)
	}

	// all=false keeps exited sessions out of the sweep;
	// keepExited=true suppresses list's auto-GC — kill must not
	// silently delete entries as a side effect of enumerating them.
	rows, _, err := loadListRows(csFile, asFile, false, true)
	if err != nil {
		return fmt.Errorf("kill: %w", err)
	}

	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		fmt.Fprintln(out, "kill: no live agent sessions")
		return nil
	}

	// Resolve each agent name to the binary it spawns, so the sweep
	// can prove a PID still belongs to that agent before signalling
	// it. A registry that fails to build is not fatal: we fall back
	// to "cannot verify" (see verifyPIDOwner).
	commands := agentCommands(cfg)

	outcomes := make([]killOutcome, 0, len(rows))
	var firstErr error
	for _, r := range rows {
		oc := killSession(asFile, r, commands[r.Agent], f.force)
		outcomes = append(outcomes, oc)
		if oc.err != nil && firstErr == nil {
			firstErr = oc.err
		}
	}

	printKillTable(out, outcomes)
	if firstErr != nil {
		return fmt.Errorf("kill: %w", firstErr)
	}
	return nil
}

// killSession terminates one session's child process and updates its
// registry entry. Every failure mode is reported in the returned
// outcome rather than aborting the sweep: one unkillable child must
// not leave the remaining ones running.
//
// wantCommand is the binary the entry's agent is expected to run
// (e.g. "claude"); empty means "unknown, cannot verify" — see
// verifyPIDOwner for why that matters.
func killSession(asFile *registry.AgentSessionFile, r listRow, wantCommand string, force bool) killOutcome {
	switch {
	case r.PID <= 0:
		// Detached/ACP sessions may carry no PID at all — there is
		// nothing to signal, and we cannot prove the session is
		// dead, so the entry is left untouched.
		return killOutcome{row: r, result: "skipped (no pid)"}
	case r.PID == os.Getpid():
		// Defensive: a corrupt store must never make us kill the
		// process running this command.
		return killOutcome{row: r, result: "skipped (self)"}
	}

	// PIDs in agent_sessions.json can be arbitrarily stale: entries
	// survive a daemon crash, and the OS recycles PIDs. Signalling a
	// recycled PID would kill an unrelated process — the user's
	// editor, another agent, anything. Verify the PID still belongs
	// to the expected binary before touching it. This matters most
	// in exactly the scenario `kill` is for: a crashed daemon that
	// left stale "running" entries behind.
	if actual, ok := verifyPIDOwner(r.PID, wantCommand); !ok {
		return killOutcome{row: r, result: fmt.Sprintf("skipped (pid %d is %q, not %s)", r.PID, actual, wantCommand)}
	}

	result := "killed"
	switch err := killProcess(r.PID, force); {
	case err == nil:
	case isProcessGone(err):
		// The child died on its own between list and kill — the
		// desired end state, so we still mark the entry exited.
		result = "already exited"
	default:
		return killOutcome{row: r, result: "failed", err: fmt.Errorf("pid %d: %w", r.PID, err)}
	}

	if err := markSessionExited(asFile, r.AgentSessionID); err != nil {
		return killOutcome{row: r, result: result + " (registry update failed)", err: err}
	}
	return killOutcome{row: r, result: result}
}

// agentCommands maps agent name → the binary that agent spawns
// (e.g. "claude" → "claude"), used to prove a persisted PID has not
// been recycled. Returns an empty map when the registry cannot be
// built; every lookup then yields "" and verification is skipped
// rather than blocking the sweep.
func agentCommands(cfg *config.Config) map[string]string {
	reg := agentregistry.Build(cfg, "")
	if reg == nil {
		return map[string]string{}
	}
	out := make(map[string]string)
	for _, s := range reg.List() {
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

// markSessionExited flips one agent session entry to StatusExited.
// SessionID is deliberately left in place: it is the agent's own
// resume id, and dropping it here would make the session
// unresumable — the same reason `nightme list` refuses to GC exited
// entries that still carry one.
//
// One Upsert per entry means one file rewrite per killed session.
// That is acceptable at this scale (a handful of agents per host)
// and keeps kill's write path identical to the runtime's.
func markSessionExited(asFile *registry.AgentSessionFile, id string) error {
	e, ok := asFile.Get(id)
	if !ok || e == nil {
		// Raced with the daemon deleting the entry — the end state
		// we wanted is already on disk.
		return nil
	}
	code := killedExitCode
	e.Status = registry.StatusExited
	e.PID = 0
	e.ExitCode = &code
	if err := asFile.Upsert(e); err != nil {
		return fmt.Errorf("mark %s exited: %w", id, err)
	}
	return nil
}

// printKillTable writes the per-session report. The header is always
// emitted so an operator can tell an empty sweep from a no-op run.
func printKillTable(w io.Writer, outcomes []killOutcome) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHAT\tAGENT\tPID\tRESULT\tSID")
	for _, oc := range outcomes {
		result := oc.result
		if oc.err != nil {
			result = fmt.Sprintf("%s: %v", result, oc.err)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			oc.row.ChatID,
			oc.row.Agent,
			pidCell(oc.row.PID),
			result,
			oc.row.AgentSessionID,
		)
	}
	tw.Flush()
}
