// shutdown.go — daemon teardown. Exposed as ShutdownRun for
// cmd/nightme tests that want to assert the prcache cleanup
// contract end-to-end.
//
// The daemon owns the lifetime of every agent child process it
// spawns: the process scope is the parent context of all of them,
// and cancelling it terminates them (SIGTERM → grace → SIGKILL to
// the child's process group). So a daemon that exits by any path —
// SIGTERM / SIGINT, `nightme stop`, `nightme restart`, the tray's
// stop / restart, Ctrl-C in `nightme run` — leaves no agent process
// behind.
//
// The shutdown sequence is ordered:
//
//  1. stop every channel, so no new inbound message can trigger a
//     spawn;
//  2. mark every chat as shutting down and reset its operation
//     contexts, so in-flight bridge calls unblock;
//  3. cancel the process scope, which terminates the agent children;
//  4. wait (bounded) for the children to exit, so their exit events
//     are recorded in agent_sessions.json;
//  5. persist the final state.
//
// Step 2 must precede step 3: the "shutting down" state has to be
// recorded before the exit events arrive, otherwise a session the
// daemon terminated would be recorded with exit code 0 instead of
// registry.ExitCodeTerminatedByNightme.
//
// The shared dsh host is deliberately NOT part of the process scope:
// it is a reusable persistent service on 127.0.0.1:3080 and the next
// daemon re-attaches to it.

package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/chatstore"
	"github.com/cnlangzi/nightme/internal/prcache"
	"github.com/cnlangzi/nightme/internal/registry"
)

const (
	// channelStopBudget bounds how long the daemon waits for the
	// channels to stop. It plus agentExitBudget has to stay inside
	// the 15s window `nightme stop` waits for (lifecycleWait in
	// cmd/nightme/daemon_lifecycle.go).
	channelStopBudget = 5 * time.Second

	// agentExitBudget bounds how long the daemon waits for the
	// agent children to exit after the process scope is cancelled.
	//
	// It has to cover the LONGEST teardown sequence among the agent
	// families, not the shortest: the exec family gives its child a
	// 3s SIGTERM grace (proc.AgentGrace), while the PTY family
	// waits 5s (pty.childExitGrace) before it escalates to SIGKILL.
	// A budget shorter than that would let the daemon exit while a
	// wedged PTY child is still waiting for its SIGKILL — the kill
	// timer dies with the daemon and the child is orphaned.
	//
	// The margin on top of the 5s covers the SIGKILL, the reap and
	// the exit-event drain. Together with channelStopBudget the
	// shutdown stays inside the 15s window `nightme stop` waits for.
	agentExitBudget = 7 * time.Second

	// agentExitPoll is how often the agent children are re-checked
	// while waiting out agentExitBudget. 50ms mirrors the daemon
	// lifecycle polling in cmd/nightme/daemon_lifecycle.go.
	agentExitPoll = 50 * time.Millisecond
)

// ShutdownRun stops the channel and persists final state.
//
// It is the legacy single-channel path (v0.x tests). New callers
// should use ShutdownRunMulti, which iterates all per-channel mgrs,
// stops every successfully-started channel and terminates the agent
// child processes.
//
// Persistence: chat_sessions.json + agent_sessions.json are left
// in place. The Manager has been writing through to them
// throughout the run via WithPersistence.
func ShutdownRun(out io.Writer, ch channel.Channel, mgr *chatsession.Manager, csFile *chatstore.Store, asFile *registry.AgentSessionFile, prReg *prcache.Registry, logger *slog.Logger) error {
	_ = out
	_ = asFile
	if logger == nil {
		logger = slog.Default()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), channelStopBudget)
	defer cancel()

	var firstErr error
	if ch != nil {
		if err := ch.Stop(shutdownCtx); err != nil {
			firstErr = fmt.Errorf("run: stop channel: %w", err)
		}
	}

	if prReg != nil {
		prReg.CloseAll()
	}

	persistChatStates(mgr, csFile)

	return firstErr
}

// ShutdownRunMulti is the v1.3+ multi-channel shutdown path.
//
// It stops every channel the runtime successfully started, terminates
// the agent child processes, and flushes state for every per-channel
// Manager in runtime.allMgrs.
//
// cancelProcs cancels the process scope — the parent context of every
// agent child process. Pass nil when there is no scope (the legacy
// single-channel path).
func ShutdownRunMulti(
	out io.Writer,
	chs []channel.Channel,
	csFile *chatstore.Store,
	asFile *registry.AgentSessionFile,
	prReg *prcache.Registry,
	logger *slog.Logger,
	cancelProcs context.CancelFunc,
) error {
	_ = out
	if logger == nil {
		logger = slog.Default()
	}

	// (1) Stop every channel first: no new inbound message may
	// trigger a spawn while the daemon tears down.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), channelStopBudget)
	defer cancel()

	var firstErr error
	for _, ch := range chs {
		if ch == nil {
			continue
		}
		if err := ch.Stop(shutdownCtx); err != nil {
			firstErr = fmt.Errorf("run: stop channel %s: %w", ch.Name(), err)
			logger.Warn("channel stop failed", "name", ch.Name(), "err", err)
		}
	}

	mgrs := snapshotManagers()

	// (2) Mark every chat as shutting down and cancel its operation
	// contexts, so in-flight bridge calls unblock promptly instead
	// of hanging until the daemon exits.
	markChatsShuttingDown(mgrs)
	resetChatContexts(mgrs)

	// (3) Cancel the process scope: every agent child gets SIGTERM,
	// then SIGKILL once its grace window expires.
	if cancelProcs != nil {
		cancelProcs()
	}

	// (4) Wait for the agent children to exit. Their exit events
	// are what record the exit code in agent_sessions.json.
	waitForAgentExit(mgrs, agentExitBudget, logger)

	if prReg != nil {
		prReg.CloseAll()
	}

	// (5) Persist the final state from every per-channel mgr.
	for _, mgr := range mgrs {
		if mgr == nil {
			continue
		}
		persistChatStates(mgr, csFile)
	}
	persistAgentSessions(mgrs, asFile)

	return firstErr
}

// snapshotManagers returns the per-channel managers the daemon owns.
func snapshotManagers() []*chatsession.Manager {
	allMgrsMu.RLock()
	defer allMgrsMu.RUnlock()
	return append([]*chatsession.Manager(nil), allMgrs...)
}

// newProcScope creates the daemon's process scope — the parent
// context of every agent child process — together with the cancel
// function that tears it down.
//
// Cancelling the scope is what stops the children, so the cancel
// marks every known chat as shutting down FIRST: the exit events
// that follow have to be attributed to the shutdown instead of
// being reported as crashes.
func newProcScope() (context.Context, context.CancelFunc) {
	scope, cancel := context.WithCancel(context.Background())
	return scope, func() {
		markChatsShuttingDown(snapshotManagers())
		cancel()
	}
}

// markChatsShuttingDown flags every chat as shutting down.
//
// The flag is what tells the exit path the daemon stopped the agent
// on purpose: it picks registry.ExitCodeTerminatedByNightme over 0
// and makes the chat report the shutdown notice instead of a crash.
func markChatsShuttingDown(mgrs []*chatsession.Manager) {
	for _, mgr := range mgrs {
		if mgr == nil {
			continue
		}
		for _, cs := range mgr.List() {
			if cs == nil {
				continue
			}
			cs.MarkShuttingDown()
		}
	}
}

// resetChatContexts cancels every chat's operation contexts, so
// in-flight bridge calls unblock promptly instead of hanging until
// the daemon exits.
func resetChatContexts(mgrs []*chatsession.Manager) {
	for _, mgr := range mgrs {
		if mgr == nil {
			continue
		}
		for _, cs := range mgr.List() {
			if cs == nil {
				continue
			}
			cs.ResetContext()
		}
	}
}

// waitForAgentExit waits until every agent child has exited, or the
// budget runs out.
//
// The children report their death through the exit event, and that
// event is what records the exit code. Waiting for the event (rather
// than for a fixed delay) keeps the shutdown as short as the agents
// actually need.
func waitForAgentExit(mgrs []*chatsession.Manager, budget time.Duration, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	deadline := time.Now().Add(budget)
	for {
		if !anyAgentRunning(mgrs) {
			return
		}
		if !time.Now().Before(deadline) {
			logger.Warn("agent children did not exit within the shutdown budget",
				"budget", budget)
			return
		}
		time.Sleep(agentExitPoll)
	}
}

// anyAgentRunning reports whether any chat still holds a running
// agent session — i.e. a child process that has not been reaped yet.
//
// A session that never had a process (an SDK-style backend) or one
// that is already exited does not hold the shutdown up.
func anyAgentRunning(mgrs []*chatsession.Manager) bool {
	for _, as := range allAgentSessions(mgrs) {
		if as.Status() == chatsession.StatusRunning && as.PID() > 0 {
			return true
		}
	}
	return false
}

// allAgentSessions returns every AgentSession the daemon knows about:
// the ones in each chat's active pool plus the ones parked in the
// shared warm pool.
//
// The warm pool matters: a session parked by /cwd keeps its child
// process alive and is no longer in the chat's own pool, so a
// shutdown that only looked at the chat pools would not wait for it
// and would not record its exit code.
func allAgentSessions(mgrs []*chatsession.Manager) []*chatsession.AgentSession {
	seen := map[string]bool{}
	var out []*chatsession.AgentSession
	add := func(ases []*chatsession.AgentSession) {
		for _, as := range ases {
			if as == nil || seen[as.ID] {
				continue
			}
			seen[as.ID] = true
			out = append(out, as)
		}
	}
	for _, mgr := range mgrs {
		if mgr == nil {
			continue
		}
		for _, cs := range mgr.List() {
			if cs == nil {
				continue
			}
			add(cs.Pool())
		}
		add(mgr.AgentSessionPool().List())
	}
	return out
}

// persistAgentSessions writes the final state of every AgentSession
// to agent_sessions.json.
//
// The per-AS persist callback already flushes every lifecycle
// transition, but it runs AFTER the status flip — the daemon could
// exit between the two and lose the row's final state. This final
// pass closes that window: by now every agent session has reached a
// terminal state, so the row on disk is the truth.
func persistAgentSessions(mgrs []*chatsession.Manager, asFile *registry.AgentSessionFile) {
	if asFile == nil {
		return
	}
	for _, as := range allAgentSessions(mgrs) {
		if err := asFile.Upsert(as.Entry()); err != nil {
			slog.Warn("persist agent session on shutdown failed", "as_id", as.ID, "err", err)
		}
	}
}

// persistChatStates is intentionally a no-op: chatstore SetXxx
// already sync-persists on every mutation (docs/CHATSTORE.md).
// Kept as a named helper so ShutdownRun / ShutdownRunMulti call
// sites stay stable.
func persistChatStates(mgr *chatsession.Manager, csFile *chatstore.Store) {
	_ = mgr
	_ = csFile
}
