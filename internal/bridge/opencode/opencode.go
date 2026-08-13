//go:build !windows

// Package opencode implements the bridge to the opencode CLI via its
// first-party HTTP server (`opencode serve`).
//
// Compared to the other nightme bridges (claudecode / codex / pi / acp)
// this one is structurally simpler:
//
//   - Transport: plain HTTP + Server-Sent Events. No JSON-RPC envelope,
//     no NDJSON framing, no PTY carrier. The TS SDK is a thin wrapper
//     around the same HTTP API; we hand-write the equivalent Go client.
//   - Process lifecycle: 1 bridge session = 1 `opencode serve` child
//     process. The server listens on 127.0.0.1:<random-port> (we pass
//     --port 0 and parse the URL from stdout). Sessions are server-side
//     resources scoped to a workspace via the `x-opencode-directory`
//     header (mirrors the v2 SDK behaviour).
//   - Events: the single SSE stream is the only event source. The
//     discriminator on each event is `type`, and the heavy lifting
//     happens in message.part.updated with a `part` union. We dispatch
//     part types to AgentEvent (text → EventAgentText, reasoning →
//     EventAgentText with the [思考] prefix, tool → tool lifecycle
//     events).
//
// Design reference: docs/feat/F-OPENCODE-opencode-bridge.md.
//
// Agent is BOTH the template (registered with agent.Builtins) and the
// live handle (returned by Start). The template half is set once by
// New and is immutable thereafter; Start clones the receiver and
// populates runtime fields on the clone. The two states share one type
// so the registry, the Spawner, and AgentSession.handle all deal with a
// single agent.Agent — no separate session struct.
//
// Session lifetime is two-tier:
//
//   - process: serveCmd.Start() → "opencode server listening on http://..."
//     captured from stdout → ... → Close() → cmd KILL
//   - turn:    POST /api/session/{id}/prompt → SSE events … →
//     session.idle (translated to EventAgentDone{Reason:"settled"})
//
// The process carries many turns. EventAgentDone marks the end of one
// turn but does NOT close the events channel; only process exit or
// Close() does. This mirrors the contract documented in
// docs/bridge/codex.md §2.3 and lets ChatSession.runReadPump continue
// reading across many turns on the same AgentSession.
package opencode

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"
)

// ─── errors ───

// ErrSessionClosed is returned by Client calls after the server process
// has exited. Mirrors the codex/pi convention so the runtime can
// distinguish send-on-dead-session from a real wire error.
var ErrSessionClosed = errors.New("opencode: session closed")

// ErrServerStartTimeout is returned by startServer when the
// "opencode server listening on http://..." banner does not appear
// within serverStartTimeout. The caller should treat this as a fatal
// Start error.
var ErrServerStartTimeout = errors.New("opencode: server start timeout")

// ErrNoPendingPermission is returned by SendPermission when there is no
// outstanding permission/asked event to answer. Matches the
// codex/pi convention.
var ErrNoPendingPermission = errors.New("opencode: no pending permission answer")

// ErrImageTooLarge is returned by SendBlocks when a single image
// exceeds maxImageBytes. Mirrors the limit used by codex / pi.
var ErrImageTooLarge = errors.New("opencode: image too large")

// ErrTurnBusy is returned by SendBlocks when a previous turn is still
// streaming. The caller may retry after session.idle arrives.
var ErrTurnBusy = errors.New("opencode: previous turn still active")

// ─── timing ───

// serverStartTimeout bounds how long we wait for the
// "opencode server listening on http://..." banner after spawning
// `opencode serve`. Cold start with model preload can take a few
// seconds; 10s matches the other bridges' handshakeTimeout.
const serverStartTimeout = 10 * time.Second

// handshakeTimeout bounds the first HTTP round-trip (POST /api/session
// or GET /api/session/{id}). opencode answers within a couple of
// seconds even on cold start; 10s matches the other bridges.
const handshakeTimeout = 10 * time.Second

// shutdownGrace is the SIGINT-to-SIGKILL window for Close(), kept
// short so /close on a stuck server does not hang the runtime.
const shutdownGrace = 2 * time.Second

// closeDrainTimeout bounds the time Close() will wait for the
// lifecycle goroutine to reap the child and close the events channel.
// Beyond this window Close returns even if the underlying cmd.Wait is
// wedged (zombie / SIGKILL reap not landing).
const closeDrainTimeout = 5 * time.Second

// promptTimeout bounds a single POST /api/session/{id}/prompt and its
// matching session.idle. Even with slow APIs, 90s usually suffices; we
// leave 30s more headroom than codex's `promptTimeout` to account for
// SSE delivery lag.
const promptTimeout = 90 * time.Second

// permissionTimeout is how long an EventAgentPermission waits for a
// user decision before defaulting to reject. Mirrors codex's
// 5-minute default.
const permissionTimeout = 5 * time.Minute

// startupMaxAttempts bounds the Start retry loop. Two attempts
// covers the common "stale HOME/.opencode state" case without
// hiding genuine misconfiguration (auth / binary missing). Tested
// as the upper bound for the retry budget in agent_test.go.
const startupMaxAttempts = 2

// startupRetryDelay is the wait between Start attempts. Long
// enough that any transient I/O settle, short enough that the
// user does not notice on the happy path.
const startupRetryDelay = 2 * time.Second

// turnWatchdogTimeout bounds the wall time between consecutive SSE
// events during a turn. Resets on every event delivered by the
// translator (model is alive, plugin loaded, etc.). On timeout the
// bridge kills the server and emits EventAgentError so the
// runtime readpump clears the busy guard and the chat surfaces a
// clear "agent session timed out (no response)" message instead
// of hanging on the busy spinner.
//
// Default 10 minutes — matches the model of "humans typing into
// chat apps are patient but not infinitely so". cc-connect's
// equivalent uses 2 hours (defaultEventIdleTimeout in their
// engine.go) but our session-scoped bridge plus the per-turn
// busy-guard semantics argue for a tighter bound; runtime
// operators can extend via NIGHTME_OPENCODE_TURN_WATCHDOG.
const turnWatchdogTimeout = 10 * time.Minute

// turnWatchdogEmptyFlag is the Done.Reason string we use when the
// turn settled normally but produced zero EventAgentText /
// EventAgentToolStart events. Runtime uses this to surface
// "(empty response)" hints in the channel footer.
const turnWatchdogEmptyFlag = "empty"

// ─── buffer sizes ───

// eventBufferSize is the events channel capacity.
//
// Sized to match the producer-side contract promoted in commit
// 67b295ec ("unify producer-side buffer contract across all bridges"):
// 40960 across pi / claudecode / pty / acp. The codex bridge adopted
// the same value (see internal/bridge/codex/session.go eventBufferSize).
//
// Allocated in newSession; tests pin the value at the package level so
// a regression that lowers the cap or reintroduces a default-drop is
// caught in `go test`.
const eventBufferSize = 40960

// maxImageBytes is the upper bound for a single image attachment read
// into memory before base64-encoding into the prompt parts. 10 MiB
// matches the codex / pi limit.
const maxImageBytes = 10 * 1024 * 1024

// sseBufferSize is the maximum size of one SSE data line. opencode can
// emit fairly large message-part.updated payloads (full tool argument
// dumps); 10 MiB is generous and matches the codex MaxFrameSize.
const sseBufferSize = 10 * 1024 * 1024

// ─── debug ───

// opencodeDebug toggles the bridge's detailed debug logging. Default
// ON so a "why is opencode stuck" incident produces a usable
// breadcrumb trail. Silence it with NIGHTME_OPENCODE_DEBUG=0
// (also accepts "false", "no", "off", case-folded).
var opencodeDebug = opencodeDebugEnabled()

func opencodeDebugEnabled() bool {
	v := os.Getenv("NIGHTME_OPENCODE_DEBUG")
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes", "on":
		return true
	}
	return false
}

// version is the bridge version reported to opencode via the
// Client-Name header. Kept in sync with the module's semantic intent;
// bump manually when the HTTP contract changes materially.
const version = "0.1.0"

// ─── logging ───

// oLog emits an info-level message tagged [opencode] (component=
// "opencode") when debug is enabled. Mirrors the codex cLog / pi piLog
// pattern so log scrapers see a consistent component label across all
// bridges. Tests in this package may swap slog.Default via
// slog.SetDefault() to keep test output clean.
func oLog(msg string, args ...any) {
	if !opencodeDebug {
		return
	}
	all := make([]any, 0, len(args)+2)
	all = append(all, "component", "opencode")
	all = append(all, args...)
	slog.Default().Info("[opencode] "+msg, all...)
}
