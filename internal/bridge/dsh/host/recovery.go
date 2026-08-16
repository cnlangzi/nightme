// recovery.go — restart recovery for the shared dsh host.
//
// The shared host runs for the daemon's lifetime — but the daemon
// itself may restart (config reload, host upgrade, crash), and
// every nightme-managed dsh session has a sessionId that must
// survive the restart. dsh's `session.create` accepts a
// pre-allocated sessionId: same id + same cwd returns the existing
// session (dsh-shared-host.md §2.6, dsh-api.md §2.1.3). We use
// that primitive to re-attach every persisted dsh session at boot.
//
// Two entry points:
//
//   RecoverSession(ctx, sessionID, cwd) error
//     Re-attach one session. Idempotent — calling twice with the
//     same id+cwd is a no-op the second time (server returns the
//     existing session). Used by AgentSession.FromPersisted when a
//     restored chat session is materialized.
//
//   RecoverAll(ctx, known func() []PersistedSession) (RecoverResult, error)
//     Walk all persisted sessionIds from the registry, re-attach
//     each. Used by runtime.runDaemon after the shared host is up.
//
// Failure modes:
//   - session-conflict (id + cwd mismatch) → log + treat as fresh
//     (next session.prompt starts a new dsh session; the user's
//     old history is preserved in dsh's log, but our bridge no
//     longer references it — they pay one new session, no
//     permanent history loss).
//   - transport error → log + skip; recoverable on next boot
//   - session-not-found → log + skip; id has been reaped server-side
package host

import (
	"context"
	"fmt"
	"log/slog"
)

// PersistedSession is the minimal projection of a registry
// AgentSessionEntry needed for recovery. The runtime layer maps
// registry entries into this shape before passing them in.
type PersistedSession struct {
	SessionID string // the dsh sessionId from the previous session
	CWD       string // must match what was used to create the session
	// BridgeName discriminates non-dsh bridges. Recovery only
	// processes entries where BridgeName == "dsh" — others are
	// ignored (claudecode / opencode / pi have their own resume
	// protocols that live in their respective bridges).
	BridgeName string
	// Label is a free-form human-readable tag used in log lines
	// (typically "<chatID>/<asID>"). Never required; nil-safe.
	Label string
}

// RecoverResult summarizes the outcome of RecoverAll. The runtime
// uses this to surface metrics and to skip already-recovered
// sessions on subsequent retries.
type RecoverResult struct {
	Reattached int      // successfully re-attached
	Orphaned   []string // server rejected (session-conflict, etc.) — kept as labels
	Skipped    int      // non-dsh bridges or empty sessionId (no recovery needed)
}

// RecoverSession re-attaches a single session by calling
// session.create with the pre-allocated sessionId + cwd. On success
// the session is "attached" again on the shared host (its events
// will appear on the mux stream); the caller is responsible for
// Router.Subscribe to actually receive frames.
//
// Idempotent: server returns the existing session when given a
// matching id+cwd, so calling twice is a no-op.
//
// Returns:
//   - nil on success (idempotent second call also returns nil)
//   - errSessionConflict when cwd doesn't match → caller treats
//     as fresh (history is lost for this client, but dsh's own
//     log still has it)
//   - other errors for transport / decode failures
func (c *RPCClient) RecoverSession(ctx context.Context, sessionID, cwd string) error {
	if sessionID == "" {
		return fmt.Errorf("dsh.host: recover: empty sessionId")
	}
	if cwd == "" {
		return fmt.Errorf("dsh.host: recover: empty cwd for sessionId=%s", sessionID)
	}
	opts := SessionCreateOpts{
		CWD:       cwd,
		SessionID: sessionID,
	}
	// server's behavior when sessionId is set with matching cwd:
	// returns the same id (re-attach). With mismatched cwd:
	// returns session-conflict.
	got, err := c.SessionCreate(ctx, opts)
	if err != nil {
		return fmt.Errorf("dsh.host: recover %s: %w", sessionID, err)
	}
	if got != sessionID {
		return fmt.Errorf("dsh.host: recover %s: server returned new id %s (cwd mismatch?)",
			sessionID, got)
	}
	return nil
}

// RecoverAll walks the persisted sessions list and re-attaches each
// dsh entry. Non-dsh entries are counted as Skipped. Re-attachment
// failures are logged at Warn level and returned in RecoverResult
// (RecoverAll does not short-circuit on individual failures — the
// boot process continues regardless so one bad sessionId doesn't
// block the rest of the daemon).
//
// known is a callback (rather than a slice) so the caller can pull
// from its own registry without forcing us to import the registry
// package. The function is called once at the start of RecoverAll.
//
// The slog logger is used for individual-recovery messages. nil
// falls back to slog.Default().
func (c *RPCClient) RecoverAll(
	ctx context.Context,
	known func() []PersistedSession,
	logger *slog.Logger,
) (RecoverResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var result RecoverResult
	sessions := known()
	if sessions == nil {
		return result, nil
	}
	for _, s := range sessions {
		if s.SessionID == "" {
			result.Skipped++
			continue
		}
		if s.BridgeName != "" && s.BridgeName != "dsh" {
			// Non-dsh bridges own their own resume protocol.
			result.Skipped++
			continue
		}
		if err := c.RecoverSession(ctx, s.SessionID, s.CWD); err != nil {
			logger.Warn("dsh.host: recover failed; treating session as fresh",
				"session_id", s.SessionID,
				"label", s.Label,
				"err", err.Error())
			result.Orphaned = append(result.Orphaned, s.Label)
			continue
		}
		result.Reattached++
		logger.Info("dsh.host: re-attached session",
			"session_id", s.SessionID,
			"label", s.Label)
	}
	return result, nil
}