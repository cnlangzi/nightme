// Package-level kill functions for /kill's process-shutdown
// logic. Two entry points, no methods on ChatSession:
//
//	KillAgent(c *KillCmd, agentName)    /kill <agent> path
//	KillAllAgents(c *KillCmd)            /kill (no args) path
//
// Both functions:
//   - Per-entry graceful shutdown via bridge.Close (5s outer bound)
//   - Clear cs.activeAS iff it pointed to a killed entry
//     (dangling-pointer safety invariant; not a /kill state change)
//   - Delete the killed entries' agent_sessions.json rows
//   - Preserve cs.activeCwd / cs.activeAgent / queue / InputBuffer
//   - Preserve entries in OTHER cwds (cwd-scoped invariant)
//
// The slash handler at internal/command/kill/cmd.go wraps these
// with RequireActiveCwd preflight + FormatKillResults rendering.
// Daemon shutdown (cmd/nightme/run.go) does NOT call these —
// agents survive nightme restart via the Detached registry state.
package chatsession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// KillCmd is the per-call kill context. CS is the ChatSession
// whose pool entries to operate on; Ctx is the parent context
// for any blocking bridge calls (currently unused but kept for
// forward compat with bridges that may want to wire it).
type KillCmd struct {
	CS  *ChatSession
	Ctx context.Context
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
// Returns ErrAgentNotFound when no entry matches. Returns a
// generic error when c.CS is nil.
//
// Per-entry behavior:
//   - StatusRunning / StatusDetached-with-handle → Close() with
//     5s outer bound. Per-bridge 2s + SIGKILL fallback is layered
//     inside the bridge; killGraceTotal covers a wedged bridge
//     that bypasses its own watchdog.
//   - StatusExited / StatusDetached-without-handle → "stale-cleared"
//     (no live process to signal; just clean disk).
//   - activeAS is cleared iff it pointed to the killed entry.
//   - The killed entry's agent_sessions.json row is deleted.
//
// Preserved (matching /kill semantics — only the targeted process
// dies, chat state and other entries are untouched):
//   - cs.activeCwd / cs.activeAgent / queue / InputBuffer
//   - Sibling entries in OTHER cwds / OTHER agent names
func KillAgent(c *KillCmd, agentName string) (KillResult, error) {
	if c == nil || c.CS == nil {
		return KillResult{}, errors.New("chatsession: kill: nil ChatSession")
	}
	cs := c.CS
	cwd := cs.ActiveCwd()

	cs.mu.RLock()
	as, ok := cs.pool[agentCwdKey{Agent: agentName, Cwd: cwd}]
	cs.mu.RUnlock()
	if !ok {
		return KillResult{}, ErrAgentNotFound
	}

	result := KillResult{
		Agent:       as.Agent,
		Cwd:         as.Cwd,
		BeforeState: as.Status(),
	}

	isAlive := as.Status() == StatusRunning ||
		(as.Status() == StatusDetached && as.Handle() != nil)
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

	cs.mu.Lock()
	delete(cs.pool, agentCwdKey{Agent: agentName, Cwd: cwd})
	if cs.activeAS == as {
		cs.activeAS = nil
	}
	cs.mu.Unlock()

	if cs.asFile != nil {
		_ = cs.asFile.Delete(as.ID)
	}
	cs.persistChatEntry()
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
// Concurrency: pool mutation under write lock; persist after
// release. Each bridge drives its own Close goroutine; wg.Wait
// + 5s outer timeout guards against a wedged bridge that bypasses
// its own SIGKILL fallback.
func KillAllAgents(c *KillCmd) ([]KillResult, error) {
	if c == nil || c.CS == nil {
		return nil, errors.New("chatsession: kill: nil ChatSession")
	}
	cs := c.CS
	cwd := cs.ActiveCwd()
	if cwd == "" {
		return nil, nil
	}

	// 1. Snapshot entries in activeCwd under read lock.
	cs.mu.RLock()
	snapshot := make([]*AgentSession, 0)
	for _, as := range cs.pool {
		if as.Cwd == cwd {
			snapshot = append(snapshot, as)
		}
	}
	cs.mu.RUnlock()

	if len(snapshot) == 0 {
		return nil, nil
	}

	// 2. Classify each entry + fan out graceful shutdown for
	//    alive ones. StatusRunning / StatusDetached-with-handle
	//    → Close(); StatusExited / StatusDetached-without-handle
	//    → "stale-cleared" (no process to signal).
	results := make([]KillResult, len(snapshot))
	closeCh := make(chan closeOutcome, len(snapshot))
	var wg sync.WaitGroup
	for i, as := range snapshot {
		results[i] = KillResult{
			Agent:       as.Agent,
			Cwd:         as.Cwd,
			BeforeState: as.Status(),
		}
		isAlive := as.Status() == StatusRunning ||
			(as.Status() == StatusDetached && as.Handle() != nil)
		if !isAlive {
			results[i].Action = "stale-cleared"
			continue
		}
		results[i].Action = "killed"
		wg.Add(1)
		go func(i int, as *AgentSession) {
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

	// 5. Pool + activeAS cleanup. Per-entry delete to preserve
	//    entries in OTHER cwds. activeAS is cleared iff it pointed
	//    to a killed entry.
	cs.mu.Lock()
	activeWasKilled := false
	for _, as := range snapshot {
		delete(cs.pool, agentCwdKey{Agent: as.Agent, Cwd: as.Cwd})
		if cs.activeAS == as {
			activeWasKilled = true
		}
	}
	if activeWasKilled {
		cs.activeAS = nil
	}
	cs.mu.Unlock()

	// 6. Delete the killed entries' agent_sessions.json rows.
	//    Other entries' rows are preserved (orphan GC from the
	//    historical KillAll is NOT applied here — /kill only
	//    touches its own scope).
	if cs.asFile != nil {
		for _, as := range snapshot {
			_ = cs.asFile.Delete(as.ID)
		}
	}
	cs.persistChatEntry()
	return results, nil
}