// Package feishu — WS *Client rebuild path.
//
// The larksuite/oapi-sdk-go v3 ws.Client sets an internal `terminal`
// flag to true on any non-retryable error, specifically any
// *ws.ClientError. The handshake codes that produce one and that we
// see in practice are:
//
//   - 514 + ExceedConnLimit (1000040350) — the Feishu server still
//     sees the old pre-sleep device_id as alive when our first
//     reconnect dial lands. macOS wake + short sleep duration is the
//     canonical trigger.
//   - 403 — app permission revoked; once-per-process retry does not
//     help, but a fresh *Client also won't help, so we still bound
//     the rebuild loop below.
//
// Once terminal, every subsequent client.Start returns
// errClientTerminal — the *Client cannot be revived and a brand-new
// *Client must be constructed (the SDK has no reset / rearm API).
//
// This file adds the rebuild escape hatch: ReconnectSDK observes the
// previous run's terminal error, constructs a fresh *larkws.Client
// (with widened HandshakeTimeout / httpClient.Timeout so the first
// dial after a wake has headroom), then spawns the new run. Cooldown
// + max consecutive failures bound the rebuild rate: a misconfigured
// credential that returns *ClientError every dial must not spin the
// rebuild at the prober's 30s cadence forever.
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
}

func newRebuildState() *rebuildState { return &rebuildState{} }

func (s *rebuildState) snapshot() RebuildSnapshot {
	out := RebuildSnapshot{
		RebuildCount:          s.count.Load(),
		ConsecutiveFailures:   s.consecutiveFails.Load(),
		SkippedCooldown:       s.skippedCooldown.Load(),
		SkippedMaxConsecutive: s.skippedMaxStreak.Load(),
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

// isTerminalSDKError reports whether err is a *ws.ClientError (the
// SDK marker for non-retryable handshake / endpoint errors, all of
// which flip the SDK's terminal flag and freeze the *Client).
//
// We use errors.As because the SDK sometimes wraps the *ClientError
// in higher-level types (and a future SDK version may wrap further).
// The catch-all `*ws.ClientError` arm matches anything the SDK
// classifies as non-retryable — codes that come up in practice are
// 514 + Handshake-Autherrcode=1000040350 (ExceedConnLimit, the macOS-
// wake signature), 403 (permission revoked), and the various
// bootstrap / endpoint error codes from fetchEndpoint.
func isTerminalSDKError(err error) bool {
	if err == nil {
		return false
	}
	var ce *larkws.ClientError
	return errors.As(err, &ce)
}

// recordLastStartErr is called from the goroutine that wraps
// client.Start in Start() and ReconnectSDK(). err is the value
// returned by client.Start — the run's final lifecycle error. We
// only record non-nil, non-context.Cancel errors; transient context
// cancels are noise (they happen on every normal Stop / reconnect).
//
// When the err is terminal (see isTerminalSDKError) we also stamp
// the dedicated lastTerminalErr / lastTerminalErrAt so the snapshot
// path can surface the "this is not going to fix itself" signal
// and bump the consecutive-fail streak (which gates rebuild).
// Non-terminal errs reset the streak so a long stretch of retryable
// failures (network outage) doesn't poison the rebuild budget once
// we finally hit a *ClientError.
func (a *Adapter) recordLastStartErr(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	cp := err
	a.lastStartErr.Store(&cp)
	if isTerminalSDKError(err) {
		now := time.Now()
		msg := err.Error()
		a.rebuild.lastTerminalErr.Store(&msg)
		a.rebuild.lastTerminalErrAt.Store(&now)
		a.rebuild.consecutiveFails.Add(1)
		return
	}
	a.rebuild.consecutiveFails.Store(0)
}

// maybeRebuildClient runs once per ReconnectSDK tick. Returns nil
// even when it declines to rebuild (cooldown / max-streak guard) so
// the caller can always proceed with the normal Start flow. The
// rebuild itself is gated on:
//
//  1. lastStartErr must be a terminal error (else nothing to fix).
//  2. consecutive-failure streak below sdkMaxConsecutiveRebuilds.
//  3. rebuild cooldown elapsed since the last successful rebuild.
//
// All three guards increment counter fields so `nightme health` can
// see why a rebuild was skipped.
func (a *Adapter) maybeRebuildClient() {
	lastPtr := a.lastStartErr.Load()
	if lastPtr == nil {
		return
	}
	last := *lastPtr
	if !isTerminalSDKError(last) {
		return
	}

	if a.rebuild.consecutiveFails.Load() >= sdkMaxConsecutiveRebuilds {
		a.rebuild.skippedMaxStreak.Add(1)
		a.logger.Error("feishu: rebuild skipped after max consecutive failures",
			"app_id", a.appID(),
			"consecutive_failures", a.rebuild.consecutiveFails.Load(),
			"last_err", last.Error())
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
		larkws.WithOnReady(func() {
			now := time.Now()
			a.health.recordConnect(now)
			a.logger.Info("feishu: ws connected",
				"app_id", a.appID(),
				"reconnect_count", a.health.Snapshot().ReconnectCount)
		}),
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
		larkws.WithOnReconnected(func() {
			now := time.Now()
			a.health.recordConnect(now)
			a.logger.Info("feishu: ws reconnected",
				"app_id", a.appID(),
				"reconnect_count", a.health.Snapshot().ReconnectCount)
			// Terminal-recovery telemetry: every successful
			// reconnect-after-rebuild resets the consecutive-fail
			// streak so a one-off *ClientError doesn't permanently
			// burn the rebuild budget.
			a.rebuild.consecutiveFails.Store(0)
			// Clear lastStartErr so a future disconnect (transient
			// network blip, server-side maintenance) doesn't trigger
			// a spurious rebuild against the long-since-resolved
			// terminal error. Without this, maybeRebuildClient
			// would see the stale *ws.ClientError from yesterday's
			// wake and tear down a perfectly healthy WS.
			a.lastStartErr.Store(nil)
			if a.prober != nil {
				a.prober.Stop()
				a.logger.Info("feishu: reconnect prober stopped",
					"app_id", a.appID(),
					"force_attempts", a.prober.Snapshot().ForceCount)
			}
		}),
	)
}
