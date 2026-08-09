// Package kill — /kill's process-shutdown logic.
//
// Two entry points, both cwd-scoped to the chat's activeCwd:
//
//	KillAgent(c *Cmd, agentName)    /kill <agent> path
//	KillAllAgents(c *Cmd)            /kill (no args) path
//
// Per-entry graceful shutdown is owned here (5s outer bound, per-
// bridge Close goroutine fan-out, StatusRunning / StatusDetached-
// with-handle → Close, otherwise "stale-cleared"). Pool + activeAS
// + agent_sessions.json cleanup is delegated to ChatSession's
// lifecycle primitives (LookupInPool / AgentSessionsInCwd /
// DropAgentSession) — this package never reaches into ChatSession's
// private fields.
//
// Preserved invariants (matching /kill semantics):
//   - cs.activeCwd / cs.activeAgent / queue / InputBuffer
//   - Sibling entries in OTHER cwds / OTHER agent names
//
// Daemon shutdown (cmd/nightme/run.go) does NOT call these —
// agents survive nightme restart via the Detached registry state.
package kill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// Cmd is the per-call kill context. CS is the ChatSession whose
// pool entries to operate on; Ctx is the parent context for any
// blocking bridge calls (currently unused but kept for forward
// compat with bridges that may want to wire it).
type Cmd struct {
	CS  *chatsession.ChatSession
	Ctx context.Context
}

// ErrNoContext is returned when Cmd.CS is nil. Validation should
// happen at the handler / gateway layer; this is the safety net.
var ErrNoContext = errors.New("kill: nil ChatSession")

// Result is one row of the /kill reply. It captures what happened
// to a single pool entry during KillAgent / KillAllAgents so the
// handler can render a per-agent status instead of a bare count.
type Result struct {
	Agent       string // e.g. "claude", "codex"
	Cwd         string // e.g. "/code/A"
	BeforeState chatsession.Status // StatusRunning / StatusDetached / StatusExited
	Action      string // "killed" | "stale-cleared"
	Error       error  // nil on success
}

// killGraceTotal is the outer-bound graceful shutdown timeout.
// Each bridge has its own 2s window + SIGKILL fallback; this 5s
// covers bridge grace + SIGKILL + race detector margin + bridges
// that take a beat to surface exit through ObserveClose. Tuned
// for "user waits for /kill to confirm"; rarely triggers.
const killGraceTotal = 5 * time.Second

// closeOutcome carries one goroutine's Close result back to the
// orchestrator. idx indexes into the snapshot slice.
type closeOutcome struct {
	idx int
	err error
}

// KillAgent terminates the (agentName, c.CS.ActiveCwd()) entry.
//
// Returns chatsession.ErrAgentNotFound when no entry matches.
// Returns ErrNoContext when c.CS is nil.
//
// Per-entry behavior:
//   - StatusRunning / StatusDetached-with-handle → Close() with
//     5s outer bound. Per-bridge 2s + SIGKILL fallback is layered
//     inside the bridge; killGraceTotal covers a wedged bridge
//     that bypasses its own watchdog.
//   - StatusExited / StatusDetached-without-handle → "stale-cleared"
//     (no live process to signal; just clean disk).
//
// After Close (or the stale-clear decision), ChatSession.DropAgentSession
// handles pool + activeAS + agent_sessions.json cleanup. This
// function never reaches into CS private fields directly.
func KillAgent(c *Cmd, agentName string) (Result, error) {
	if c == nil || c.CS == nil {
		return Result{}, ErrNoContext
	}
	cs := c.CS
	cwd := cs.ActiveCwd()

	as, err := cs.LookupInPool(agentName, cwd)
	if err != nil {
		return Result{}, chatsession.ErrAgentNotFound
	}

	result := Result{
		Agent:       as.Agent,
		Cwd:         as.Cwd,
		BeforeState: as.Status(),
	}

	isAlive := as.Status() == chatsession.StatusRunning ||
		(as.Status() == chatsession.StatusDetached && as.Handle() != nil)
	if !isAlive {
		result.Action = "stale-cleared"
	} else {
		result.Action = "killed"
		done := make(chan error, 1)
		go func() { done <- as.Close() }()
		select {
		case err := <-done:
			if err != nil {
				result.Action = "kill-failed"
				result.Error = err
			}
		case <-time.After(killGraceTotal):
			slog.Warn("kill: graceful shutdown timeout",
				"agent", as.Agent,
				"cwd", as.Cwd,
				"limit", killGraceTotal)
			result.Action = "kill-failed"
			result.Error = fmt.Errorf("graceful shutdown timed out after %s", killGraceTotal)
		}
	}

	cs.DropAgentSession(as)
	return result, nil
}

// KillAllAgents terminates every pool entry whose Cwd ==
// c.CS.ActiveCwd().
//
// Returns nil + nil when activeCwd is empty OR the pool has no
// entries in that cwd (no action taken). FormatKillResults on
// nil/empty renders as "No active agents to kill."
//
// Per-entry behavior mirrors KillAgent. Pool + activeAS cleanup
// is scoped to the matching entries only; entries in OTHER cwds
// (from prior /cwd switches) are preserved, matching /kill's
// cwd-scoped invariant.
//
// Concurrency: pool snapshot via cs.AgentSessionsInCwd (read-
// only); each bridge drives its own Close goroutine; wg.Wait
// + 5s outer timeout guards against a wedged bridge that bypasses
// its own SIGKILL fallback. Per-entry cleanup via cs.DropAgentSession.
func KillAllAgents(c *Cmd) ([]Result, error) {
	if c == nil || c.CS == nil {
		return nil, ErrNoContext
	}
	cs := c.CS
	cwd := cs.ActiveCwd()
	if cwd == "" {
		return nil, nil
	}

	// 1. Snapshot entries in activeCwd (read-only — ChatSession
	//    returns a fresh slice).
	snapshot := cs.AgentSessionsInCwd(cwd)
	if len(snapshot) == 0 {
		return nil, nil
	}

	// 2. Classify each entry + fan out graceful shutdown for
	//    alive ones. StatusRunning / StatusDetached-with-handle
	//    → Close(); StatusExited / StatusDetached-without-handle
	//    → "stale-cleared" (no process to signal).
	results := make([]Result, len(snapshot))
	closeCh := make(chan closeOutcome, len(snapshot))
	var wg sync.WaitGroup
	for i, as := range snapshot {
		results[i] = Result{
			Agent:       as.Agent,
			Cwd:         as.Cwd,
			BeforeState: as.Status(),
		}
		isAlive := as.Status() == chatsession.StatusRunning ||
			(as.Status() == chatsession.StatusDetached && as.Handle() != nil)
		if !isAlive {
			results[i].Action = "stale-cleared"
			continue
		}
		results[i].Action = "killed"
		wg.Add(1)
		go func(i int, as *chatsession.AgentSession) {
			defer wg.Done()
			closeCh <- closeOutcome{idx: i, err: as.Close()}
		}(i, as)
	}

	// 3. Wait for all bridges to confirm exit (5s outer bound).
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(killGraceTotal):
		slog.Warn("killAllAgents: graceful shutdown timeout",
			"cwd", cwd,
			"limit", killGraceTotal)
	}
	close(closeCh)

	// 4. Fold per-entry Close errors into KillResult (review
	//    finding #B2: previously `_ = as.Close()` was discarded).
	for oc := range closeCh {
		if oc.err != nil {
			results[oc.idx].Error = oc.err
			results[oc.idx].Action = "kill-failed"
		}
	}

	// 5. Per-entry cleanup via DropAgentSession. activeAS is
	//    cleared by DropAgentSession iff it pointed to one of
	//    the killed entries (safety invariant).
	for _, as := range snapshot {
		cs.DropAgentSession(as)
	}
	return results, nil
}