// Package relay is the in-process capability surface for nightme's
// dsh drivers. One Relay per nightme process — drivers and tests
// fetch it via Get() and call its Bridge methods.
//
// What lives here vs the existing bridge/dsh packages:
//
//   - host.Client + host.StreamHub: the WS / RPC transport. The
//     relay takes ownership of the global Client and is its only
//     caller; drivers never reach host.Client directly anymore.
//   - backfill (was N goroutines in driver.runBackfillLoop, each
//     polling a non-existent session.history HTTP endpoint):
//     now one backfill per relay, fetching via the real
//     session/page endpoint, served from a per-session ring
//     buffer when possible. See backfill.go.
//   - ring buffer + per-session central state: every driver
//     Subscribe replays from the same buffer; one central mux
//     subscriber feeds it. See session_state.go.
//
// What stays in driver for now (per the design conversation):
// the per-session translator / wireState / dispatcher state
// machines. They're single-goroutine-accessed in driver, so
// moving them would force lock discipline changes for zero
// observable benefit at this layer.
//
// Lifecycle:
//
//   - Get() lazy-starts the shared dsh host on first call and
//     returns the singleton; subsequent calls return the same
//     instance.
//   - close() tears down the mux WS, cancels every subscriber,
//     and idles the relay. After close, Bridge methods return
//     errors and Get still returns the (closed) instance.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/dsh/api"
	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// Relay is the production api.Bridge implementation. See
// bridge.go for the contract; this file owns lifecycle and
// session map.
type Relay struct {
	// host is the current shared dsh host.Client. Stored in
	// atomic.Value so replaceClient (Phase 3 fix #4) can swap
	// without racing RPC reads.
	host atomic.Value // *host.Client

	// clientMu guards hostChanged only. mux pumps read r.host
	// via atomic.Value.Load() and re-bind on hostChanged.
	clientMu    sync.RWMutex
	hostChanged chan struct{}

	mu       sync.RWMutex
	sessions map[string]*sessionState

	log *slog.Logger

	closeOnce sync.Once
	closed    chan struct{}

	backfill *backfiller
}

var (
	relayOnce      sync.Once
	relayInstance  *Relay
	relayErr       error
	injectedBridge api.Bridge
)

func newRelay(cli *host.Client, log *slog.Logger) *Relay {
	if log == nil {
		log = slog.Default()
	}
	r := &Relay{
		hostChanged: make(chan struct{}, 16),
		sessions:    map[string]*sessionState{},
		log:         log,
		closed:      make(chan struct{}),
	}
	r.host.Store(cli)
	r.backfill = newBackfiller(r)
	// Phase 3 fix #4: register a ReplaceGlobal hook so the
	// relay re-binds its mux pump to the new client after a
	// dsh respawn. Without this, drivers would keep
	// dispatching frames from the dead client.
	host.RegisterReplaceGlobalHook(func(c *host.Client) {
		r.replaceClient(c)
	})
	return r
}

// SetForTest injects a custom Bridge implementation,
// bypassing the Get() singleton. Tests only — production
// never calls this. The pointer is captured; the caller must
// keep it alive for the duration of the test. UnsetForTest
// restores the singleton flow.
func SetForTest(br api.Bridge) {
	ResetForTest()
	injectedBridge = br
}

// UnsetForTest clears the injected bridge. Tests only.
func UnsetForTest() { injectedBridge = nil }

// ResetForTest tears down the singleton state so the next
// Get call lazy-starts fresh. Tests only — production never
// calls this. Pair with UnsetForTest to fully reset between
// test runs.
func ResetForTest() {
	relayOnce = sync.Once{}
	relayInstance = nil
	relayErr = nil
}

// Get returns the process-wide Relay singleton, lazily
// starting the shared dsh host on first call. Subsequent
// calls return the same instance. After close, Get still
// returns the (closed) instance — callers see errors on
// every Bridge method.
//
// Tests that inject a Bridge via SetForTest get that instance
// back, not a singleton — see internal/bridge/dsh/relay/test
// package (or SetForTest in this file) for the helper.
func Get(opts host.SharedHostOptions) (*Relay, error) {
	if injectedBridge != nil {
		if r, ok := injectedBridge.(*Relay); ok {
			return r, nil
		}
		// Non-Relay injected (test fake): the test fake is a
		// full api.Bridge but does NOT carry the *Relay-only
		// fields drivers reach for (handleMuxFrame, lastSeq,
		// etc.). Tests that need *Relay-specific behaviour
		// should keep using installGlobal + the real relay.
		return nil, fmt.Errorf("relay: SetForTest got non-Relay bridge; use GetBridge for fakes")
	}
	relayOnce.Do(func() {
		cli, err := host.EnsureSharedHost(context.Background(), opts)
		if err != nil {
			relayErr = err
			return
		}
		relayInstance = newRelay(cli, opts.Logger)
	})
	if relayErr != nil {
		return nil, relayErr
	}
	return relayInstance, nil
}

// GetBridge is the test-friendly form of Get that returns
// the api.Bridge interface directly. Tests inject a fake
// via SetForTest and call this to drive Bridge methods.
//
// Production callers should use Get (returns *Relay so they
// can reach MuxClient / etc. — see Phase 2 design).
func GetBridge(opts host.SharedHostOptions) (api.Bridge, error) {
	if injectedBridge != nil {
		return injectedBridge, nil
	}
	return Get(opts)
}

func (r *Relay) Close() error {
	var err error
	r.closeOnce.Do(func() {
		close(r.closed)
		r.backfill.stop()
		r.host.Load().(*host.Client).Close()
	})
	return err
}

// sessionStateFor is an internal accessor used by backfill and
// the future mux-dispatch migration. Returns nil if the session
// is unknown (caller decides how to react).
func (r *Relay) sessionStateFor(id string) *sessionState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[id]
}

// MuxClient exposes the underlying *host.Client so drivers
// can call Subscribe / Unsubscribe (the one transport-level
// hook left to them in Phase 1 — for protocol translation that
// stays per-driver). Phase 2 will move mux dispatch into the
// relay and remove this accessor; for now it keeps the
// driver-side refactor surface small.
func (r *Relay) MuxClient() *host.Client { return r.host.Load().(*host.Client) }

// Connect implements api.Bridge.
//
// Flow:
//  1. Workspace.create (idempotent on path).
//  2. Session.create with opts.SessionID. dsh treats (id, cwd)
//     as the idempotency key; same id+cwd returns the same
//     in-memory session and joins the mux live set.
//  3. register sessionState in the relay so Subscribe /
//     History / Send can find it.
//
// opts.SessionID == "" means "create fresh"; non-empty means
// "re-attach" (chatsession auto-reconnect path — see the chat
// session's persisted sessionId).
func (r *Relay) Connect(ctx context.Context, opts api.ConnectOpts) (api.SessionHandle, error) {
	select {
	case <-r.closed:
		return api.SessionHandle{}, fmt.Errorf("relay: closed")
	default:
	}

	repoRoot := detectRepoRoot(opts.Workspace)
	ws, err := r.host.Load().(*host.Client).RPC.WorkspaceCreate(ctx, repoRoot)
	if err != nil {
		return api.SessionHandle{}, fmt.Errorf("relay: workspace.create: %w", err)
	}

	sessionID := opts.SessionID
	if sessionID == "" {
		created, err := r.host.Load().(*host.Client).RPC.SessionCreate(ctx, host.SessionCreateOpts{
			WorkspaceID: ws.WorkspaceID,
			CWD:         opts.Workspace,
		})
		if err != nil {
			return api.SessionHandle{}, fmt.Errorf("relay: session.create: %w", err)
		}
		sessionID = created
	} else {
		// Re-attach path: dsh's session.create is idempotent on
		// (id, cwd); same id+cwd returns the same in-memory
		// session and joins the mux live set.
		got, err := r.host.Load().(*host.Client).RPC.SessionCreate(ctx, host.SessionCreateOpts{
			SessionID:   sessionID,
			WorkspaceID: ws.WorkspaceID,
			CWD:         opts.Workspace,
		})
		if err != nil {
			return api.SessionHandle{}, fmt.Errorf("relay: session.create re-attach: %w", err)
		}
		if got != sessionID {
			return api.SessionHandle{}, fmt.Errorf("relay: re-attach returned %q, want %q", got, sessionID)
		}
	}

	s := newSessionState(sessionID, opts.Workspace)
	r.mu.Lock()
	r.sessions[sessionID] = s
	r.mu.Unlock()

	if opts.PermissionMode != "" {
		// dsh exposes permission mode via /commands/execute.
		// Best-effort: a failure here is logged at warn but not
		// surfaced to the caller — the session is still usable
		// in default mode and a follow-up /permission can fix it.
		if err := r.host.Load().(*host.Client).RPC.CommandsExecute(ctx, sessionID, "/permission "+opts.PermissionMode); err != nil {
			r.log.Warn("relay: set permission mode failed",
				"session_id", sessionID,
				"mode", opts.PermissionMode,
				"err", err)
		}
	}

	return api.SessionHandle{
		ID:        sessionID,
		Workspace: opts.Workspace,
		CWD:       opts.CWD,
	}, nil
}

// Disconnect implements api.Bridge.
func (r *Relay) Disconnect(ctx context.Context, id string) error {
	r.mu.Lock()
	s, ok := r.sessions[id]
	if ok {
		delete(r.sessions, id)
	}
	r.mu.Unlock()

	if s != nil {
		s.closeSubscribers()
	}

	// Phase 3 fix #5: surface the joined error to the caller
	// (driver.Close) instead of logging and returning nil.
	// A non-fatal cancel / archive failure (e.g. session
	// already gone because dsh restarted) is still surfaced
	// but the caller can ignore it.
	var errs []error
	if err := r.host.Load().(*host.Client).RPC.SessionCancel(ctx, id); err != nil {
		r.log.Warn("relay: session.cancel failed",
			"session_id", id, "err", err)
		errs = append(errs, fmt.Errorf("session.cancel: %w", err))
	}
	if err := r.host.Load().(*host.Client).RPC.WorkspaceArchiveSession(ctx, id); err != nil {
		r.log.Warn("relay: workspace.archiveSession failed",
			"session_id", id, "err", err)
		errs = append(errs, fmt.Errorf("workspace.archiveSession: %w", err))
	}
	return errors.Join(errs...)
}

// Send implements api.Bridge.
func (r *Relay) Send(ctx context.Context, id string, blocks []agent.ContentBlock) error {
	if r.sessionStateFor(id) == nil {
		return fmt.Errorf("relay: unknown session %s", id)
	}
	parts, err := blocksToPromptParts(blocks)
	if err != nil {
		return fmt.Errorf("relay: encode prompt: %w", err)
	}
	if err := r.host.Load().(*host.Client).RPC.SessionPrompt(ctx, id, "queue", parts); err != nil {
		return fmt.Errorf("relay: session.prompt: %w", err)
	}
	return nil
}

// Respond implements api.Bridge.
//
// Phase 1: thin shim. The per-driver pending* maps (in
// session.go) remain authoritative — see bridge.go header
// for the plan to move FIFO routing into the relay.
func (r *Relay) Respond(ctx context.Context, id, response string) error {
	s := r.sessionStateFor(id)
	if s == nil {
		return fmt.Errorf("relay: unknown session %s", id)
	}
	return s.respond(ctx, response)
}

// Subscribe implements api.Bridge.
//
// First subscriber opens the underlying mux subscription
// (via MuxSubscribe under the hood); later subscribers share
// the same mux path. Phase 3 (focused) returns the wire-shape
// Event — drivers translate through their existing
// translator / wireState / dispatcher.
//
// The channel closes when ctx fires, the relay shuts down, or
// Disconnect is called. Unsubscribe is idempotent.
func (r *Relay) Subscribe(ctx context.Context, id string) (<-chan api.Event, func(), error) {
	s := r.sessionStateFor(id)
	if s == nil {
		return nil, nil, fmt.Errorf("relay: unknown session %s", id)
	}
	return s.subscribe(ctx)
}

// History implements api.Bridge. See backfill.go for the full
// read path (ring buffer → session/page fallback).
//
// Phase 3 (focused) returns wire-shape Event; translation
// is still per-driver.
func (r *Relay) History(ctx context.Context, id string, sinceSeq int64) ([]api.Event, error) {
	s := r.sessionStateFor(id)
	if s == nil {
		return nil, fmt.Errorf("relay: unknown session %s", id)
	}
	return r.backfill.fetch(ctx, s, sinceSeq)
}

// MuxSubscribe implements api.Bridge.
//
// Phase 2 (focused): the relay owns the per-session mux
// subscription. The first MuxSubscribe call opens the
// underlying host subscription; later calls just append
// to the per-session handler list. The single central
// mux pump fans every frame out to every registered
// handler, so multiple drivers attached to the same
// session (today's reality: per-session there's exactly
// one driver, but the architecture supports many) all
// see the same frames.
//
// The returned detach func removes the handler from the
// list and, when no handlers remain, cancels the central
// mux pump. Idempotent; safe to call multiple times.
func (r *Relay) MuxSubscribe(ctx context.Context, id, workspace string, handler api.MuxHandler) (func(), error) {
	s := r.sessionStateFor(id)
	if s == nil {
		return nil, fmt.Errorf("relay: unknown session %s", id)
	}
	if handler == nil {
		return nil, fmt.Errorf("relay: nil mux handler for %s", id)
	}

	// Phase 3 fix #6: addMuxHandler returns an ID + detach
	// that actually drops the entry. Old code never deleted.
	_, detach := s.addMuxHandler(handler)

	// Start the central mux pump on first subscriber.
	s.muxMu.Lock()
	first := s.muxCancel == nil
	if first {
		muxCtx, cancel := context.WithCancel(context.Background())
		s.muxCancel = cancel
		go r.runMuxPump(muxCtx, s, workspace)
	}
	s.muxMu.Unlock()

	return detach, nil
}

// runMuxPump is the central mux subscriber for one session.
// It opens a host.Client.Subscribe once, and for every
// incoming frame calls every registered handler. The
// pump exits when ctx is cancelled (Disconnect / relay
// close).
//
// Phase 3 fix #4: the pump re-binds its mux subscription
// when replaceClient swaps the host (dsh respawn path). The
// loop watches r.host via the relay's atomic field and
// re-subscribes when it changes.
func (r *Relay) runMuxPump(ctx context.Context, s *sessionState, workspace string) {
	defer func() {
		r.host.Load().(*host.Client).Unsubscribe(s.id)
	}()
	for {
		cli := r.host.Load().(*host.Client)
		done := make(chan struct{})
		cli.Subscribe(s.id, workspace, func(method, rpcID string, payload api.MuxHandlerPayload) {
			s.dispatchMux(method, rpcID, payload)
		})
		select {
		case <-ctx.Done():
			// Close the host subscription then exit.
			r.host.Load().(*host.Client).Unsubscribe(s.id)
			close(done)
			return
		case <-r.hostChanged:
			// Host client swapped. Unsubscribe from old, loop
			// re-binds to new client on next iteration.
			cli.Unsubscribe(s.id)
			close(done)
			// Fall through; the for-loop iteration picks up
			// the new r.host value.
		}
	}
}

// replaceClient is the ReplaceGlobal hook callback (Phase 3
// fix #4). Atomically swaps r.host and signals all active mux
// pumps to re-bind. Existing subscribers' frames in-flight on
// the old client are dropped (driver-side `select-default`
// protects against send-on-closed-channel).
func (r *Relay) replaceClient(c *host.Client) {
	r.host.Store(c)
	select {
	case r.hostChanged <- struct{}{}:
	default:
	}
}

// Health implements api.Bridge.
//
// Phase 1: single process-level probe via host.RPC.SessionList.
// Replaces the N per-driver /api/session.list calls that fired
// every 30s × 3 strikes = 90s on every session.
func (r *Relay) Health(ctx context.Context) error {
	if _, err := r.host.Load().(*host.Client).RPC.SessionList(ctx); err != nil {
		return fmt.Errorf("relay: health: %w", err)
	}
	return nil
}
