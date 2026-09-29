// Package feishu — WS *Client rebuild path.
//
// The larksuite/oapi-sdk-go v3 ws.Client sets an internal `terminal`
// flag to true under three conditions (ws/client_lifecycle.go:125-128):
//
//   - runStopByContext — our cancel of the run context (the prober's
//     ReconnectSDK does this on every tick; see recordLastStartErr
//     below for the impact).
//   - runStopByClose — Close() called.
//   - runStopByFailure with run.everConnected — a non-retryable
//     *ws.ClientError from the server AFTER a successful connect.
//     Note: an initial-connect *ClientError (server rejected the
//     very first dial, run.everConnected=false) does NOT flip
//     terminal; the SDK's runCoordinator returns and Start returns
//     the *ClientError but the *Client itself is reusable. The
//     rebuild path doesn't run in that case (no OnDisconnected
//     callback fires before the failure).
//
// The terminal flag freezes the *Client — every subsequent
// client.Start returns errClientTerminal ("websocket client cannot
// be restarted", ws/error.go:12) and the SDK has no reset / rearm
// API. A brand-new *larkws.Client must be constructed.
//
// Two terminal signatures the rebuild path must detect:
//
//   - *ws.ClientError — real non-retryable server-side failure.
//     Canonical trigger: 514 + Handshake-Autherrcode=1000040350
//     (ExceedConnLimit) after a macOS wake, when the server still
//     sees the old pre-sleep device_id as alive.
//   - errClientTerminal — the sentinel returned by Start on an
//     already-terminal client (the SDK's `errors.New(...)` is
//     unexported, so we string-match its message). Fires on every
//     post-terminal Start attempt. We MUST detect this too —
//     otherwise a *ClientError → cancel → next Start → errClientTerminal
//     cycle leaves the rebuild loop spinning forever, because
//     lastStartErr is non-terminal (errClientTerminal) and the
//     gate currently requires terminal to rebuild.
//
// This file adds the rebuild escape hatch: ReconnectSDK observes the
// previous run's terminal-class state, constructs a fresh
// *larkws.Client (with widened HandshakeTimeout / httpClient.Timeout
// so the first dial after a wake has headroom), then spawns the new
// run. Cooldown + max consecutive failures bound the rebuild rate: a
// misconfigured credential that returns *ClientError every dial must
// not spin the rebuild at the prober's 30s cadence forever.
package feishu

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

const (
	// sdkHandshakeTimeout widens gorilla/websocket's default 45s
	// HandshakeTimeout so the first WS dial after a macOS wake —
	// when the network stack and TLS session resume lag — still has
	// headroom. The SDK's ws.Client option WithWebSocketDialer is
	// the only knob that drives this.
	sdkHandshakeTimeout = 90 * time.Second

	// sdkHTTPTimeout widens the SDK's default 10s http.Client.Timeout
	// on the bootstrap endpoint fetch (POST /callback/ws/endpoint).
	// The first HTTPS call after a wake routinely exceeds 10s due to
	// TCP slow-start + TLS handshake on a freshly reconnected
	// interface. Without this, every post-wake dial is one strike
	// closer to a *ClientError from the server side.
	sdkHTTPTimeout = 30 * time.Second

	// sdkRebuildCooldown bounds how often rebuildWSClient may fire.
	// The very first terminal detection gets one free rebuild; after
	// that we wait at least this long before rebuilding again. Stops
	// a hot loop on a config error that returns *ClientError every
	// dial (and would otherwise burn at the prober's 30s cadence).
	sdkRebuildCooldown = 60 * time.Second

	// sdkMaxConsecutiveRebuilds stops the rebuild loop after N
	// straight rebuild-then-still-terminal cycles. Past this the
	// error is a config / credentials bug, not a transient failure,
	// and the prober stops rebuilding — the persistent last_error is
	// already surfaced via `nightme health` for the operator to act
	// on.
	sdkMaxConsecutiveRebuilds = 5

	// sdkErrClientTerminalMessage is the exact string the SDK's
	// ws/error.go:12 errClientTerminal sentinel uses. The sentinel
	// is unexported (we can't errors.Is against it directly); the
	// message is part of the SDK's public surface and pinned by
	// TestIsStrandedTerminal_DetectsSDKMessage. Any SDK version
	// that changes this string will surface as a unit test failure.
	sdkErrClientTerminalMessage = "websocket client cannot be restarted"
)

// RebuildSnapshot is the rebuild-side mirror of WSHealthSnapshot's
// Prober field. Operators running `nightme health` can see whether
// the rebuild path has fired, and how many times, without digging
// through the log file. JSON tags match the cross-process wire format.
type RebuildSnapshot struct {
	RebuildCount          int64     `json:"rebuild_count"`
	LastRebuildAt         time.Time `json:"last_rebuild_at"`
	ConsecutiveFailures   int64     `json:"consecutive_failures"`
	SkippedCooldown       int64     `json:"skipped_cooldown"`
	SkippedMaxConsecutive int64     `json:"skipped_max_consecutive"`
	LastTerminalErr       string    `json:"last_terminal_err"`
	LastTerminalErrAt     time.Time `json:"last_terminal_err_at"`
	CancelStrandedStreak  int64     `json:"cancel_stranded_streak"`
}

// rebuildState is the per-Adapter atomic state used by the rebuild
// path. Pointer-on-Adapter so the snapshot can be read under no locks.
type rebuildState struct {
	count             atomic.Int64
	lastAt            atomic.Pointer[time.Time]
	consecutiveFails  atomic.Int64
	skippedCooldown   atomic.Int64
	skippedMaxStreak  atomic.Int64
	lastTerminalErr   atomic.Pointer[string]
	lastTerminalErrAt atomic.Pointer[time.Time]

	// cancelStrandedTerminal is true when the most recent Start()
	// returned the SDK's errClientTerminal sentinel — i.e. we (or
	// the SDK) already put the *Client into terminal state and any
	// further Start on it will keep returning the same sentinel.
	// maybeRebuildClient gates on this flag in addition to the
	// *ClientError detector, because a prober tick that cancels a
	// still-blocked Start never records a *ClientError on its own
	// (the cancel returns context.Canceled which recordLastStartErr
	// filters) — the next Start's errClientTerminal is the only
	// signal we get. Reset by markSDKConnected (successful connect
	// proves the new client is healthy) and by rebuildWSClient
	// itself (the rebuild succeeded regardless of which terminal
	// path triggered it).
	cancelStrandedTerminal atomic.Bool

	// strandedStreak counts how many consecutive errClientTerminal
	// events we've recorded since the last successful rebuild. The
	// main consecutiveFails counter only bumps on *ClientError
	// (real server-side terminal); strandedStreak tracks the cancel-
	// induced terminal path separately so `nightme health` can
	// distinguish them.
	strandedStreak atomic.Int64
}

func newRebuildState() *rebuildState { return &rebuildState{} }

func (s *rebuildState) snapshot() RebuildSnapshot {
	out := RebuildSnapshot{
		RebuildCount:          s.count.Load(),
		ConsecutiveFailures:   s.consecutiveFails.Load(),
		SkippedCooldown:       s.skippedCooldown.Load(),
		SkippedMaxConsecutive: s.skippedMaxStreak.Load(),
		CancelStrandedStreak:  s.strandedStreak.Load(),
	}
	if t := s.lastAt.Load(); t != nil {
		out.LastRebuildAt = *t
	}
	if t := s.lastTerminalErrAt.Load(); t != nil {
		out.LastTerminalErrAt = *t
	}
	if e := s.lastTerminalErr.Load(); e != nil {
		out.LastTerminalErr = *e
	}
	return out
}

// isClientError reports whether err is a *ws.ClientError — the SDK's
// marker for non-retryable handshake / endpoint errors returned from
// the server (e.g., 514 + ExceedConnLimit, 403, endpoint bootstrap
// errors). Each match bumps consecutiveFails in recordLastStartErr;
// the counter feeds the max-streak guard so a misconfigured credential
// doesn't loop forever.
func isClientError(err error) bool {
	if err == nil {
		return false
	}
	var ce *larkws.ClientError
	return errors.As(err, &ce)
}

// isStrandedTerminal reports whether err is the SDK's
// errClientTerminal sentinel ("websocket client cannot be restarted"
// from ws/error.go:12). The sentinel is unexported, so we can't
// errors.Is against it — we match by exact-message. Detection
// triggers the rebuild path (set cancelStrandedTerminal=true in
// recordLastStartErr) but does NOT bump consecutiveFails; that
// counter is for *ClientError only, and an errClientTerminal fires
// on every post-terminal Start attempt (prober ticks every 30s) —
// bumping it would prematurely trip the max-streak limit.
func isStrandedTerminal(err error) bool {
	if err == nil {
		return false
	}
	return err.Error() == sdkErrClientTerminalMessage
}

// isTerminalSDKError is the union of isClientError and isStrandedTerminal,
// preserved for callers that only need to know "is this in a terminal
// class?". The rebuild path's gate uses isClientError specifically
// (because the consecutiveFails counter is *ClientError-only), and
// checks the stranded flag separately.
func isTerminalSDKError(err error) bool {
	return isClientError(err) || isStrandedTerminal(err)
}

// recordLastStartErr is called from the goroutine that wraps
// client.Start in Start() and ReconnectSDK(). err is the value
// returned by client.Start — the run's final lifecycle error. We
// filter nil + context.Canceled (transient cancels happen on every
// normal Stop / prober-initiated cancel) and classify the rest:
//
//   - *ws.ClientError (isClientError): real terminal. Stamp
//     lastTerminalErr + bump consecutiveFails (which feeds the
//     max-streak guard).
//   - errClientTerminal (isStrandedTerminal): stranded terminal —
//     the *Client was already in terminal state when Start was
//     called. Set cancelStrandedTerminal so maybeRebuildClient
//     knows to rebuild; bump strandedStreak for diagnostics. Do
//     NOT touch consecutiveFails (see comment on isStrandedTerminal).
//   - Anything else: store lastStartErr for diagnostics, do NOT
//     touch the rebuild counters. We don't reset consecutiveFails
//     here — only *ClientError bumps it, and a stray reset would
//     let the rebuild loop escape its budget on the cancel-induced
//     terminal path.
func (a *Adapter) recordLastStartErr(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	cp := err
	a.lastStartErr.Store(&cp)

	if isClientError(err) {
		now := time.Now()
		msg := err.Error()
		a.rebuild.lastTerminalErr.Store(&msg)
		a.rebuild.lastTerminalErrAt.Store(&now)
		a.rebuild.consecutiveFails.Add(1)
		return
	}
	if isStrandedTerminal(err) {
		a.rebuild.cancelStrandedTerminal.Store(true)
		a.rebuild.strandedStreak.Add(1)
		return
	}
}

// maybeRebuildClient runs once per ReconnectSDK tick. Returns without
// rebuilding when:
//
//   - lastStartErr is nil AND cancelStrandedTerminal is false
//     (the SDK is in its normal retry loop and we shouldn't interfere),
//   - OR lastStartErr is non-nil but not a *ClientError AND
//     cancelStrandedTerminal is false (network blip, retryable),
//   - OR the consecutive-fail streak hit sdkMaxConsecutiveRebuilds
//     (config error, give up),
//   - OR the rebuild cooldown hasn't elapsed since the last rebuild.
//
// The gate fires (rebuild proceeds) when:
//
//   - lastStartErr is a *ws.ClientError (real server-side terminal),
//   - OR cancelStrandedTerminal is true (the SDK is stuck on a
//     terminal *Client; we must rebuild to escape).
//
// Successful rebuild clears cancelStrandedTerminal (the new client
// is fresh and may or may not be terminal yet — that's its own
// state to track).
func (a *Adapter) maybeRebuildClient() {
	lastPtr := a.lastStartErr.Load()
	stranded := a.rebuild.cancelStrandedTerminal.Load()

	if lastPtr == nil && !stranded {
		return
	}

	terminal := stranded
	if lastPtr != nil {
		// isTerminalClientError gates also on *ClientError (real
		// server-side terminal). We deliberately do NOT include
		// isStrandedTerminal here — the cancelStrandedTerminal
		// flag carries that signal without bumping consecutiveFails.
		if isClientError(*lastPtr) {
			terminal = true
		}
	}
	if !terminal {
		return
	}

	if a.rebuild.consecutiveFails.Load() >= sdkMaxConsecutiveRebuilds {
		a.rebuild.skippedMaxStreak.Add(1)
		a.logger.Error("feishu: rebuild skipped after max consecutive failures",
			"app_id", a.appID(),
			"consecutive_failures", a.rebuild.consecutiveFails.Load(),
			"last_err", errStringFor(lastPtr))
		return
	}

	if t := a.rebuild.lastAt.Load(); t != nil {
		if time.Since(*t) < sdkRebuildCooldown {
			a.rebuild.skippedCooldown.Add(1)
			a.logger.Debug("feishu: rebuild skipped: cooldown",
				"app_id", a.appID(),
				"cooldown", sdkRebuildCooldown.String(),
				"since_last", time.Since(*t).String())
			return
		}
	}

	if err := a.rebuildWSClient(); err != nil {
		a.logger.Warn("feishu: rebuildWSClient failed",
			"app_id", a.appID(),
			"err", err.Error())
		return
	}
	// Successful rebuild — the new *larkws.Client is non-terminal
	// (fresh), so the stranded flag no longer applies. consecutiveFails
	// stays — it represents how many *ClientError events we've
	// survived, and we shouldn't forget that.
	a.rebuild.cancelStrandedTerminal.Store(false)
}

// rebuildWSClient constructs a fresh *larkws.Client and atomically
// swaps it into a.client / a.wsStart / a.wsClose. The old client is
// Close()d (no-op when already terminal) so we don't leak a stale
// socket on the heap. The handler + dispatcher are reused from the
// old client via Client.EventHandler() — its closure-bound callbacks
// still reference the same *Adapter, so receipts / health / prober
// all see the same lifecycle.
//
// The new client gets sdkHandshakeTimeout + sdkHTTPTimeout so the
// very first dial after a wake has headroom over the SDK's tight
// defaults (45s / 10s).
func (a *Adapter) rebuildWSClient() error {
	a.mu.RLock()
	cur := a.client
	a.mu.RUnlock()
	if cur == nil {
		return errors.New("feishu: rebuildWSClient: no current client")
	}

	newClient := a.buildWSClient(cur.EventHandler())

	a.mu.Lock()
	a.client = newClient
	a.wsStart = newClient.Start
	a.wsClose = newClient.Close
	a.mu.Unlock()

	now := time.Now()
	a.rebuild.count.Add(1)
	a.rebuild.lastAt.Store(&now)

	// cur.Close is a method value; on a non-nil *larkws.Client it's
	// always non-nil and is a no-op when the client is already in
	// the SDK's terminal state (Stop() short-circuits on
	// run.stopReason != runStopNone).
	cur.Close()
	a.logger.Info("feishu: ws client rebuilt after terminal state",
		"app_id", a.appID(),
		"rebuild_count", a.rebuild.count.Load())
	return nil
}

// appID returns the configured Feishu app id, or "unknown" when
// a.cfg is nil (only happens in unit-test paths). Used by the
// rebuild log lines so a nil cfg in tests doesn't panic the slog
// attribute formatter.
func (a *Adapter) appID() string {
	if a.cfg == nil {
		return "unknown"
	}
	return a.cfg.Feishu.AppID
}

// errStringFor formats lastStartErr (or "" if nil) for log fields.
// Tolerant of nil so callers can hand in the result of
// a.lastStartErr.Load() without a guard.
func errStringFor(errPtr *error) string {
	if errPtr == nil {
		return ""
	}
	return (*errPtr).Error()
}

// markSDKConnected is the OnReady / OnReconnected callback body,
// extracted to a method so the success-state invariants are unit-
// testable without going through the SDK's unexported callback
// channel. Both signals mean "WS is healthy, reset rebuild state".
//
// Without this reset, the still-running prober (started by the
// previous run's OnDisconnected) keeps ticking every 30s, cancels
// the new run's context (runStopByContext → terminal), and the
// rebuild path spins on errClientTerminal forever — exactly the
// failure mode this feature exists to fix.
//
// Called from two SDK callbacks in buildWSClient (OnReady for a
// rebuilt client's first connect, OnReconnected for every
// subsequent reconnect).
func (a *Adapter) markSDKConnected() {
	now := time.Now()
	a.health.recordConnect(now)
	a.logger.Info("feishu: ws connected",
		"app_id", a.appID(),
		"reconnect_count", a.health.Snapshot().ReconnectCount)
	// Terminal-recovery telemetry: every successful connect
	// (initial after rebuild, or reconnect after a network blip)
	// clears the rebuild budget so a future disconnect starts
	// from a clean slate. Without clearing lastStartErr a future
	// transient disconnect would rebuild against the long-since-
	// resolved terminal error; without clearing the stranded flag
	// a future cancel would also rebuild unnecessarily.
	a.rebuild.consecutiveFails.Store(0)
	a.rebuild.cancelStrandedTerminal.Store(false)
	a.lastStartErr.Store(nil)
	if a.prober != nil {
		a.prober.Stop()
		a.logger.Info("feishu: reconnect prober stopped",
			"app_id", a.appID(),
			"force_attempts", a.prober.Snapshot().ForceCount)
	}
}

// buildWSClient is the single source of truth for the *larkws.Client
// construction. NewAdapter calls it once on startup; rebuildWSClient
// calls it on every terminal recovery. Both call sites share the
// same dialer / httpClient configuration so the post-wake relaxations
// apply uniformly (not just on rebuild).
//
// handler is the SDK's event dispatcher — we accept it as a parameter
// rather than reading it off Adapter so the rebuild path can pull it
// from the about-to-be-replaced client via Client.EventHandler().
// nil is a valid handler for tests; the SDK accepts nil and just
// doesn't dispatch any events on the resulting *Client.
func (a *Adapter) buildWSClient(handler *larkdispatcher.EventDispatcher) *larkws.Client {
	dialer := &websocket.Dialer{
		HandshakeTimeout: sdkHandshakeTimeout,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	httpTransport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          10,
	}
	httpClient := &http.Client{
		Timeout:   sdkHTTPTimeout,
		Transport: httpTransport,
	}

	return larkws.NewClient(
		a.cfg.Feishu.AppID,
		a.cfg.Feishu.AppSecret,
		larkws.WithEventHandler(handler),
		larkws.WithHttpClient(httpClient),
		larkws.WithWebSocketDialer(dialer),
		larkws.WithOnReady(a.markSDKConnected),
		larkws.WithOnError(func(err error) {
			if err == nil {
				return
			}
			now := time.Now()
			a.health.recordError(now, err.Error())
			a.logger.Warn("feishu: ws error",
				"app_id", a.appID(),
				"err", err.Error())
		}),
		larkws.WithOnDisconnected(func() {
			now := time.Now()
			a.health.recordDisconnect(now)
			a.logger.Warn("feishu: ws disconnected",
				"app_id", a.appID())
			if a.prober != nil {
				if a.prober.Start() {
					a.logger.Info("feishu: reconnect prober started",
						"app_id", a.appID(),
						"interval", defaultProberInterval.String())
				}
			}
		}),
		larkws.WithOnReconnecting(func() {
			now := time.Now()
			a.health.recordReconnecting(now, "")
			snap := a.health.Snapshot()
			a.logger.Warn("feishu: ws reconnecting",
				"app_id", a.appID(),
				"reconnect_count", snap.ReconnectCount)
		}),
		larkws.WithOnReconnected(a.markSDKConnected),
	)
}
