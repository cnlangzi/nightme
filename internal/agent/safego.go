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
//       readMuxPump(...)
//   })
//
// The name is mandatory: panics in long-running goroutines are
// otherwise invisible, and the stack trace is the only clue which
// pump went bad. A recovery log without `name` is just a fire alarm
// with no address.
//
// Usage rules (mirrors the F-32 / nightme bot 失败处理优先级
// memory: try silent recovery before notifying — a recovered
// bridge panic should NOT page anyone, it should just be in the
// daemon log so the next maintenance pass can pick it up):
//
//   - Call SafeGo for every long-lived per-session goroutine a
//     bridge spawns (read pumps, drainers, watchers).
//   - Do NOT call SafeGo for one-shot goroutines whose error is
//     already captured in a channel / WaitGroup — they'd never
//     panic in normal flow and the wrapping would just hide bugs.
//   - Do NOT call SafeGo from inside a defer — defer ordering
//     makes the recover unreliable.
package agent

import (
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
