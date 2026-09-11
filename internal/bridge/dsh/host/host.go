// host.go — facade wiring RPC + StreamHub + Router into one Client.
//
// Phase 0 entry point for the 1:N architecture. One Client per
// nightme daemon; the daemon calls Start once at boot, dials
// /api/events.mux + /api/events.host, and exposes RPC + subscription
// routing for every ChatSession / AgentSession to share.
//
// Lifecycle:
//   - New(baseURL, log): construct (cheap, no I/O)
//   - Start(ctx): kick off mux+host pumps (one goroutine each +
//     reconnect-watchdog built into the pumps)
//   - Close: graceful shutdown — wait for pumps to drain
//
// What Phase 0 does NOT include (deferred to later phases):
//   - Phase 1: spawn the actual `dsh --profile web` subprocess
//   - Phase 2: ChatSession.Spawner hooks (subscribe on create,
//     unsubscribe on drop, wire pending-approval answers through
//     RPCClient.Respond)
//   - Phase 3: remove the per-driver code paths in the parent dsh
//     package
//   - Phase 4: restart-recovery via SessionList

package host

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
)

// globalMu guards globalClient. Set once at nightme daemon boot
// (cmd/nightme/main.go) via SetGlobal; every ChatSession / AgentSession
// reads it via GetGlobal when constructing a new bridge.Session.
//
// Mutex rather than atomic.Pointer because we also want the
// "not initialized" error to be cheap (a nil-check under RLock is
// cheaper than an atomic load + nil-check on every bridge Start).
var (
	globalMu     sync.RWMutex
	globalClient *Client
)

// SetGlobal installs c as the process-wide shared dsh client. Must be
// called once during daemon boot BEFORE any ChatSession / AgentSession
// that needs dsh is created. Calling twice panics — the second caller
// almost certainly has a bug.
func SetGlobal(c *Client) {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalClient != nil {
		panic("dsh.host: SetGlobal called twice; the shared client is a singleton")
	}
	globalClient = c
}

// ReplaceGlobal swaps the process-wide shared dsh client. Unlike
// SetGlobal, it does NOT panic on prior install — it's used by the
// watchdog after a dsh respawn when the new dsh is up at a (possibly
// different) URL and every existing subscriber must start using
// the new RPC client. The caller is responsible for closing the
// previous Client (typically after swapping, so any in-flight RPC
// gets a clean error rather than a hung transport).
func ReplaceGlobal(c *Client) {
	globalMu.Lock()
	globalClient = c
	globalMu.Unlock()
}

// GetGlobal returns the shared Client installed by SetGlobal, or nil
// if SetGlobal hasn't run yet. Callers should treat nil as a fatal
// startup-ordering bug (the daemon should have initialized the shared
// client before any bridge starts).
func GetGlobal() *Client {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalClient
}

// UnsetGlobal clears the global. Used by tests that install a fresh
// client per case. Not intended for production paths.
func UnsetGlobal() {
	globalMu.Lock()
	globalClient = nil
	globalMu.Unlock()
}

// Client is the single per-nightme-daemon facade for the shared dsh
// web host. Every ChatSession / AgentSession interacts with the host
// through this Client (via its three exported fields).
type Client struct {
	baseURL string
	log     *slog.Logger

	// RPC is the shared HTTP RPC transport. POSTs /api/{method}
	// for every business call. Safe for concurrent use across all
	// sessions.
	RPC *RPCClient

	// Hub is the WS pump pair (mux + host). Owns the single
	// connection per stream; auto-reconnects on loss.
	Hub *StreamHub

	// Router holds per-session mux subscriptions + the shared
	// pending-approvals/questions table. ChatSession/AgentSession
	// register/unregister themselves here.
	Router *Router

	closeOnce sync.Once
	closed    chan struct{}
}

// New constructs the Client but does NOT start any I/O. Call Start
// to bring the WS pumps online.
//
// baseURL is the dsh web server root (e.g. "http://127.0.0.1:3080").
// Trailing slashes are tolerated (RPCClient strips them). An empty
// baseURL is an error surfaced at Start time (Start needs a parseable
// URL even if we don't dial immediately).
//
// New builds an unauthenticated client (no cookie jar). It is the
// right entry point for tests with httptest-backed fake dsh servers
// that don't auth-gate. Production code paths must use NewWithJar
// — dsh 0.1.2-rc.1 auth-gates /api/* and /api/events.*, and an
// unauthenticated client gets 401 on every call.
func New(baseURL string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	c := &Client{
		baseURL: baseURL,
		log:     log,
		closed:  make(chan struct{}),
	}
	c.Router = NewRouter(log)
	c.RPC = NewRPCClient(baseURL)
	// Wire Hub callbacks to Router. DispatchMux extracts sessionId
	// from the payload itself — the Hub is payload-agnostic.
	c.Hub = NewStreamHub(baseURL, log,
		c.Router.DispatchMux,
		c.Router.DispatchHost,
	)
	return c
}

// NewWithJar constructs the Client with a cookie jar attached to
// both the HTTP RPC client and the WebSocket dialer. spawnAndWire
// uses this after minting the dsh-auth cookie via the
// `GET /?token=<launchToken>` exchange (dsh 0.1.2-rc.1 returns a
// 303 with `set-cookie: dsh-auth-<hash>=<signed-payload>` which
// we stash in jar before constructing the client).
//
// jar must be non-nil; passing nil is a programming error — use
// New instead.
func NewWithJar(baseURL string, jar http.CookieJar, log *slog.Logger) *Client {
	if jar == nil {
		// Defensive — callers that hand us a nil jar are almost
		// certainly confused about which constructor to use.
		// Falling back to New keeps the bridge from panic'ing
		// but the first /api call will 401 and surface the bug.
		// Log loudly so the misconfiguration surfaces in startup
		// logs rather than hours later when the bridge first
		// tries to talk to dsh.
		l := log
		if l == nil {
			l = slog.Default()
		}
		l.Error("dsh.host: NewWithJar called with nil jar; falling back to unauthenticated New — every /api/* will 401")
		return New(baseURL, log)
	}
	if log == nil {
		log = slog.Default()
	}
	c := &Client{
		baseURL: baseURL,
		log:     log,
		closed:  make(chan struct{}),
	}
	c.Router = NewRouter(log)
	c.RPC = NewRPCClientWithHTTP(baseURL, &http.Client{
		Jar:     jar,
		Timeout: httpClientTimeout,
	})
	c.Hub = NewStreamHubWithJar(baseURL, jar, log,
		c.Router.DispatchMux,
		c.Router.DispatchHost,
	)
	return c
}

// BaseURL returns the URL the Client was constructed against.
// Exposed for tests + observability.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// Start kicks off the mux + host pumps. Returns an error if the
// baseURL is malformed or empty (StreamHub.Start validates).
//
// Start does NOT itself verify the dsh server is reachable — the
// first dial attempt happens inside Hub.runPump and the reconnect
// loop will keep trying if the server is down at boot. This matches
// dsh/web's expected startup order: spawn dsh → read URL → bring
// up WS. If the user starts dsh late or restarts it, the
// reconnect loop eventually catches up.
func (c *Client) Start(ctx context.Context) error {
	return c.Hub.Start(ctx)
}

// Close stops the mux + host pumps and waits for them to drain.
// Idempotent (subsequent calls are no-ops). Safe to call from any
// goroutine.
//
// Close does NOT close the underlying dsh process — that's the
// caller's responsibility (Phase 1 adds the spawn wrapper, which
// owns the dsh subprocess lifecycle).
//
// Done() fires AFTER Hub.Close returns — consumers that wait on
// Done() can rely on the mux pump being fully drained (Hub.Close
// blocks on dispatchDrain).
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.Hub.Close()
		close(c.closed)
	})
}

// Done returns a channel that's closed when Close completes (or has
// already completed). Useful for tests waiting on shutdown.
func (c *Client) Done() <-chan struct{} {
	return c.closed
}

// RecoverSubscriptions walks every active Router subscription and
// re-attaches it on the current dsh via RPCClient.SessionCreate
// (the sessionId+cwd is the documented re-attach key in
// dsh-api.md §2.1.3; the legacy RecoverSession helper has been
// retired in favour of plain SessionCreate). Used by the
// watchdog after a dsh respawn so surviving ChatSessions keep
// receiving mux frames from the new dsh instance.
//
// Per-session failures are logged at Warn level so operators see
// "5 sessions were dropped on respawn" without having to inspect
// the code. The orphan list is still returned in the result
// struct for programmatic consumers (and `/diagnose` display).
//
// Safe for concurrent use: the subscription enumeration is a
// snapshot, and SessionCreate is its own RPC round-trip.
//
// Returns RecoverResult summarizing how many subscriptions were
// re-attached vs. orphaned (server-side session-conflict or
// transport error). One bad sessionId does not abort the loop —
// the watcher logs and continues.
func (c *Client) RecoverSubscriptions(ctx context.Context, logger *slog.Logger) RecoverResult {
	if logger == nil {
		logger = slog.Default()
	}
	var result RecoverResult
	for _, sub := range c.Router.EnumerateSubscriptions() {
		if sub.SessionID == "" {
			result.Orphaned = append(result.Orphaned, sub.SessionID)
			continue
		}
		opts := SessionCreateOpts{
			CWD:       sub.CWD,
			SessionID: sub.SessionID,
		}
		got, err := c.RPC.SessionCreate(ctx, opts)
		if err != nil {
			logger.Warn("dsh.host: reattach orphaned after respawn",
				"session_id", sub.SessionID,
				"cwd", sub.CWD,
				"err", err.Error())
			result.Orphaned = append(result.Orphaned, sub.SessionID)
			continue
		}
		if got != sub.SessionID {
			logger.Warn("dsh.host: reattach returned new id (cwd mismatch?)",
				"session_id", sub.SessionID,
				"got", got,
				"cwd", sub.CWD)
			result.Orphaned = append(result.Orphaned, sub.SessionID)
			continue
		}
		logger.Info("dsh.host: reattached subscription",
			"session_id", sub.SessionID)
		result.Reattached++
	}
	return result
}

// RecoverResult summarizes the outcome of Client.RecoverSubscriptions.
// The runtime uses this to surface metrics after a watchdog respawn.
type RecoverResult struct {
	Reattached int      // successfully re-attached sessions
	Orphaned   []string // server rejected (session-conflict, transport) — sessionIds
}

// ─── convenience methods (thin wrappers around Router) ────────────
//
// These exist so callers don't have to reach into the Router field
// for the common cases. They're 1-line wrappers — the doc comments
// live on Router.

// Subscribe installs a per-session mux handler AND opens a
// `session/follow` stream on the dsh mux connection so the
// handler actually receives events. Returns a cancel function
// that drops both the handler and the stream.
//
// cwd is captured for respawn recovery (see Router.Subscribe).
func (c *Client) Subscribe(sessionID, cwd string, h MuxFrameHandler) (unsubscribe func()) {
	c.Router.Subscribe(sessionID, cwd, h)
	// StreamHub.Subscribe handles "no Hub yet" gracefully (it's a
	// no-op when Hub is nil) — callers can call Subscribe before
	// Start without panicking. The stream actually opens when
	// Start runs and the mux pump comes up; re-issue Subscribe
	// from inside Client.RecoverSubscriptions if you need a
	// guaranteed post-respawn re-subscribe.
	unsubscribeFromHub := c.Hub.Subscribe(sessionID, func(method, rpcID string, payload json.RawMessage) {
		c.Router.DispatchMux(method, rpcID, payload)
	})
	return func() {
		if unsubscribeFromHub != nil {
			unsubscribeFromHub()
		}
		c.Router.Unsubscribe(sessionID)
	}
}

// Unsubscribe removes the sessionId's mux handler, drops its pending
// approval/question channels, AND cancels the StreamHub session
// stream on dsh so the server stops pushing items for it. See
// Router.Unsubscribe / Hub.Subscribe for the per-side contract.
//
// Use this (not Router.Unsubscribe directly) on every Close path —
// skipping the Hub teardown leaks a stale session/follow stream on
// dsh and a stale *sessionStream entry in StreamHub.sessions.
func (c *Client) Unsubscribe(sessionID string) {
	if sessionID == "" {
		return
	}
	if c.Hub != nil {
		c.Hub.Unsubscribe(sessionID)
	}
	c.Router.Unsubscribe(sessionID)
}

// SetHostHandler installs the global host-stream handler. See
// Router.SetHostHandler.
func (c *Client) SetHostHandler(h HostFrameHandler) {
	c.Router.SetHostHandler(h)
}

// RegisterPendingApproval stores a response channel under
// (sessionID, frameRpcID). See Router.RegisterPendingApproval.
func (c *Client) RegisterPendingApproval(sessionID, frameRpcID string) chan string {
	return c.Router.RegisterPendingApproval(sessionID, frameRpcID)
}

// AnswerPending writes outcome to the channel registered under
// (sessionID, frameRpcID). See Router.AnswerPending.
func (c *Client) AnswerPending(sessionID, frameRpcID, outcome string) bool {
	return c.Router.AnswerPending(sessionID, frameRpcID, outcome)
}
