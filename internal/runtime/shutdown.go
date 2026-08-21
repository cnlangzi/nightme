// shutdown.go — daemon teardown. Exposed as ShutdownRun for
// cmd/nightme tests that want to assert the prcache cleanup
// contract end-to-end.

package runtime

import (
	"context"
	"fmt"
	"github.com/cnlangzi/nightme/internal/chatstore"
	"io"
	"log/slog"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/prcache"
	"github.com/cnlangzi/nightme/internal/registry"
)

// ShutdownRun stops the channel and persists final state.
//
// # Agent processes are INTENTIONALLY NOT killed here — they are
//
// Agent processes are INTENTIONALLY NOT killed here — they are
// long-running CLI sessions independent of nightme's lifetime.
// AgentSessions that were Running remain in the registry as
// Detached; the next `nightme run` re-attach path (Manager.RestoreFromRegistry
// + FromAgentSessionEntry) hands them back to nightme, and
// LookupActiveAgentSession reuses them via --resume where the
// bridge supports it. /kill is the only path that terminates
// agent processes; it is cwd-scoped and runs in chatsession.KillAgent /
// chatsession.KillAllAgents (see internal/chatsession/kill.go).
//
// Persistence: chat_sessions.json + agent_sessions.json are left
// in place. The Manager has been writing through to them
// throughout the run via WithPersistence.
//
// v1.3+ multi-channel: ShutdownRun takes a single channel + mgr
// (the legacy single-channel path used by v0.x tests). New
// callers should use ShutdownRunMulti, which iterates all
// per-channel mgrs in runtime.allMgrs and stops every
// successfully-started channel.
func ShutdownRun(out io.Writer, ch channel.Channel, mgr *chatsession.Manager, csFile *chatstore.Store, asFile *registry.AgentSessionFile, prReg *prcache.Registry, logger *slog.Logger) error {
	_ = out
	_ = asFile
	if logger == nil {
		logger = slog.Default()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
// It stops every channel the runtime successfully started and
// flushes state for every per-channel Manager in runtime.allMgrs.
func ShutdownRunMulti(
	out io.Writer,
	chs []channel.Channel,
	csFile *chatstore.Store,
	asFile *registry.AgentSessionFile,
	prReg *prcache.Registry,
	logger *slog.Logger,
) error {
	_ = out
	_ = asFile
	if logger == nil {
		logger = slog.Default()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

	if prReg != nil {
		prReg.CloseAll()
	}

	// Persist final state from every per-channel mgr.
	allMgrsMu.RLock()
	mgrs := append([]*chatsession.Manager(nil), allMgrs...)
	allMgrsMu.RUnlock()
	for _, mgr := range mgrs {
		if mgr == nil {
			continue
		}
		persistChatStates(mgr, csFile)
	}

	return firstErr
}

// persistChatStates is intentionally a no-op: chatstore SetXxx
// already sync-persists on every mutation (docs/CHATSTORE.md).
// Kept as a named helper so ShutdownRun / ShutdownRunMulti call
// sites stay stable.
func persistChatStates(mgr *chatsession.Manager, csFile *chatstore.Store) {
	_ = mgr
	_ = csFile
}
