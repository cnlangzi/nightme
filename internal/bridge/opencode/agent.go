
// Package opencode — shared helpers for the bridge.
//
// agent.go (this file) holds:
//   - the helpers used by both Starter (starter.go) and driver
//     (driver.go): deliver, sseLoop (with reconnect), lifecycle,
//     watchdog, livenessProbeConfig, finishTurn, detectBranch,
//     isUnrecoverableStartErr, oLog.
//
// The Starter + driver split (see starter.go and driver.go
// respectively) follows the codebase-wide Info/Starter/Agent/
// driver three-piece model: Starter is the immutable spawn recipe,
// driver is the unexported runtime state, agent.NewAgent wraps
// driver into the unified *agent.Agent that runtime consumers see.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// isUnrecoverableStartErr returns true for start errors that
// retrying cannot fix. The retry wrapper in newDriver skips the
// next attempt when this returns true. Currently:
//
//   - binary not found ("executable file not found") — the same
//     missing binary will not magically appear.
//   - "command not found" — same root cause.
//   - "no such file" — same root cause.
//   - context.Canceled / context.DeadlineExceeded — the caller
//     already gave up.
//
// Network / timeout / "stale HOME state" errors are considered
// recoverable.
func isUnrecoverableStartErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "executable file not found") ||
		strings.Contains(msg, "command not found") ||
		strings.Contains(msg, "no such file")
}

// ─── deliver ─────────────────────────────────────────────────────

// deliver stamps the bridge-side session context onto every event
// before delivery, then blocks on pushing onto the events channel
// until either the runtime drains it OR the session is closed / the
// lifecycle has reaped the server.
//
// Producer-side contract (matches codex / pi / claudecode / pty / acp,
// promoted in commit 67b295ec):
//   - No `default:` instant-drop. The producer is allowed to block
//     until the consumer (runtime readpump) drains. The channel's
//     40960 buffer absorbs bursts.
//   - No timeout drop. A `case <-time.After(1s)` branch was the
//     root cause of the F-54 "bridge reset: pi: new_session:
//     context deadline exceeded" incident.
//   - Close signals release a parked deliver(). This prevents leaked
//     goroutines after the session is torn down.
func (d *driver) deliver(ev agent.AgentEvent) agent.AgentEvent {
	if d.server == nil {
		return ev
	}
	if ev.SessionID == "" {
		ev.SessionID = d.sessionID
	}
	if ev.Model == "" {
		ev.Model = d.model
	}
	ev.AgentName = d.name
	ev.Workspace = d.workspace
	ev.Branch = d.branch

	select {
	case d.events <- ev:
	case <-d.stopDeliver:
		// Lifecycle has begun teardown. Drop silently.
	case <-d.closed:
		oLog("deliver dropped (session closed)", "kind", ev.Kind.String())
	case <-d.exitDone:
		// lifecycle closed exitDone after wait returned; the bridge
		// is being torn down. Drop silently.
	}
	return ev
}

// ─── SSE reader + lifecycle ──────────────────────────────────────

// sseLoop owns the long-lived connection to /api/event. It
// subscribes, drains events, and on ANY disconnect (graceful EOF,
// wire error, server restart, network blip) it backs off and
// reconnects — local `opencode serve` on 127.0.0.1 SHOULD never
// drop, but when it does (kernel resource limit, proxy idle, the
// server process briefly restarting itself) we self-heal instead
// of going silently blind.
//
// We delegate event handling to the translator; this function
// never touches the events channel directly.
//
// Exit conditions:
//   - sseCtx (parent ctx) cancelled — Close() / liveness-kill /
//     caller shutdown.
//   - d.closed signalled — Close().
//   - Non-retryable subscribe error (HTTP 4xx: auth, not-found,
//     bad-request). These will not fix themselves with retries.
//
// Backoff: starts at sseReconnectMin (100ms), doubles up to
// sseReconnectMax (5s), with full jitter so concurrent bridge
// sessions do not thundering-herd the server on a restart.
func (d *driver) sseLoop(ctx context.Context) {
	d.pumpWG.Add(1)
	defer d.pumpWG.Done()
	defer d.finishTurn()

	backoff := sseReconnectMin
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.closed:
			return
		default:
		}

		body, err := d.client.Subscribe(ctx, d.sessionID)
		if err != nil {
			if !isRetryableSubscribeErr(err) {
				oLog("sse subscribe error (non-retryable), giving up",
					"err", err.Error())
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-d.closed:
				return
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff)
			oLog("sse subscribe failed, will retry",
				"err", err.Error(),
				"backoff", backoff,
			)
			continue
		}

		// Connected. Drop the busy guard if it was set by a
		// previous turn whose session.idle / session.error event
		// we may have missed during the disconnect window — local
		// TCP drops should not strand the user on ErrTurnBusy.
		// The server will either push continuing events (in which
		// case the new turn waits naturally) or has already
		// finished (in which case we free the next SendBlocks).
		d.pendingMu.Lock()
		d.pendingTurnActive = false
		d.pendingMu.Unlock()
		// Stamp the watchdog's "last event" timer immediately so a
		// quiet SSE stream from the start does not look like a hang.
		d.lastEventAtUnixNano.Store(time.Now().UnixNano())

		// Note: backoff is NOT reset on a bare Subscribe success.
		// A server that closes the connection immediately on every
		// attempt (e.g. broken handshake, auth round-trips, kernel
		// drops) would otherwise be hammered at sseReconnectMin.
		// We only reset after a connection has been stable for
		// at least sseStableGrace (we observed at least one event
		// OR the connection lasted longer than the grace window).
		stable := false
		connectedAt := time.Now()
		gotEvent := false

		err = decodeSSE(body, func(ev SessionEvent) error {
			gotEvent = true
			stable = true
			d.lastEventAtUnixNano.Store(time.Now().UnixNano())
			if ev.Type == "permission.asked" {
				var p PermissionAsked
				if err := json.Unmarshal(ev.properties(), &p); err == nil {
					d.pendingMu.Lock()
					d.pendingApprovalID = p.ID
					d.pendingMu.Unlock()
				}
			}
			switch ev.Type {
			case "session.idle", "session.error":
				d.pendingMu.Lock()
				d.pendingTurnActive = false
				d.pendingMu.Unlock()
			}
			return d.trans.handleEvent(ev)
		})
		_ = body.Close()

		// If the connection survived long enough without an event
		// (e.g. opencode answered the GET but the model stayed
		// silent for the grace window), treat that as stability
		// too. We don't want the bridge to ramp up backoff just
		// because the model is thinking.
		if !gotEvent && time.Since(connectedAt) >= sseStableGrace {
			stable = true
		}
		if stable {
			backoff = sseReconnectMin
		}

		if err != nil {
			oLog("sse read error, will reconnect", "err", err.Error())
		} else {
			oLog("sse closed by server, will reconnect")
		}

		// Honour shutdown before backing off.
		select {
		case <-ctx.Done():
			return
		case <-d.closed:
			return
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff)
	}
}

// isRetryableSubscribeErr returns true for subscribe errors that
// may fix themselves with a short retry: network failures, server
// 5xx, EOF. Returns false for 4xx (auth, not-found, bad-request)
// which will only succeed again after a config change.
func isRetryableSubscribeErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// 4xx is non-retryable; 5xx is transient. The Subscribe wrapper
	// formats errors as "opencode: subscribe: <code>: <body>".
	for _, code := range []string{": 400", ": 401", ": 403", ": 404", ": 410"} {
		if strings.Contains(msg, code) {
			return false
		}
	}
	return true
}

// nextBackoff doubles the wait, capped at sseReconnectMax, with
// full jitter so concurrent reconnects do not synchronise.
func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > sseReconnectMax {
		next = sseReconnectMax
	}
	// full jitter in [next/2, next]
	half := next / 2
	jitter := time.Duration(time.Now().UnixNano() % int64(half+1))
	return half + jitter
}

// finishTurn is called when the SSE stream closes. It releases the
// pendingTurnActive guard so the next SendBlocks can proceed. The
// retry trap (turnActive not released) was the F-54 / F-32 root
// cause for the other bridges; we mirror the same fix.
func (d *driver) finishTurn() {
	d.pendingMu.Lock()
	d.pendingTurnActive = false
	d.pendingMu.Unlock()
}

// lifecycle is the single owner of the events-channel close. Once-close
// semantics are enforced by the closeOnce in Close(); everything else
// just nudges the process toward a clean exit.
func (d *driver) lifecycle() {
	// Both closes are in defer so a panic anywhere in the body
	// (or in pumpWG.Wait / Process.Wait) still tears the bridge
	// down cleanly. Mirrors codex/dsh/pi — opencode was the
	// outlier before this change. With these defers, a recovered
	// lifecycle panic still closes events/stopDeliver; without
	// them, a panic would orphan consumers waiting on either
	// channel. The order — stopDeliver first, events last —
	// matches the producer-side back-pressure contract: a producer
	// selecting on <-d.stopDeliver sees the close before events is
	// closed, so any in-flight deliver() exits without sending.
	defer close(d.events)
	defer close(d.stopDeliver)
	defer close(d.exitDone)
	if d.server != nil && d.server.cmd != nil {
		_, _ = d.server.cmd.Process.Wait()
	} else {
		<-d.closed
	}
	d.pumpWG.Wait()
}

// ─── turn watchdog ───────────────────────────────────────────────

// watchdogTimeout returns the configured turn watchdog timeout,
// with NIGHTME_OPENCODE_TURN_WATCHDOG as the per-deployment
// override (e.g. operators with a slow enterprise model bump
// this to 30m). Returns 0 to disable the watchdog entirely.
func watchdogTimeout() time.Duration {
	v := strings.TrimSpace(osGetenv("NIGHTME_OPENCODE_TURN_WATCHDOG"))
	if v == "" {
		return turnWatchdogTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return turnWatchdogTimeout
	}
	return d
}

// watchdog is a per-turn self-healing timer. Patterned after
// cc-connect (defaultEventIdleTimeout in their engine.go:953):
// the timer is reset on every SSE event (sseLoop writes
// lastEventAtUnixNano on each frame). If the gap exceeds the
// threshold while a turn is pending, we kill the server,
// synthesise an EventAgentError, and let the runtime readpump
// surface a clear "agent session timed out (no response)" message
// instead of leaving the chat stuck on the busy spinner.
//
// The watchdog exits as soon as the busy-guard drops (terminal
// event arrived) or the bridge closes (Close was called).
func (d *driver) watchdog() {
	timeout := watchdogTimeout()
	if timeout <= 0 {
		return
	}
	tickInterval := timeout / 10
	if tickInterval < 20*time.Millisecond {
		tickInterval = 20 * time.Millisecond
	} else if tickInterval > 5*time.Second {
		tickInterval = 5 * time.Second
	}
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.closed:
			return
		case <-ticker.C:
			d.pendingMu.Lock()
			busy := d.pendingTurnActive
			d.pendingMu.Unlock()
			if !busy {
				return
			}
			lastEvent := time.Unix(0, d.lastEventAtUnixNano.Load())
			if time.Since(lastEvent) < timeout {
				continue
			}
			oLog("watchdog: turn timeout, killing session",
				"timeout", timeout,
				"since_last_event", time.Since(lastEvent),
			)
			d.deliver(agent.AgentEvent{
				Kind:      agent.EventAgentError,
				SessionID: d.sessionID,
				Model:     d.model,
				AgentName: d.name,
				Workspace: d.workspace,
				Branch:    d.branch,
				Err:       fmt.Errorf("opencode: turn watchdog timeout (no events for %s)", timeout),
			})
			_ = d.Close()
			return
		}
	}
}

// ─── helpers ─────────────────────────────────────────────────────

// detectBranch returns the current git branch for workspace, or "" on
// any failure (non-git workspace, git not installed, detached HEAD).
// Mirrors the helper used by the codex / pi bridges.
func detectBranch(workspace string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// osGetenv is the package-level binding for os.Getenv. Wrapped in
// a var so tests can stub the environment lookup without mutating
// the process environment.
var osGetenv = os.Getenv

// livenessProbeConfig returns (interval, per-probe timeout,
// failure threshold), with NIGHTME_OPENCODE_LIVENESS_INTERVAL /
// _TIMEOUT / _THRESHOLD env-var overrides for tests and operators
// who want tighter or looser behaviour. Matches the
// watchdogTimeout() env-override pattern.
//
//   - interval:   how often the probe ticks. Must be > 0.
//   - timeout:    per-probe HTTP timeout. Must be > 0.
//   - threshold:  consecutive failures that trigger teardown.
//                 Must be >= 1.
//
// On parse error or invalid value we fall back to the package
// default. Test code should set the env vars and restart the
// goroutine to pick them up.
func livenessProbeConfig() (time.Duration, time.Duration, int) {
	interval := livenessProbeInterval
	timeout := livenessProbeTimeout
	threshold := livenessFailThreshold

	if v := strings.TrimSpace(osGetenv("NIGHTME_OPENCODE_LIVENESS_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}
	if v := strings.TrimSpace(osGetenv("NIGHTME_OPENCODE_LIVENESS_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			timeout = d
		}
	}
	if v := strings.TrimSpace(osGetenv("NIGHTME_OPENCODE_LIVENESS_THRESHOLD")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			threshold = n
		}
	}
	return interval, timeout, threshold
}
