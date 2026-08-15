// Package agent — SafeGo: panic-isolated goroutine launcher.
//
// Bridges spawn multiple long-lived goroutines (WS read pumps, HTTP
// SSE pumps, child-process drainers, lifecycle watchers). Before
// SafeGo, an unhandled panic in any one of these took down the
// entire nightme daemon — we hit this in production on 2026-08-15
// when a dsh `assistant/chunk` translator panic propagated up the
// mux pump goroutine and crashed the host process, leaving every
// chatsession orphaned and the daemon unable to restart cleanly
// (stale daemon.lock vs. dead PID — see translate.go for the
// specific textBuf root cause).
//
// SafeGo is the bridge-tier equivalent of cmd/nightme/recover.go's
// Recover(): both wrap a call site in `defer recover()` and log
// the panic + stack via slog. SafeGo targets goroutines (no cobra
// context, no error to return — the goroutine simply exits), so
// its surface is intentionally tiny:
//
//   agent.SafeGo("dsh:mux-pump", func() {
//       defer agent.PanicEventHandler("dsh:mux-pump", d.deliver,
//           d.sessionID, d.agentName, d.workspace, d.branch)
//       readMuxPump(...)
//   })
//
// The name is mandatory: panics in long-running goroutines are
// otherwise invisible, and the stack trace is the only clue which
// pump went bad. A recovery log without `name` is just a fire alarm
// with no address.
//
// Two helpers, two layers:
//
//   - SafeGo: outer (daemon-level) safety net. Recovers from any
//     panic, logs it, keeps the nightme daemon alive. Use for
//     every long-lived per-session goroutine a bridge spawns.
//
//   - PanicEventHandler: inner (domain-level) recovery. Designed
//     to be used as `defer PanicEventHandler(...)` inside the fn
//     passed to SafeGo. On panic it both logs AND emits an
//     EventAgentError via the bridge's deliver function, so the
//     runtime can surface a "bridge session died" notification
//     to the user. Without this layer a recovered panic leaves
//     a zombie session: daemon alive, but no events flow and
//     nothing tells the user why.
//
//   SafeGo and PanicEventHandler compose: PanicEventHandler
//   catches the domain panic first (so the user gets notified),
//   and if anything in PanicEventHandler's own recovery path
//   (e.g. deliver itself) panics, SafeGo's outer recover is
//   the last line of defense.
//
// Usage rules (mirrors the F-32 / nightme bot 失败处理优先级
// memory: try silent recovery before notifying — a recovered
// bridge panic should NOT page anyone, it should just be in the
// daemon log so the next maintenance pass can pick it up):
//
//   - Call SafeGo + PanicEventHandler for every long-lived per-
//     session goroutine a bridge spawns (read pumps, drainers,
//     lifecycle, watchdog).
//   - Do NOT call them for one-shot goroutines whose error is
//     already captured in a channel / WaitGroup — they'd never
//     panic in normal flow and the wrapping would just hide bugs.
//   - Do NOT call them from inside a defer — defer ordering
//     makes the recover unreliable.
package agent

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// SafeGo runs fn in a new goroutine protected by a panic guard.
//
// On panic, the panic value + full goroutine stack are logged at
// ERROR level and the goroutine exits cleanly — the nightme
// daemon is never taken down by a single bridge bug. Variadic
// loggers mirror cmd/nightme/recover.go: pass one to attach the
// recovery log to a structured logger (e.g. the per-session
// slog handler); nil / empty falls back to slog.Default().
//
// name identifies the goroutine in the log line; pick a stable
// "<bridge>:<role>" tag (e.g. "dsh:mux-pump", "dsh:host-pump",
// "opencode:sse-pump") so multiple pumps in the same bridge are
// distinguishable from a single log search.
//
// SafeGo is intentionally the OUTER layer. For domain-specific
// recovery (emit EventAgentError so the runtime can notify the
// user), pair it with PanicEventHandler as an inner defer in fn.
func SafeGo(name string, fn func(), loggers ...*slog.Logger) {
	logger := slog.Default()
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	go func() {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			logger.Error("bridge goroutine panic recovered",
				"name", name,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}()
		fn()
	}()
}

// PanicEventHandler is the INNER layer of the two-level panic
// recovery used by bridge pump goroutines. Place it as a defer
// inside the fn passed to SafeGo:
//
//   agent.SafeGo("dsh:mux-pump", func() {
//       defer d.pumpWG.Done()
//       defer agent.PanicEventHandler(
//           "dsh:mux-pump", d.deliver,
//           d.sessionID, d.agentName, d.workspace, d.branch)
//       readMuxPump(d.muxWS, "mux", d.handleMuxFrame)
//   })
//
// On a panic, PanicEventHandler:
//  1. recovers the panic value
//  2. logs the panic + full stack at ERROR via slog
//  3. delivers an EventAgentError via the bridge's deliver
//     function, so the runtime can surface a "bridge died" card
//     to the user (matches the nightme bot 失败处理优先级
//     memory: silent recovery first, notify ONLY if the system
//     can't self-heal — a dead pump can't self-heal, so we
//     notify).
//
// On a normal return this function is a no-op (recover() returns
// nil, we exit silently).
//
// SafeGo is the outer safety net — if PanicEventHandler itself
// panics (e.g. a nil-pointer deref in deliver), SafeGo's recover
// still catches it and keeps the daemon alive.
//
// The deliver function MUST be non-nil; passing nil will panic
// (intentional — that's a programmer error, not a runtime
// condition).
func PanicEventHandler(
	name string,
	deliver func(AgentEvent),
	sessionID, agentName, workspace, branch string,
	loggers ...*slog.Logger,
) {
	r := recover()
	if r == nil {
		return
	}
	logger := slog.Default()
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	logger.Error("bridge goroutine panic",
		"name", name,
		"panic", r,
		"stack", string(debug.Stack()),
	)
	deliver(AgentEvent{
		Kind:      EventAgentError,
		SessionID: sessionID,
		AgentName: agentName,
		Workspace: workspace,
		Branch:    branch,
		Err:       fmt.Errorf("%s: panic: %v", name, r),
	})
}
