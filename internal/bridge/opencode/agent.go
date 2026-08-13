
// Package opencode — shared helpers for the bridge.
//
// agent.go (this file) holds:
//   - the helpers used by both Starter (starter.go) and driver
//     (driver.go): deliver, readSSE, lifecycle, watchdog,
//     finishTurn, detectBranch, isUnrecoverableStartErr, oLog.
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
	"io"
	"os"
	"os/exec"
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

// readSSE owns the SSE body. It blocks on decodeSSE until the server
// closes the stream (graceful EOF → nil) or the wire errors. We
// delegate event handling to the translator; this function never
// touches the events channel directly.
func (d *driver) readSSE(body io.ReadCloser) {
	d.pumpWG.Add(1)
	defer d.pumpWG.Done()
	defer body.Close()
	defer d.finishTurn()

	// Stamp the watchdog's "last event" timer immediately so a
	// quiet SSE stream from the start does not look like a hang.
	d.lastEventAtUnixNano.Store(time.Now().UnixNano())

	err := decodeSSE(body, func(ev SessionEvent) error {
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
	if err != nil {
		oLog("sse read error", "err", err.Error())
	}
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
	defer close(d.exitDone)
	if d.server != nil && d.server.cmd != nil {
		_, _ = d.server.cmd.Process.Wait()
	} else {
		<-d.closed
	}
	d.pumpWG.Wait()
	close(d.stopDeliver)
	close(d.events)
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
// the timer is reset on every SSE event (readSSE writes
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
