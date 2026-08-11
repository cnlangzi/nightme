// Package close — /close's "stop + kill the bridge process" logic.
//
// Two entry points, both cwd-scoped to the chat's SelectedCwd:
//
//	CloseAgent(c *Cmd, agentName)    /close <agent> path
//	CloseAllAgents(c *Cmd)            /close (no args) path
//
// Per-entry graceful shutdown is owned here (5s outer bound, per-
// bridge Close goroutine fan-out, StatusRunning / StatusDetached-
// with-handle → Close, otherwise "stale-cleared"). The AgentSession
// entry is intentionally NOT touched here: /close kills the
// process but preserves session identity (pool entry, sessionID,
// agent_sessions.json row) so the next user message triggers a
// respawn that replays `--resume <id>` to continue the
// conversation.
//
// Compare to the sibling commands:
//
//   - /stop — signals the bridge to halt its in-flight turn; the
//     bridge process may or may not exit. No state mutation on
//     the AgentSession.
//   - /close — forcibly terminates the bridge process (close stdin
//     → SIGINT → 2 s grace → SIGKILL fallback). AgentSession goes
//     to StatusExited but stays in the pool; sessionID is preserved
//     so respawn resumes the conversation.
//   - /new — invokes the bridge's in-place context reset (claude's
//     `/clear`, pi's `new_session` RPC, etc.). The conversation
//     history is cleared but the bridge process stays alive.
//
// Use /close for "this bridge is wedged, give it a hard restart"
// without losing the conversation context. To fully discard the
// session (kill process AND forget conversation), use daemon
// restart or `agent_sessions.json` cleanup.
//
// Daemon shutdown (cmd/nightme/run.go) does NOT call these —
// agents survive nightme restart via the Detached registry state.
package close

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// Cmd is the per-call close context. CS is the ChatSession whose
// pool entries to operate on; Ctx is the parent context for any
// blocking bridge calls (currently unused but kept for forward
// compat with bridges that may want to wire it).
type Cmd struct {
	CS  *chatsession.ChatSession
	Ctx context.Context
}

// ErrNoContext is returned when Cmd.CS is nil. Validation should
// happen at the handler / gateway layer; this is the safety net.
var ErrNoContext = errors.New("close: nil ChatSession")

// Result is one row of the /close reply. It captures what happened
// to a single pool entry during CloseAgent / CloseAllAgents so the
// handler can render a per-agent status instead of a bare count.
type Result struct {
	Agent       string // e.g. "claude", "codex"
	Cwd         string // e.g. "/code/A"
	BeforeState chatsession.Status // StatusRunning / StatusDetached / StatusExited
	Action      string // "closed" | "stale-cleared" | "close-failed"
	Error       error  // nil on success
}

// closeGraceTotal is the outer-bound graceful shutdown timeout.
// Each bridge has its own 2s window + SIGKILL fallback; this 5s
// covers bridge grace + SIGKILL + race detector margin + bridges
// that take a beat to surface exit through ObserveClose. Tuned
// for "user waits for /close to confirm"; rarely triggers.
const closeGraceTotal = 5 * time.Second

// closeOutcome carries one goroutine's Close result back to the
// orchestrator. idx indexes into the snapshot slice.
type closeOutcome struct {
	idx int
	err error
}

// CloseAgent terminates the bridge process backing the
// (agentName, c.CS.ActiveCwd()) entry. The AgentSession entry
// itself stays in the pool; sessionID is preserved for respawn.
//
// Returns chatsession.ErrAgentNotFound when no entry matches.
// Returns ErrNoContext when c.CS is nil.
//
// Per-entry behavior:
//   - StatusRunning / StatusDetached-with-handle → Close() with
//     5s outer bound. Per-bridge 2s + SIGKILL fallback is layered
//     inside the bridge; closeGraceTotal covers a wedged bridge
//     that bypasses its own watchdog.
//   - StatusExited / StatusDetached-without-handle → "stale-cleared"
//     (no live process to signal; the entry was already dead).
//
// This function never reaches into CS private fields directly. It
// does NOT call DropAgentSession — the AgentSession is preserved so
// the next user message can respawn the bridge with `--resume
// <sessionID>` and continue the conversation.
func CloseAgent(c *Cmd, agentName string) (Result, error) {
	if c == nil || c.CS == nil {
		return Result{}, ErrNoContext
	}
	cs := c.CS
	cwd := cs.SelectedCwd()

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
		return result, nil
	}

	result.Action = "closed"
	done := make(chan error, 1)
	go func() { done <- as.Close() }()
	select {
	case err := <-done:
		if err != nil {
			result.Action = "close-failed"
			result.Error = err
		}
	case <-time.After(closeGraceTotal):
		slog.Warn("close: graceful shutdown timeout",
			"agent", as.Agent,
			"cwd", as.Cwd,
			"limit", closeGraceTotal)
		result.Action = "close-failed"
		result.Error = fmt.Errorf("graceful shutdown timed out after %s", closeGraceTotal)
	}
	return result, nil
}

// CloseAllAgents terminates every bridge process backing the pool
// entries whose Cwd == c.CS.SelectedCwd(). AgentSession entries
// are preserved so the next user message can respawn each bridge
// with `--resume <sessionID>` and continue.
//
// Returns nil + nil when selectedCwd is empty OR the pool has no
// entries in that cwd (no action taken). FormatResults on
// nil/empty renders as "No active agents to close."
//
// Per-entry behavior mirrors CloseAgent. Entries in OTHER cwds
// (from prior /cwd switches) are preserved, matching /close's
// cwd-scoped invariant.
//
// Concurrency: pool snapshot via cs.AgentSessionsInCwd (read-
// only); each bridge drives its own Close goroutine; wg.Wait
// + 5s outer timeout guards against a wedged bridge that bypasses
// its own SIGKILL fallback. No DropAgentSession call — the pool
// stays intact so respawn can resume each conversation.
func CloseAllAgents(c *Cmd) ([]Result, error) {
	if c == nil || c.CS == nil {
		return nil, ErrNoContext
	}
	cs := c.CS
	cwd := cs.SelectedCwd()
	if cwd == "" {
		return nil, nil
	}

	// 1. Snapshot entries in selectedCwd (read-only — ChatSession
	//    returns a fresh slice).
	snapshot := cs.AgentSessionsInCwd(cwd)
	if len(snapshot) == 0 {
		return nil, nil
	}

	// 2. Classify each entry + fan out graceful shutdown for
	//    alive ones. StatusRunning / StatusDetached-with-handle
	//    → Close(); StatusExited / StatusDetached-without-handle
	//    → "stale-cleared" (no live processes to signal).
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
		results[i].Action = "closed"
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
	case <-time.After(closeGraceTotal):
		slog.Warn("closeAllAgents: graceful shutdown timeout",
			"cwd", cwd,
			"limit", closeGraceTotal)
	}
	close(closeCh)

	// 4. Fold per-entry Close errors into Result (review
	//    finding #B2: previously `_ = as.Close()` was discarded).
	for oc := range closeCh {
		if oc.err != nil {
			results[oc.idx].Error = oc.err
			results[oc.idx].Action = "close-failed"
		}
	}

	return results, nil
}