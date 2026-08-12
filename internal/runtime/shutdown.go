// shutdown.go — daemon teardown. Exposed as ShutdownRun for
// cmd/nightme tests that want to assert the prcache cleanup
// contract end-to-end.

package runtime

import (
	"context"
	"fmt"
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
func ShutdownRun(out io.Writer, ch channel.Channel, mgr *chatsession.Manager, csFile *registry.ChatSessionFile, asFile *registry.AgentSessionFile, prReg *prcache.Registry, logger *slog.Logger) error {
	_ = out // future shutdown status line
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

	// Cancel every per-AgentSession PR-cache refresh goroutine
	// so the daemon doesn't exit mid-`gh pr list`. Best-effort:
	// the goroutines are stateless (HTTP/git calls), so a missed
	// cancel only wastes a few round-trips, not state corruption.
	// We do this BEFORE persisting chat state so any in-flight
	// refresh that was about to land back into a Cache sees the
	// cancel signal at its next checkpoint and exits silently.
	if prReg != nil {
		prReg.CloseAll()
	}

	if mgr != nil {
		// Persist final state.
		for _, cs := range mgr.List() {
			// Touch lastInteractionAt so the entry is fresh on disk.
			cs.SetSelectedAgent(cs.SelectedAgent()) // no-op write trigger via the locked path
		}
	}

	// Best-effort: flush registry stores.
	if csFile != nil {
		// Upsert each ChatSession so the file reflects current state.
		for _, cs := range mgr.List() {
			_ = csFile.Upsert(cs.Entry())
		}
	}

	return firstErr
}