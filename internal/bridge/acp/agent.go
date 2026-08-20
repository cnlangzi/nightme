// Package acp also provides the Agent wrapper that turns a CLI
// command into an agent.Agent backed by the Agent Client Protocol
// defined in this package. PTY remains the physical carrier;
// ACP supplies the structured request and event layer above it.
//
// Lives in bridge/acp/ (not in a separate agent package) so the
// whole ACP story is one tree. See docs/feat/F-21-agent-modes.md §5.3.
//
// Agent is BOTH the template (registered with agent.Builtins) and
// the live handle (returned by Start). The template half is set once
// by NewAgent and is immutable thereafter; Start clones the receiver
// and populates runtime fields on the clone. The two states share
// one type so the registry, the Spawner, and AgentSession.handle
// all deal with a single agent.Agent — no separate session struct.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
)

const (
	eventBufferSize   = 40960
	// initializeTimeout bounds the ACP initialize RPC. This is a
	// tight sanity-check: the server should reply within a couple of
	// seconds to prove it is alive and speaks the right protocol
	// version. If it doesn't, something is structurally broken
	// (dead bridge, broken PTY) and we want to fail fast.
	initializeTimeout = 3 * time.Second
	// newSessionTimeout bounds the ACP session/new RPC. This is where
	// the ACP server does the real work. For opencode specifically,
	// `opencode acp` boots its own internal HTTP backend and then
	// performs a parallel `loadDirectorySnapshot` RPC that hits
	// config.providers / app.agents / command.list / app.skills / config.get
	// against the opencode backend (~5 sequential-ish HTTP calls), plus
	// session.create + MCP registration. Cold start on a first-run
	// machine or a slow CI runner can easily exceed 10s, so this budget
	// is intentionally generous. The previous shared `startupTimeout`
	// of 10s would hit during the `session/new` wait and surface as a
	// misleading "context deadline exceeded" long before the bridge had
	// actually hung.
	newSessionTimeout = 45 * time.Second
)

// Agent is the ACP-mode bridge descriptor.
//
// Two states share one type:
//
//   - Template state (after NewAgent, before Start): only the
//     spec-half fields are populated. Registered in agent.Builtins
//     and held there as a long-lived singleton per name.
//
//   - Live state (after Start, before Close): the receiver is a
//     freshly-allocated clone with runtime fields populated (transport,
//     rpc, ctx, events, sessionID, ...). Calls to Events / PID /
//     Send* / New / Close are valid here. Spec-half fields are still
//     readable.
//
// The template (in Builtins) is never mutated; Start returns a
// separate *driver so concurrent Start calls from different chats do
// not interfere with each other.
type driver struct {
	// ─── runtime fields (zero before Start; populated by newDriver) ───
	transport Transport
	rpc       *rpcClient
	ctx       context.Context
	cancel    context.CancelFunc

	// agentName and workspace are captured at Start for the
	// EventAgentReady payload. ACP does not currently tell the runtime
	// about its session id through the channel — we synthesize one
	// here so the rest of the runtime can capture the resume id
	// uniformly (other bridges do this via the bridge's own init
	// event).
	agentName string
	workspace string

	sessionID string
	events    chan agent.AgentEvent

	// connectedSent guards the synthesized EventAgentReady. We emit at
	// most once per session, after the first successful session/new.
	connectedSent bool

	permissionMu sync.Mutex
	permissions  []permissionCall
	closeOnce    sync.Once

	// updateHandler, when non-nil, receives raw session/update
	// params.update payloads BEFORE the built-in 4-case fallback
	// runs. Used by per-agent bridges (opencode) to inject a
	// bridge-specific translator for sessionUpdate variants the
	// generic ACP bridge does not recognize. nil keeps the
	// existing behaviour. See SessionView + SetUpdateHandler
	// below.
	//
	// Stored as atomic.Pointer so SetUpdateHandler and the
	// readPump can access the handler without a mutex. The
	// SetUpdateHandler contract ("must be called BEFORE the
	// readPump observes the first session/update") is the
	// primary correctness guarantee; atomic.Pointer only
	// eliminates the data race between the writer and the
	// reader goroutines.
	//
	// All-or-nothing ownership: a non-nil handler means the bridge
	// takes over the entire sessionUpdate stream and the built-in
	// 4-case fallback in handleSessionUpdate is bypassed. Installing
	// a handler does NOT extend the fallback — it replaces it.
	updateHandler atomic.Pointer[UpdateHandler]

	// flushHandler, when non-nil, lets the bridge-specific UpdateHandler
	// register a "flush any buffered text now" hook. Triggered on
	// SessionView.FlushPending and (via the generic acp bridge) right
	// before EventAgentDone is emitted in translatePromptResponse so a
	// turn-end never silently drops the trailing text. atomic.Pointer
	// mirrors updateHandler so the readPump / writePump race stays free
	// of a mutex.
	flushHandler atomic.Pointer[FlushHandler]

	// methodHandler, when non-nil, receives unrecognized JSON-RPC
	// methods from the ACP server. Used by per-agent bridges
	// (cursor) to handle agent-specific extension methods like
	// cursor/update_todos, cursor/create_plan, etc. The handler
	// is called BEFORE the default "method not found" response.
	// nil keeps the existing behaviour.
	methodHandler atomic.Pointer[MethodHandler]

	// pendingTurnMu guards the in-flight session/prompt guard.
	// Opencode ACP's prompt response IS the turn-end signal (the
	// server resolves only after the turn settles), so the generic
	// acp bridge now blocks on session/prompt until the response
	// arrives. pendingTurnActive tracks that single in-flight
	// turn so a second SendBlocks call gets ErrTurnBusy instead
	// of stacking prompts on the same session.
	//
	// Released by SendBlocks on response receipt (success or
	// error) and by Close as a safety net.
	pendingTurnMu     sync.Mutex
	pendingTurnActive bool

	// ─── built-in text buffering ─────────────────────────────────
	// textBuf accumulates agent_message_chunk payloads (reply text).
	// thoughtBuf accumulates agent_thought_chunk payloads (reasoning).
	// Both are flushed on sentence-terminating punctuation (".?!。！？"),
	// on tool_call boundaries, and at turn-end via flushTextBuffers.
	//
	// This replaces the per-bridge buffering that opencode/cursor
	// previously implemented in their UpdateHandler — all ACP
	// bridges now get automatic sentence-level batching.
	textBuf    *strings.Builder
	thoughtBuf *strings.Builder
	textMu     sync.Mutex

	// thinkingPrefix marks reasoning text so the gateway can route
	// it to the thinking surface (OutThinking) rather than the reply
	// surface (OutReply). Matches pi/dsh/opencode conventions.
	thinkingPrefix string

	// model is the bridge-local cached model name. Captured from
	// vendor-extension sessionUpdate payloads (usage_update.model,
	// session_info_update.model). May stay empty if the server
	// never reports one — runtime tolerates empty Model and the
	// footer just omits the model segment.
	//
	// Concurrent. Writers are handleUsageUpdate /
	// handleSessionInfoUpdate on the readPump goroutine; readers
	// are deliver() called from any goroutine (handshake,
	// SendBlocks via translatePromptResponse, flushTextBuffers).
	// Without modelMu the race detector flags this as a torn
	// string read (P1). Contention is low — writers fire at most
	// a handful of times per turn — so a plain Mutex is fine.
	model   string
	modelMu sync.Mutex

	// lastUsage is the per-turn usage snapshot. Written by
	// handleUsageUpdate on usage_update sessionUpdate; consumed
	// (and cleared) by translatePromptResponse and
	// handleSessionStatus at turn-end. nil = "no usage reported".
	//
	// Concurrent: handleUsageUpdate writes, the two turn-end
	// handlers read+clear. A small mutex protects the pointer.
	lastUsage   *agent.UsageInfo
	lastUsageMu sync.Mutex

	// turnSettled guards against emitting EventAgentDone twice
	// within the same turn. Opencode acp fires both the
	// session/prompt RESPONSE (synchronous, translatePromptResponse
	// path) AND the session.status:idle notification
	// (asynchronous, handleSessionStatus path) for the same turn;
	// we keep the first and drop the second. Reset on every New()
	// when the session id rolls.
	turnSettled   bool
	turnSettledMu sync.Mutex
}


// resetTurnState clears the per-turn dedup flag so the next
// SendBlocks call starts a fresh turn-end emission window. Called
// at the top of SendBlocks after the busy-guard acquisition; the
// state is reset there (not in New()) because turn-end and turn-
// start are paired within SendBlocks's busy-guard window, and the
// runtime calls SendBlocks for every turn.
//
// Factored out of SendBlocks so the regression test
// (TestSendBlocks_ResetsTurnSettled) doesn't have to mirror the
// exact mutex sequence inline — if SendBlocks's preamble
// evolves (e.g. adds a third flag), the test updates with it
// automatically.
func (d *driver) resetTurnState() {
	d.turnSettledMu.Lock()
	d.turnSettled = false
	d.turnSettledMu.Unlock()
}

// SessionView is the read-only handle the bridge-specific
// UpdateHandler receives. It exposes only the surface a translator
// needs (events channel for emission, session id for stamping)
// without leaking the unexported *driver or any of its locks /
// rpc plumbing.
//
// Lifetime: the SessionView is valid for the lifetime of the
// driver / *agent.Agent that produced it. Once Close() is called
// the underlying transport is gone and Emit becomes a closed-
// channel no-op (send completes silently via the close-aware
// select in deliver()).
type SessionView struct {
	// Emit blocks on the underlying events channel with the same
	// producer-side contract as the bridge's deliver() helper
	// (no instant drop, no timeout drop; close-aware).
	Emit func(ev agent.AgentEvent)

	// SessionID returns the negotiated ACP session id. Stable
	// across multi-turn conversations on the same driver; flips
	// to "" before handshake completes.
	SessionID func() string

	// AgentName / Workspace are captured at Start time. Immutable
	// for the driver's lifetime — exposed as strings (not funcs)
	// so callers can use them without a lock.
	AgentName string
	Workspace string

	// FlushPending requests the bridge-specific translator to flush
	// any buffered text it is holding onto before the next event is
	// emitted (typically EventAgentDone). A no-op when the translator
	// has nothing buffered or no flush handler is registered. Bound
	// to *driver.flushHandler by View() — kept on the view so callers
	// never have to type-assert on the driver.
	FlushPending func()
}


// UpdateHandler is the bridge-supplied callback that receives raw
// session/update params.update payloads. Returning a non-nil error
// is logged at debug level but does NOT kill the read pump — wire
// decoding stays tolerant so a malformed update from one agent
// version does not strand the user on a broken bridge.
//
// Bridges that want to translate sessionUpdate variants beyond
// the built-in 4-case fallback (agent_message_chunk, tool_call,
// tool_call_update, message_chunk) register a handler via
// SetUpdateHandler. The handler runs BEFORE the fallback, so it
// can fully replace the default behaviour for any kind it cares
// about (just return nil and don't re-emit if you want the
// fallback to also run).
type UpdateHandler func(view *SessionView, raw json.RawMessage) error

// FlushHandler is the bridge-supplied callback that drains any
// buffered text the translator is holding. Triggered on turn-end
// (before EventAgentDone) so a turn never silently drops the
// trailing fragment of text the agent already produced. The
// translator should keep state per *driver (or per agent.Agent)
// — the bridge guarantees the same handler runs against the
// same view it installed.
type FlushHandler func(view *SessionView)

// MethodHandler is the bridge-supplied callback that receives
// unrecognized JSON-RPC methods from the ACP server. The generic
// ACP bridge handles standard methods (session/update,
// tool_start, tool_end, etc.); any method not in that set is
// forwarded to this handler if installed. The handler receives
// the method name, raw params, and an optional respond function
// (non-nil when the method has an id, i.e. it expects a
// response). Return true if the method was handled (the bridge
// will not send a "method not found" error). Return false to
// let the bridge fall through to the default error response.
//
// The respond callback accepts either a result value or an error.
// If err is non-nil, the bridge sends a JSON-RPC error response;
// otherwise it sends the result value. For notifications (no id),
// respond is a no-op that returns true.
type MethodHandler func(method string, params json.RawMessage, respond func(id json.RawMessage, result any, err error) bool) bool


// DriverHandle is the exported alias for the package-private
// driver. Bridges that need to install an UpdateHandler (opencode,
// future per-bridge translators) reach the driver through this
// type via:
//
//	d, ok := a.Driver().(*acp.DriverHandle)
//
// and then call d.SetUpdateHandler(...) or d.View(). The type is
// opaque to consumers; it exists purely so bridge-specific code
// outside the acp package can perform the type assertion without
// re-exporting the unexported driver struct.
type DriverHandle = driver


// permissionCall tracks an outstanding session/request_permission
// request so SendPermission can route the answer back to the right
// JSON-RPC call.
type permissionCall struct {
	id     json.RawMessage
	legacy bool
}

// initializeParams / newSessionParams / promptParams / contentBlock /
// clientInfo are the ACP request payload shapes — package-private and
// only used by Start + Send* methods on Agent.
type initializeParams struct {
	ProtocolVersion    int            `json:"protocolVersion"`
	ClientCapabilities map[string]any `json:"clientCapabilities,omitempty"`
	ClientInfo         clientInfo     `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type newSessionParams struct {
	CWD        string `json:"cwd"`
	MCPServers []any  `json:"mcpServers"`
}

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

// sessionCancelParams — fix-stop: structured in-flight cancel.
//
// ACP's protocol exposes session/cancel as the canonical way to
// halt a prompt without killing the agent process. SIGINT was the
// pre-fix fallback (via transport.Signal), but on a PTY-backed
// ACP server it depends on every agent implementation actually
// handling SIGINT the same way — some agents exit, some
// translate it to a structured cancel, some ignore it. Routing
// through session/cancel gives us the same in-band protocol
// guarantee as codex's turn/interrupt: the agent stays alive,
// settles the in-flight turn cleanly, and the chat layer's
// TryFlush picks up the next queued prompt on the SAME sessionId.
//
// Method-not-found on older agents (-32601) falls back to SIGINT
// to preserve the pre-fix behaviour on agents that haven't
// shipped session/cancel yet.
type sessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// newDriver is invoked from starter.Start. It spawns the CLI under
// a PTY, runs the ACP initialize + session/new handshake, and
// returns a fully-wired *driver. Args are the command's protocol
// flags (e.g. the ACP server flag); passed in from the Starter.
func newDriver(ctx context.Context, s *Starter, cfg agent.StartConfig) (*driver, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cols, rows := s.cols, s.rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	// arg order: agent defaults, then user overrides (user wins).
	args := append([]string(nil), s.args...)
	args = append(args, cfg.Args...)
	// env order: agent defaults, then per-session overrides (cfg wins).
	env := append([]string(nil), s.env...)
	env = append(env, cfg.Env...)

	transport, err := pty.NewTransport(cfg.Workspace, s.command, args, env, cols, rows)
	if err != nil {
		return nil, err
	}

	parentCtx, cancel := context.WithCancel(ctx)
	live := &driver{
		transport:      transport,
		rpc:            newRPCClient(transport),
		ctx:            parentCtx,
		cancel:         cancel,
		agentName:      s.name,
		workspace:      cfg.Workspace,
		events:         make(chan agent.AgentEvent, eventBufferSize),
		textBuf:        &strings.Builder{},
		thoughtBuf:     &strings.Builder{},
		thinkingPrefix: "[思考] ",
	}
	// readPump is the per-session long-lived read loop. Wrap in
	// agent.SafeGo (outer, daemon-level safety net) +
	// agent.PanicEventHandler (inner, domain-level recovery
	// that emits EventAgentError) so a single acp bug
	// (translator panic, wire-decode panic) cannot take down
	// the nightme daemon AND the runtime can surface a
	// "bridge died" EventAgentError to the user. The 2026-
	// 08-15 dsh textBuf panic that motivated this pattern is
	// the prototype. See internal/agent/safego.go for the
	// contract.
	//
	// acp has no .deliver() method; we synthesize a closure
	// that does a non-blocking send (drop-on-full, drop-on-
	// closed) — same pattern as claudecode's panicDeliver.
	panicDeliver := func(ev agent.AgentEvent) {
		select {
		case live.events <- ev:
		case <-live.ctx.Done():
			// session cancelled; drop silently
		default:
		}
	}
	agent.SafeGo("acp:read-pump", func() {
		// PanicEventHandler uses live.sessionID (not "") so
		// the runtime can correlate the "bridge died" card
		// with the dying session. live.sessionID is populated
		// by handshake before readPump starts, so it is
		// non-empty by the time PanicEventHandler fires.
		// Note: defer close(live.events) lives inside readPump
		// itself (see readPump doc), so a panic inside the read
		// loop closes events BEFORE PanicEventHandler can
		// deliver — the notification is silently dropped. The
		// sessionID is still correctly stamped so a future
		// refinement (e.g. moving close to the wrapper) won't
		// regress correlation.
		defer agent.PanicEventHandler(
			"acp:read-pump", panicDeliver,
			live.sessionID, live.agentName, live.workspace, "")
		live.readPump()
	})
	go func() {
		<-parentCtx.Done()
		_ = live.Close()
	}()

	// Per-step timeout budgets live inside handshake() — see the
	// initializeTimeout / newSessionTimeout constants and the
	// handshake doc. newDriver does not apply an aggregate ceiling
	// here; the caller's parentCtx (which may already carry a
	// deadline from upstream) remains the source of early
	// cancellation, and each handshake phase has its own bounded
	// timeout for a clear, tunable failure mode.
	if err := live.handshake(parentCtx, cfg.Workspace); err != nil {
		_ = live.Close()
		return nil, err
	}
	return live, nil
}

// handshake runs the ACP initialize + session/new protocol exchange
// and seeds the synthesized EventAgentReady. Caller must have already
// populated live.rpc, live.ctx, live.events, live.agentName, and (for
// most callers) live.workspace.
//
// Timeout policy (split by phase):
//
//   - initialize:     initializeTimeout (3s). Tight sanity-check;
//     the server should reply with a protocol version within a
//     couple of seconds. Failing here means the bridge is dead or
//     the PTY transport is broken, so we want a fast, unambiguous
//     failure.
//   - session/new:    newSessionTimeout (45s). Generous — this is
//     where the ACP server does the expensive work. For opencode
//     `opencode acp` this includes booting an internal HTTP backend
//     and a parallel loadDirectorySnapshot (~5 HTTP calls against
//     providers/agents/commands/skills/config) before it can reply
//     to session/new. The previous shared 10s budget (startupTimeout)
//     was routinely too short on first-run CI or slow machines.
//
// Each phase creates its own deadline context; the parent ctx
// (passed in) still applies as an early-cancel path. Error
// messages now mention which phase and which timeout budget was
// hit, so the failure mode is visible without digging into code.
//
// Extracted from Start so tests using a mockTransport (no real PTY)
// can drive the handshake against an in-process net.Pipe server
// without going through pty.NewTransport.
func (d *driver) handshake(ctx context.Context, workspace string) error {
	initCtx, initCancel := context.WithTimeout(ctx, initializeTimeout)
	defer initCancel()
	if _, err := d.rpc.request(initCtx, "initialize", initializeParams{
		ProtocolVersion: protocolVersion,
		ClientCapabilities: map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		ClientInfo: clientInfo{
			Name:    clientName,
			Title:   "nightme (" + d.agentName + ")",
			Version: clientVersion,
		},
	}); err != nil {
		return fmt.Errorf("bridge/acp: initialize (timeout=%s): %w", initializeTimeout, err)
	}

	newCtx, newCancel := context.WithTimeout(ctx, newSessionTimeout)
	defer newCancel()
	result, err := d.rpc.request(newCtx, "session/new", newSessionParams{
		CWD:        workspace,
		MCPServers: []any{},
	})
	if err != nil {
		return fmt.Errorf("bridge/acp: session/new (timeout=%s): %w", newSessionTimeout, err)
	}
	if err := d.setSessionID(result); err != nil {
		return err
	}
	return nil
}

// ─── live-half methods (valid only between Start and Close) ───

func (d *driver) Events() <-chan agent.AgentEvent { return d.events }

func (d *driver) PID() int {
	if d.transport == nil {
		return 0
	}
	return d.transport.PID()
}

// SendBlocks delivers a structured user turn. ACP's content-block
// protocol supports text + image + file natively; the bridge
// translates agent.ContentBlock values into the wire shape. Today
// only Text is exercised by production agents (Codex / OpenCode
// have not yet landed), so the type-safe Path-based blocks are
// preserved here for Phase 2.
//
// Synchronous semantics
//
// Unlike a stream-json bridge (claudecode), this bridge awaits
// the session/prompt RESPONSE before returning. Rationale:
//
//   - Opencode ACP's session/prompt is server-side synchronous:
//     the server resolves the response only after the turn
//     settles (it internally awaits sdk.global.event for
//     session.status:idle).
//   - The response carries stopReason + per-turn usage that the
//     runtime reads from EventAgentDone.Usage. Awaiting lets us
//     deliver Done with accurate usage on the same event.
//   - The runtime's pump already drains sessionUpdate events
//     while SendBlocks is blocked, so no events are buffered or
//     dropped — the user sees the same text/tool stream either
//     way; the only difference is SendBlocks returns at turn-end
//     instead of immediately after writing the prompt.
//
// ErrTurnBusy is returned when a previous turn is still in
// flight. Mirrors codex / pi / claudecode / opencode-serve's
// pending-turn guard.
func (d *driver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	if d.sessionID == "" {
		return errors.New("bridge/acp: session is not initialized")
	}
	if len(blocks) == 0 {
		return nil
	}

	// pendingTurnActive guard — refuse a second prompt while
	// the first is still in flight. Released in the deferred
	// block below on every return path.
	d.pendingTurnMu.Lock()
	if d.pendingTurnActive {
		d.pendingTurnMu.Unlock()
		return ErrTurnBusy
	}
	d.pendingTurnActive = true
	d.pendingTurnMu.Unlock()

	// Reset the turn-end dedup flag for the new turn. The previous
	// turn's terminal paths (translatePromptResponse success /
	// cancelled / max_tokens / refusal, handleSessionStatus, the
	// unparseable-response fallback) set turnSettled=true; without
	// an explicit reset here, the new turn would see turnSettled=true
	// and silently skip its own EventAgentDone emit — the busy guard
	// would never clear and the spinner would hang forever (P0).
	//
	// Reset happens AFTER the busy-guard acquisition so a second
	// SendBlocks call that gets ErrTurnBusy does not clear turn 1's
	// dedup state out from under it.
	d.resetTurnState()

	defer func() {
		d.pendingTurnMu.Lock()
		d.pendingTurnActive = false
		d.pendingTurnMu.Unlock()
	}()

	out := make([]contentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			if b.Text == "" {
				continue
			}
			out = append(out, contentBlock{Type: "text", Text: b.Text})
		case agent.ContentImage, agent.ContentFile:
			// Phase 2: encode as proper ACP image/file blocks. For
			// now, fall back to a "@<path>" annotation so the agent
			// can read the file via its tools.
			if b.Path == "" {
				continue
			}
			out = append(out, contentBlock{
				Type: "text",
				Text: "@" + b.Path,
			})
		default:
			continue
		}
	}
	if len(out) == 0 {
		return nil
	}

	result, err := d.rpc.request(ctx, "session/prompt", promptParams{
		SessionID: d.sessionID,
		Prompt:    out,
	})
	if err != nil {
		// Surface the wire error as EventAgentError so the
		// runtime can render a "bridge rejected prompt" card
		// instead of just hanging on the busy guard.
		d.emit(agent.AgentEvent{
			Kind: agent.EventAgentError,
			Err:  fmt.Errorf("bridge/acp: session/prompt: %w", err),
		})
		return err
	}

	// Translate the prompt response into EventAgentDone +
	// EventAgentError (depending on stopReason). The acp spec
	// response shape (per @agentclientprotocol/sdk types):
	//
	//   {
	//     "stopReason": "end_turn" | "cancelled" | "max_tokens" |
	//                  "refusal" | string,
	//     "usage": { "inputTokens": int, "outputTokens": int,
	//                "cacheReadInputTokens": int,
	//                "cacheCreationInputTokens": int }?,
	//     "_meta": {...}
	//   }
	//
	// Bridges without a stopReason (older acp spec) silently
	// emit Done{Reason:"settled"} — safe default for the
	// common-case end_turn path.
	d.translatePromptResponse(result)
	return nil
}

// translatePromptResponse parses the session/prompt response and
// emits the terminal AgentEvent for the turn. Stops on
// EventAgentDone for normal completion, EventAgentError for
// cancelled / refused / max_tokens. Per-turn usage rides on
// Done.Usage (matches codex / pi / claudecode convention).
//
// Exposed as a method so future bridges can override (e.g. a
// bridge whose prompt response carries additional fields we
// want to surface).
func (d *driver) translatePromptResponse(raw json.RawMessage) {
	var resp struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			InputTokens              int `json:"inputTokens"`
			OutputTokens             int `json:"outputTokens"`
			CacheReadInputTokens     int `json:"cacheReadInputTokens"`
			CacheCreationInputTokens int `json:"cacheCreationInputTokens"`
		} `json:"usage,omitempty"`
		UserMessageID string `json:"userMessageId,omitempty"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		// Unparseable response — emit Done with reason
		// "settled" so the runtime clears the busy guard.
		// Without this the user is stuck on the spinner.
		d.turnSettledMu.Lock()
		already := d.turnSettled
		d.turnSettled = true
		d.turnSettledMu.Unlock()
		if !already {
			d.deliver(agent.AgentEvent{
				Kind: agent.EventAgentDone,
				Done: &agent.AgentDoneEvent{Reason: "settled"},
			})
		}
		return
	}

	// Prefer the usage_update snapshot (last-mile accuracy — it
	// reflects the model that actually ran, including any post-
	// prompt switches). Fall back to the prompt-response payload
	// when the server doesn't emit usage_update (some ACP servers
	// only include usage in the synchronous session/prompt reply).
	// Also clear lastUsage after consuming so the next turn
	// doesn't inherit stale tokens.
	d.lastUsageMu.Lock()
	usage := d.lastUsage
	d.lastUsage = nil
	d.lastUsageMu.Unlock()
	if usage == nil && resp.Usage != nil {
		usage = &agent.UsageInfo{
			InputTokens:              resp.Usage.InputTokens,
			OutputTokens:             resp.Usage.OutputTokens,
			CacheCreationInputTokens: resp.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     resp.Usage.CacheReadInputTokens,
		}
	}

	switch resp.StopReason {
	case "cancelled":
		// /stop path. CLI is alive; next SendBlocks will
		// succeed. Emit Error so the runtime can render a
		// "turn cancelled" footer instead of a normal
		// completed card.
		// Flush built-in buffers first, then bridge-specific buffers
		// so a cancelled turn does not silently drop trailing fragments.
		d.flushTextBuffers()
		if h := d.flushHandler.Load(); h != nil {
			(*h)(d.View())
		}
		// Bump turnSettled so a trailing session.status:idle
		// (opencode fires both prompt response + status:idle
		// for the same turn) doesn't synthesise a Done card
		// after the Error. The turn IS terminal here; the
		// next SendBlocks will reset the flag via New() or
		// pendingTurnMu release path.
		d.turnSettledMu.Lock()
		d.turnSettled = true
		d.turnSettledMu.Unlock()
		d.deliver(agent.AgentEvent{
			Kind: agent.EventAgentError,
			Err:  errors.New("bridge/acp: turn cancelled"),
		})
	case "max_tokens":
		d.flushTextBuffers()
		if h := d.flushHandler.Load(); h != nil {
			(*h)(d.View())
		}
		// Bump turnSettled so a trailing session.status:idle
		// (opencode fires both prompt response + status:idle
		// for the same turn) doesn't synthesise a Done card
		// after the Error. Original behaviour (pre-fix) only
		// emitted Error here; we preserve that.
		d.turnSettledMu.Lock()
		d.turnSettled = true
		d.turnSettledMu.Unlock()
		d.deliver(agent.AgentEvent{
			Kind: agent.EventAgentError,
			Err:  errors.New("bridge/acp: turn exceeded max_tokens"),
		})
	case "refusal":
		d.flushTextBuffers()
		if h := d.flushHandler.Load(); h != nil {
			(*h)(d.View())
		}
		d.turnSettledMu.Lock()
		d.turnSettled = true
		d.turnSettledMu.Unlock()
		d.deliver(agent.AgentEvent{
			Kind: agent.EventAgentError,
			Err:  errors.New("bridge/acp: turn refused by content filter"),
		})
	default:
		// end_turn OR unknown stopReason → success path.
		// Emitting Done{Reason:"settled"} lets the runtime
		// clear the busy guard via pump_events, identical to
		// how the opencode-serve bridge's session.status:idle
		// translation worked.
		//
		// Flush built-in buffers first, then bridge-specific buffers,
		// so the final send_card carries the whole accumulated reply
		// rather than just the chunked preview. Without this the
		// turn-end silently swallows whatever text the buffer is
		// still holding (the trailing fragment after the last
		// agent_message_chunk).
		d.flushTextBuffers()
		if h := d.flushHandler.Load(); h != nil {
			(*h)(d.View())
		}
		// turnSettled guards against the same turn also firing
		// session.status:idle (opencode does this). First
		// signal wins; handleSessionStatus sees turnSettled=true
		// and skips its own EventAgentDone emit.
		d.turnSettledMu.Lock()
		already := d.turnSettled
		d.turnSettled = true
		d.turnSettledMu.Unlock()
		if !already {
			done := &agent.AgentDoneEvent{Reason: "settled", Usage: usage}
			d.deliver(agent.AgentEvent{Kind: agent.EventAgentDone, Done: done})
		}
	}
}

// SendPermission replies to the oldest outstanding
// session/request_permission request. ACP represents the selected
// option as the optionId supplied by the agent; callers pass that
// opaque value through unchanged.
func (d *driver) SendPermission(response string) error {
	d.permissionMu.Lock()
	if len(d.permissions) == 0 {
		d.permissionMu.Unlock()
		return errors.New("bridge/acp: no pending permission request")
	}
	call := d.permissions[0]
	d.permissions = d.permissions[1:]
	d.permissionMu.Unlock()

	if call.legacy {
		return d.rpc.requestAsync("permission_response", map[string]any{
			"response": response,
		})
	}
	return d.rpc.respond(call.id, map[string]any{
		"outcome": map[string]any{
			"outcome":  "selected",
			"optionId": response,
		},
	}, nil)
}

// New resets the conversation context on the running session without
// terminating the underlying transport. F-34 §3.2.3: ACP's
// `session/new` JSON-RPC creates a fresh session id on the same
// transport; subsequent `session/prompt` calls route to the new
// session. The transport process stays alive; Events() stays open;
// PID stays the same.
//
// Re-arms connectedSent so emitConnected fires again with the new
// sessionId, letting the runtime's AgentEventBus subscriber capture
// it via SetResumeID (cmd/nightme/run.go newEventHandler).
// Reset is the agent.driver interface name for New. Implements
// the agent.driver Reset method (F-34).
func (d *driver) Reset(ctx context.Context) error { return d.New(ctx) }

func (d *driver) New(ctx context.Context) error {
	if d.transport == nil {
		return errors.New("bridge/acp: nil transport")
	}
	newCtx, cancel := context.WithTimeout(ctx, newSessionTimeout)
	defer cancel()
	result, err := d.rpc.request(newCtx, "session/new", newSessionParams{
		CWD:        d.workspace,
		MCPServers: []any{},
	})
	if err != nil {
		return fmt.Errorf("bridge/acp: session/new: %w", err)
	}
	// Re-arm connectedSent so emitConnected fires again with the
	// new id. We reuse permissionMu (which already serializes
	// connectedSent writes through setSessionID/emitConnected) as
	// a memory barrier.
	d.permissionMu.Lock()
	d.connectedSent = false
	d.permissionMu.Unlock()
	if err := d.setSessionID(result); err != nil {
		return err
	}
	return nil
}

// Stop requests the in-flight prompt to settle by sending a
// structured session/cancel JSON-RPC. The ACP server STAYS ALIVE
// and finishes the current turn; the bridge's normal event stream
// surfaces the settled state, the chat layer's TryFlush picks up
// the next queued prompt on the SAME sessionId (no respawn, no
// --resume, no ghost turn).
//
// Pre-fix Stop sent SIGINT via transport.Signal — relying on
// every agent implementation to interpret SIGINT the same way
// when running under a PTY. That works for some agents and breaks
// for others (some exit, some ignore it, some translate it to
// an unstructured stream event the bridge can't tell apart from
// a crash). session/cancel is the documented ACP method, so the
// behaviour is now uniform.
//
// State machine:
//
//   - transport not started              → ErrNotSupported
//   - sessionID empty (handshake failed) → noop (matches
//                                          stop.go's "nothing
//                                          to stop" branch)
//   - sessionID set                      → session/cancel
//   - session/cancel fails -32601        → SIGINT (legacy
//     (method not found)                  fallback for old
//                                          agents)
//   - session/cancel fails otherwise     → return the wire
//                                          error so the chat
//                                          layer can render
//                                          "stop failed"
//
// Stop is fire-and-forget: it does NOT block waiting for the
// agent to confirm the prompt has settled. The chat layer
// coordinates the next-submit transition via the existing
// KindPromptEnded / EventAgentDone handler.
func (d *driver) Stop(ctx context.Context) error {
	if d.transport == nil {
		return agent.ErrNotSupported
	}
	if d.sessionID == "" {
		// No session — there's nothing to cancel. Mirror
		// stop.go's noop semantics so a second /stop in quick
		// succession doesn't surface a spurious failure.
		return nil
	}

	stopCtx, cancel := context.WithTimeout(ctx, stopRPCTimeout)
	defer cancel()

	_, err := d.rpc.request(stopCtx, "session/cancel", sessionCancelParams{
		SessionID: d.sessionID,
	})
	if err == nil {
		// The agent's normal event stream will deliver the
		// settled state (tool_end / session_end / message_chunk
		// end-of-stream depending on the agent's protocol
		// flavour). The chat layer's TryFlush picks up the
		// next queued prompt once the bridge's prompt-end
		// handler flips IsReady.
		return nil
	}
	if !isMethodNotFound(err) {
		return fmt.Errorf("acp: stop: %w", err)
	}

	// Last-resort SIGINT fallback for agents that pre-date the
	// session/cancel method. The pre-fix path was always
	// SIGINT, so this preserves old behaviour for old agents
	// rather than regressing them.
	return d.transport.Signal(os.Interrupt)
}

// stopRPCTimeout bounds how long Stop() waits for the agent to
// acknowledge session/cancel. The wire reply is `null` and
// typically lands in <50ms; 3s is generous and matches
// codex's stopRPCTimeout.
const stopRPCTimeout = 3 * time.Second

// isMethodNotFound reports whether err corresponds to the
// JSON-RPC 2.0 reserved error -32601 (Method not found). Older
// ACP agents that pre-date the session/cancel method return
// this; we treat it as a soft signal to fall back to SIGINT
// rather than surface a "stop failed" reply to the user.
//
// Match order:
//  1. *rpcError with Code == -32601 (the wire code verbatim).
//  2. Otherwise, a substring match for "method not found" —
//     only when no *rpcError is in the chain, so a well-formed
//     -32600 invalid-request error that happens to mention
//     "method not found" in its body is NOT mis-classified.
func isMethodNotFound(err error) bool {
	if err == nil {
		return false
	}
	var re *rpcError
	if errors.As(err, &re) {
		return re.Code == -32601
	}
	return strings.Contains(err.Error(), "method not found")
}

// Close terminates the session by cancelling the per-session ctx
// and closing the underlying transport. Idempotent.
func (d *driver) Close() error {
	var err error
	d.closeOnce.Do(func() {
		// Closing the transport is the portable ACP shutdown
		// operation. A JSON-RPC notification could block on an
		// uncooperative PTY peer, so cleanup never waits for an
		// optional server acknowledgement.
		d.cancel()
		if d.transport != nil {
			err = d.transport.Close()
		}
	})
	return err
}

// Keepalive is the driver.Keepalive implementation for the
// acp bridge. acp speaks JSON-RPC over an arbitrary Transport
// (PTY / stdio / WebSocket) — there's no canonical OS PID
// for the upstream SDK host. liveness is inferred from the
// transport state: nil transport means we've been Closed
// (or never Started). When acp's transport gets an IsAlive
// method (planned for the next ACP SDK bump), tighten this
// check with a real reachability probe.
//
// On dead state we invoke onRecover so the chat layer can
// dial the upstream ACP host fresh and replay the saved
// session id. See agent.driver.Keepalive for the full contract.
func (d *driver) Keepalive(ctx context.Context, onRecover func(context.Context) error) error {
	if d.transport == nil {
		if onRecover == nil {
			return fmt.Errorf("acp: transport nil and no recovery callback")
		}
		return onRecover(ctx)
	}
	return nil
}

// ─── per-bridge extension hooks ──────────────────────────────────

// SetUpdateHandler installs a per-bridge translator for
// VENDOR-PRIVATE protocol extensions that the generic ACP
// fallback does not cover. The handler receives raw
// session/update params.update payloads BEFORE the built-in
// fallback runs.
//
// USE THIS FOR: vendor-private sessionUpdate kinds or
// JSON-RPC methods that are NOT in the ACP spec and NOT
// covered by the generic fallback. Examples:
//   - cursor's cursor/update_todos (JSON-RPC method, routed
//     via SetMethodHandler instead — see below)
//   - vendor-private slash-command notifications
//   - any non-standard sessionUpdate shape a future CLI
//     might emit that the spec doesn't mandate
//
// DO NOT USE THIS FOR: anything the ACP spec or its common
// vendor extensions define. The generic fallback in
// handleSessionUpdate now covers the standard surface —
// usage_update, session.status, session_info_update,
// agent_message_chunk, agent_thought_chunk, tool_call,
// tool_call_update, message_chunk. New ACP-spec kinds should
// be added to the fallback, NOT to a per-bridge translator.
//
// Lifetime: must be called BEFORE the readPump observes the
// first session/update. The runtime sets the handler from
// `Starter.Start`'s post-handshake window. Calling after the
// read pump has started racing updates is racy (some updates
// may go to the old handler or the fallback). Stored as
// atomic.Pointer so the writer / reader race is lock-free.
//
// All-or-nothing ownership: a non-nil handler replaces the
// built-in fallback wholesale (no partial override) to prevent
// double-emission on kinds the handler handles. Pass nil to
// revert to the built-in fallback.
//
// For unrecognized JSON-RPC METHODS (not sessionUpdate kinds),
// use SetMethodHandler instead — same lifetime contract, same
// "vendor-private only" semantic.
func (d *driver) SetUpdateHandler(h UpdateHandler) {
	if h == nil {
		d.updateHandler.Store(nil)
		return
	}
	d.updateHandler.Store(&h)
	// All-or-nothing ownership of sessionUpdate translation: a
	// non-nil handler means the bridge takes over the full sessionUpdate
	// stream and the built-in 4-case fallback in handleSessionUpdate
	// stays out of the way. Otherwise the same chunks would be
	// emitted twice (once by the handler, once by the fallback).
	// Pass nil to revert.
}

// SetFlushHandler installs the "flush buffered text" callback used at
// turn boundaries. Mirrors SetUpdateHandler's lifetime contract —
// safe to call once after Start returns. A nil handler clears the
// hook (the generic acp bridge still tolerates it; FlushPending
// becomes a no-op and the final flush before EventAgentDone is
// skipped when no handler is installed).
func (d *driver) SetFlushHandler(h FlushHandler) {
	if h == nil {
		d.flushHandler.Store(nil)
		return
	}
	d.flushHandler.Store(&h)
}

// SetMethodHandler installs a callback that receives unrecognized
// JSON-RPC methods from the ACP server. Used by per-agent bridges
// to handle agent-specific extension methods (cursor/update_todos,
// cursor/create_plan, etc.). Safe to call once after Start returns.
// Pass nil to revert to the default "method not found" behaviour.
func (d *driver) SetMethodHandler(h MethodHandler) {
	if h == nil {
		d.methodHandler.Store(nil)
		return
	}
	d.methodHandler.Store(&h)
}

// View returns a SessionView for use by bridge-supplied
// UpdateHandlers. The view's closures are bound to this driver
// and remain valid until Close. Emit / SessionID remain safe to
// call after Close (Emit's send becomes a closed-channel no-op;
// SessionID returns the last-set value).
//
// Mostly useful for tests; production code in the opencode bridge
// constructs the SessionView inline so the closures can capture
// per-bridge state (agentName, workspace).
func (d *driver) View() *SessionView {
	return &SessionView{
		Emit:      d.emit,
		SessionID: func() string { return d.sessionID },
		AgentName: d.agentName,
		Workspace: d.workspace,
		FlushPending: func() {
			if h := d.flushHandler.Load(); h != nil {
				(*h)(d.View())
			}
		},
	}
}

// ─── internals ───

// setSessionID parses the session/new response and synthesizes the
// EventAgentReady so the runtime can capture the resume id uniformly
// with Claude Code / Pi. Model is captured later (via usage_update /
// session_info_update) and stamped on subsequent events by deliver();
// see emitConnected for the full rationale.
func (d *driver) setSessionID(result json.RawMessage) error {
	var response struct {
		SessionID      string `json:"sessionId"`
		SessionIDSnake string `json:"session_id"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return fmt.Errorf("bridge/acp: decode session/new response: %w", err)
	}
	d.sessionID = response.SessionID
	if d.sessionID == "" {
		d.sessionID = response.SessionIDSnake
	}
	if d.sessionID == "" {
		return errors.New("bridge/acp: session/new response has no sessionId")
	}
	// Synthesize an EventAgentReady. Idempotent via connectedSent.
	d.emitConnected()
	return nil
}

// emitConnected publishes a single EventAgentReady carrying the ACP
// session id. The send blocks until the consumer drains (or ctx is
// done). Sized to absorb a sustained backlog — same producer-side
// contract as pi/claudecode/pty.
func (d *driver) emitConnected() {
	if d.connectedSent {
		return
	}
	d.connectedSent = true
	// Model is intentionally left blank here. ACP's initialize
	// and session/new responses do not carry a model name — it
	// only appears in vendor-extension sessionUpdate payloads
	// (usage_update.model, session_info_update.model) that fire
	// AFTER handshake. deliver() stamps d.model on every event,
	// so once the model is captured the runtime sees it on the
	// next Text / Tool / Result / Done event (see runtime/handler.go
	// SetModel capture path). The Empty Ready.Model is therefore
	// benign — runtime tolerates it (SetModel no-ops on empty).
	d.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: d.sessionID,
		AgentName: d.agentName,
		Workspace: d.workspace,
	})
}

func (d *driver) readPump() {
	// close(d.events) stays in readPump itself (NOT moved to
	// the SafeGo wrapper) because the test contract
	// (`TestAcpSession_EOFClosesEvents` calls readPump directly
	// without going through SafeGo and ranges a.events to EOF)
	// depends on readPump closing the channel on its own.
	//
	// The downside: a panic inside readPump fires this defer
	// BEFORE the wrapper's `defer agent.PanicEventHandler` can
	// deliver the bridge-died event — PanicEventHandler then
	// sees a closed channel and silently drops the notification
	// (panicDeliver's `select { case live.events <- ev: ...
	// default: }` picks default because send-on-closed is never
	// ready). This is a documented limitation of acp: only a
	// recovered panic during *normal* lifecycle (e.g. a panic
	// in handshake) gets a user-visible notification; a panic
	// inside the read loop does not. Code that wants the
	// "bridge died" card should panic BEFORE the read loop
	// starts (i.e. during handshake) — that's where the
	// bridgeless-prototype bug lives anyway, since handshake
	// panics happen before close(d.events) is reachable.
	defer close(d.events)

	scanner := bufio.NewScanner(d.transport)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		message, err := decodeRPCMessage([]byte(line))
		if err != nil {
			// PTY-backed ACP servers may print a startup banner
			// or terminal control sequence. JSON-RPC framing
			// remains authoritative.
			continue
		}
		if d.rpc.handleResponse(message) {
			continue
		}
		if message.Method != "" {
			d.handleMethod(message)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		d.emit(agent.AgentEvent{Kind: agent.EventAgentError, Err: err})
	}
	d.rpc.failPending(io.EOF)
	d.emit(agent.AgentEvent{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: -1}})
}

func (d *driver) handleMethod(message rpcMessage) {
	switch message.Method {
	case "session/update":
		d.handleSessionUpdate(message.Params)
	case "session/request_permission", "permission_request":
		d.handlePermission(message.ID, message.Params)
	case "message_chunk":
		var params struct {
			Text    string `json:"text"`
			Content string `json:"content"`
		}
		if json.Unmarshal(message.Params, &params) == nil {
			text := params.Text
			if text == "" {
				text = params.Content
			}
			d.emit(agent.AgentEvent{Kind: agent.EventAgentText, Text: text})
		}
	case "tool_start":
		d.handleToolStart(message.Params)
	case "tool_end":
		d.handleToolEnd(message.Params)
	case "session_end":
		d.emit(agent.AgentEvent{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 0}})
	case "initialize", "session/new", "session/prompt":
		// A PTY may echo client requests before the ACP server
		// disables terminal echo. They are outbound methods, not
		// server calls.
		return
	default:
		// Bridge-specific extension handler. If a bridge (cursor)
		// has installed a MethodHandler, give it first crack at
		// unrecognized methods before falling through to the
		// default "method not found" error.
		if h := d.methodHandler.Load(); h != nil {
			respond := func(id json.RawMessage, result any, rpcErr error) bool {
				if len(id) == 0 {
					return true // notification — no response needed
				}
				if rpcErr != nil {
					_ = d.rpc.respond(id, nil, &rpcError{Code: -32000, Message: rpcErr.Error()})
				} else {
					_ = d.rpc.respond(id, result, nil)
				}
				return true
			}
			if (*h)(message.Method, message.Params, respond) {
				return
			}
		}
		if len(message.ID) > 0 {
			_ = d.rpc.respond(message.ID, nil, &rpcError{Code: -32601, Message: "method not found"})
		}
	}
}

func (d *driver) handleSessionUpdate(raw json.RawMessage) {
	var params struct {
		Update json.RawMessage `json:"update"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return
	}
	var update struct {
		SessionUpdate string          `json:"sessionUpdate"`
		Type          string          `json:"type"`
		Content       json.RawMessage `json:"content"`
		ToolCallID    string          `json:"toolCallId"`
		Title         string          `json:"title"`
		Status        string          `json:"status"`
		RawInput      json.RawMessage `json:"rawInput"`
	}
	if json.Unmarshal(params.Update, &update) != nil {
		return
	}
	kind := update.SessionUpdate
	if kind == "" {
		kind = update.Type
	}

	// Per-bridge extension hook. If a bridge (e.g. opencode) has
	// installed an UpdateHandler, dispatch the raw update payload
	// to it BEFORE the built-in 4-case fallback runs. The handler
	// is responsible for emitting AgentEvents and stashing any
	//
	// Returning a non-nil error logs at debug level but does NOT
	// abort the read pump — wire decoding stays tolerant. The
	// handler is run on every sessionUpdate regardless of kind,
	// so a bridge that wants to fully replace the default must
	// either (a) re-emit the same AgentEvents the fallback would
	// have emitted, or (b) handle just the kinds it cares about
	// and let the fallback handle the rest by returning nil
	// without emitting. The opencode bridge picks option (b):
	// only the kinds the existing default does not handle
	// (usage_update, available_commands_update, plan, etc.) plus
	// the agent_thought_chunk variant the default treats as
	// plain text.
	if h := d.updateHandler.Load(); h != nil {
		view := d.View()
		if err := (*h)(view, params.Update); err != nil {
			// log only — keep the stream alive. Use the
			// standard slog package directly so we don't
			// pull in a per-package oLog helper (acp is
			// generic across backends; per-agent logging
			// belongs in the bridge-specific layer).
			slog.Debug("acp: updateHandler error (non-fatal)",
				"kind", kind, "err", err.Error())
		}
		// The handler now owns the full sessionUpdate stream — the
		// built-in fallback below stays out of the way so every
		// chunk is delivered exactly once. The all-or-nothing
		// ownership is intentional: a bridge installing an
		// UpdateHandler has full domain knowledge of its own server's
		// sessionUpdate variants and cannot leave any kind to the
		// generic fallback without risking a double-emit on the
		// kinds it does handle. (If a future bridge wants to extend
		// rather than replace the fallback, the right answer is to
		// wrap the fallback explicitly inside the bridge handler,
		// not to half-install here.)
		return
	}

	// ─── built-in text buffering ─────────────────────────────
	// Text chunks are buffered and flushed on sentence-terminating
	// punctuation (".?!。！？"), on tool boundaries, and at turn-end.
	// This gives all ACP bridges automatic sentence-level batching
	// without per-bridge UpdateHandler boilerplate.
	switch kind {
	case "agent_message_chunk":
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(update.Content, &content) == nil && content.Text != "" {
			d.textMu.Lock()
			d.textBuf.WriteString(content.Text)
			if endsWithSentencePunctuation(d.textBuf.String()) {
				d.textMu.Unlock()
				d.flushBuffer(d.thoughtBuf, d.thinkingPrefix)
				d.flushBuffer(d.textBuf, "")
			} else {
				d.textMu.Unlock()
			}
		}
	case "agent_thought_chunk":
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(update.Content, &content) == nil && content.Text != "" {
			d.textMu.Lock()
			d.thoughtBuf.WriteString(content.Text)
			if endsWithSentencePunctuation(d.thoughtBuf.String()) {
				d.textMu.Unlock()
				d.flushBuffer(d.textBuf, "")
				d.flushBuffer(d.thoughtBuf, d.thinkingPrefix)
			} else {
				d.textMu.Unlock()
			}
		}
	case "tool_call":
		// Tool boundary: flush both buffers so the user sees
		// in-progress content before the tool receipt appears.
		d.flushBuffer(d.thoughtBuf, d.thinkingPrefix)
		d.flushBuffer(d.textBuf, "")
		d.handleToolStart(params.Update)
	case "tool_call_update":
		d.handleToolEnd(params.Update)
	case "message_chunk":
		// Legacy message_chunk: buffer like agent_message_chunk.
		var content struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(update.Content, &content) == nil && content.Text != "" {
			d.textMu.Lock()
			d.textBuf.WriteString(content.Text)
			if endsWithSentencePunctuation(d.textBuf.String()) {
				d.textMu.Unlock()
				d.flushBuffer(d.thoughtBuf, d.thinkingPrefix)
				d.flushBuffer(d.textBuf, "")
			} else {
				d.textMu.Unlock()
			}
		}
	case "usage_update":
		// Per-turn token usage from the server's usage sessionUpdate.
		// Two common shapes:
		//   - opencode: { used, size, cost }   (cumulative "used" only)
		//   - ACP spec: { inputTokens, outputTokens, cacheRead, ... }
		// Both are accepted; standard shape wins when populated.
		// The snapshot is stashed into d.lastUsage and lands on the
		// turn-end EventAgentDone.Usage (translatePromptResponse
		// prefers lastUsage; falls back to response.Usage).
		d.handleUsageUpdate(params.Update)
		// usage_update is a status event, NOT a tool boundary —
		// do not flush text buffers here.
	case "session.status":
		// Vendor-extension turn-end signal (opencode acp sends
		// session.status:{status:"idle"} as a sessionUpdate
		// rather than resolving the synchronous session/prompt
		// response). Some ACP servers fire BOTH this AND the
		// prompt response; turnSettled in handleSessionStatus
		// dedupes so we only emit EventAgentDone once per turn.
		d.handleSessionStatus(params.Update)
	case "session_info_update":
		// Vendor-extension session metadata update. Currently
		// only the model field is consumed; title / description
		// are reserved for future use (e.g. /rename slash
		// command forwarding to the chat header).
		d.handleSessionInfoUpdate(params.Update)
	default:
		// Unknown kind: flush both buffers so trailing text is
		// not lost before the next recognized event.
		d.flushBuffer(d.thoughtBuf, d.thinkingPrefix)
		d.flushBuffer(d.textBuf, "")
	}
}

// handleUsageUpdate parses a sessionUpdate of kind "usage_update"
// and stashes the resulting UsageInfo into d.lastUsage. Two
// wire shapes are supported:
//
//   - opencode shape: { used, size, cost } — cumulative tokens
//     used in the model's context; "size" is the model's context
//     window. Mapped to InputTokens + ContextWindow + pct.
//   - ACP-spec shape: { inputTokens, outputTokens, cacheRead, ...,
//     costUSD, model } — full per-turn breakdown. Used verbatim.
//
// The model's `model` field (when present) is also captured into
// d.model so deliver() stamps it on every subsequent event.
//
// All-zero payloads are treated as "no usage reported" and do
// NOT clear a previously-stashed snapshot (the most-recent
// non-zero wins).
func (d *driver) handleUsageUpdate(raw json.RawMessage) {
	var u struct {
		// opencode shape (docs/feat/F-OPENCODE-ACP-MIGRATION §3.1).
		Used int64   `json:"used"`
		Size int64   `json:"size"`
		Cost float64 `json:"cost"`

		// ACP-spec shape — permissive so both flavours parse.
		InputTokens              int     `json:"inputTokens"`
		OutputTokens             int     `json:"outputTokens"`
		CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
		CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
		TotalTokens              int     `json:"totalTokens"`
		CostUSD                  float64 `json:"costUSD"`

		// vendor-extension model discovery (some servers
		// include the active model on usage_update).
		Model string `json:"model"`
	}
	// json.Unmarshal error → all zero → no-op (matches
	// claudecode decodeUsage's permissive style).
	_ = json.Unmarshal(raw, &u)

	info := &agent.UsageInfo{}
	// Standard ACP shape wins when populated (more granular
	// input/output/cache split).
	if u.InputTokens+u.OutputTokens+u.CacheReadInputTokens+
		u.CacheCreationInputTokens > 0 || u.TotalTokens > 0 {
		info.InputTokens = u.InputTokens
		info.OutputTokens = u.OutputTokens
		info.CacheReadInputTokens = u.CacheReadInputTokens
		info.CacheCreationInputTokens = u.CacheCreationInputTokens
		info.CostUSD = u.CostUSD
	} else if u.Used > 0 || u.Size > 0 || u.Cost > 0 {
		// opencode fallback: lump "used" into InputTokens
		// (opencode reports cumulative context usage, not the
		// per-turn input/output breakdown).
		info.InputTokens = int(u.Used)
		info.CostUSD = u.Cost
	}
	// ContextWindow + pct: opencode gives Size directly; for
	// the standard shape we don't recompute (consistent with
	// claudecode: pct is bridge-local, only computed when the
	// wire reports the window).
	if u.Size > 0 {
		info.ContextWindow = int(u.Size)
		if info.InputTokens+info.OutputTokens+
			info.CacheCreationInputTokens+info.CacheReadInputTokens > 0 {
			used := info.InputTokens + info.OutputTokens +
				info.CacheCreationInputTokens + info.CacheReadInputTokens
			info.ContextWindowPct = float64(used) / float64(u.Size) * 100
		}
	}
	// All-zero payloads do NOT clear a previously-stashed
	// snapshot — the most-recent non-zero wins. This protects
	// against servers that emit a final zero-cost usage_update
	// as a "stream end" marker.
	if info.InputTokens+info.OutputTokens+
		info.CacheCreationInputTokens+info.CacheReadInputTokens > 0 ||
		info.CostUSD > 0 || info.ContextWindow > 0 {
		d.lastUsageMu.Lock()
		d.lastUsage = info
		d.lastUsageMu.Unlock()
	}

	// Model capture (vendor-extension field). Write protected by
	// modelMu — readers in deliver() can run on any goroutine
	// (see model field doc).
	if u.Model != "" {
		d.modelMu.Lock()
		d.model = u.Model
		d.modelMu.Unlock()
	}
}

// handleSessionStatus parses a sessionUpdate of kind
// "session.status" and, on status=idle, treats it as a turn-end
// signal — flushes both buffers and emits EventAgentDone{Reason:
// "settled", Usage: d.lastUsage}.
//
// turnSettled guards against emitting EventAgentDone twice for
// the same turn when both session.status:idle AND the
// synchronous session/prompt response arrive (opencode acp does
// this; the prompt response path is translatePromptResponse).
//
// For error terminations (cancelled / max_tokens / refusal) the
// prompt-response path also bumps turnSettled so this handler
// won't synthesise a stray Done after an Error.
func (d *driver) handleSessionStatus(raw json.RawMessage) {
	var st struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &st) != nil || st.Status != "idle" {
		return
	}
	d.flushTextBuffers()
	if h := d.flushHandler.Load(); h != nil {
		(*h)(d.View())
	}
	// Critical sections are kept narrow and non-nested: take
	// turnSettledMu first for the dedup decision, release, then
	// take lastUsageMu for the stash clear. The two mutexes are
	// never held simultaneously, so future code that adds a third
	// mutex (e.g. for d.model) cannot deadlock against this
	// pattern.
	d.turnSettledMu.Lock()
	already := d.turnSettled
	d.turnSettled = true
	d.turnSettledMu.Unlock()
	d.lastUsageMu.Lock()
	usage := d.lastUsage
	d.lastUsage = nil
	d.lastUsageMu.Unlock()
	if !already {
		d.deliver(agent.AgentEvent{
			Kind: agent.EventAgentDone,
			Done: &agent.AgentDoneEvent{Reason: "settled", Usage: usage},
		})
	}
}

// handleSessionInfoUpdate parses a sessionUpdate of kind
// "session_info_update" and captures the vendor-extension model
// field (if present) into d.model. Other fields (title,
// description, ...) are reserved for future use and currently
// ignored — see docs/bridge/acp.md §2.1 for the full kind table.
func (d *driver) handleSessionInfoUpdate(raw json.RawMessage) {
	var info struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &info)
	if info.Model != "" {
		d.modelMu.Lock()
		d.model = info.Model
		d.modelMu.Unlock()
	}
}

func (d *driver) handlePermission(id json.RawMessage, raw json.RawMessage) {
	var params struct {
		Tool       json.RawMessage `json:"toolCall"`
		ToolLegacy string          `json:"tool"`
		Options    []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
		} `json:"options"`
		Action string `json:"action"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return
	}
	var tool struct {
		Title    string          `json:"title"`
		Name     string          `json:"name"`
		RawInput json.RawMessage `json:"rawInput"`
	}
	_ = json.Unmarshal(params.Tool, &tool)
	toolName := tool.Title
	if toolName == "" {
		toolName = tool.Name
	}
	if toolName == "" {
		toolName = params.ToolLegacy
	}
	options := make([]string, 0, len(params.Options))
	for _, option := range params.Options {
		if option.OptionID != "" {
			options = append(options, option.OptionID)
		} else {
			options = append(options, option.Name)
		}
	}
	d.permissionMu.Lock()
	d.permissions = append(d.permissions, permissionCall{id: append(json.RawMessage(nil), id...), legacy: len(id) == 0})
	d.permissionMu.Unlock()
	d.emit(agent.AgentEvent{Kind: agent.EventAgentPermission, Permission: &agent.AgentPermissionRequest{
		Tool: toolName, Action: params.Action, Options: options, ResponseCh: make(chan string, 1),
	}})
}

func (d *driver) handleToolStart(raw json.RawMessage) {
	var event struct {
		ID         string          `json:"toolCallId"`
		LegacyID   string          `json:"id"`
		Name       string          `json:"title"`
		LegacyName string          `json:"name"`
		Args       json.RawMessage `json:"rawInput"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	id := event.ID
	if id == "" {
		id = event.LegacyID
	}
	name := event.Name
	if name == "" {
		name = event.LegacyName
	}
	d.emit(agent.AgentEvent{Kind: agent.EventAgentToolStart, ToolStart: &agent.AgentToolStartEvent{ID: id, Name: name, Args: string(event.Args)}})
}

func (d *driver) handleToolEnd(raw json.RawMessage) {
	var event struct {
		ID         string `json:"toolCallId"`
		LegacyID   string `json:"id"`
		Name       string `json:"title"`
		LegacyName string `json:"name"`
		Status     string `json:"status"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	id := event.ID
	if id == "" {
		id = event.LegacyID
	}
	name := event.Name
	if name == "" {
		name = event.LegacyName
	}
	var toolErr error
	if event.Status == "failed" || event.Status == "error" {
		toolErr = errors.New(event.Status)
	}
	d.emit(agent.AgentEvent{
		Kind:    agent.EventAgentToolEnd,
		ToolEnd: &agent.AgentToolEndEvent{ID: id, Name: name},
		Err:     toolErr,
	})
}

// deliver is the single producer-side send path. It stamps the
// per-event context fields (SessionID / Model / AgentName /
// Workspace) on every event before sending, so downstream
// consumers (runtime, gateway, channel adapters) see a uniform
// AgentEvent envelope regardless of which bridge helper
// constructed it. Already-set fields are preserved; empty fields
// are filled from bridge-local state.
//
// Mirrors codex/agent.go::deliver() — the difference is that
// acp's per-event fields are mostly captured from handshake /
// usage_update / session_info_update rather than from explicit
// thread/start responses. Model may be empty when the server
// doesn't report one; runtime tolerates empty Model and the
// footer just omits the model segment.
//
// The send blocks on ctx.Done (no instant drop) — same contract
// as the pre-fix emit() and codex's deliver. emit()'s regression
// test (TestDeliver_NoInstantDrop, formerly TestEmit_NoInstantDrop)
// still pins this contract via the deliver wrapper below.
func (d *driver) deliver(ev agent.AgentEvent) agent.AgentEvent {
	if ev.SessionID == "" {
		ev.SessionID = d.sessionID
	}
	if ev.AgentName == "" {
		ev.AgentName = d.agentName
	}
	if ev.Workspace == "" {
		ev.Workspace = d.workspace
	}
	if ev.Model == "" {
		// Snapshot d.model under modelMu. handleUsageUpdate /
		// handleSessionInfoUpdate write on the readPump
		// goroutine; deliver() runs on any goroutine
		// (handshake, SendBlocks via translatePromptResponse,
		// flushBuffer). Race detector requires this.
		d.modelMu.Lock()
		ev.Model = d.model
		d.modelMu.Unlock()
	}
	select {
	case d.events <- ev:
	case <-d.ctx.Done():
	}
	return ev
}

// emit is kept as a void wrapper for callers that don't care
// about the return value (panicDeliver / historical call sites
// that pre-date the deliver refactor). New code should call
// deliver() directly so the stamped-and-returned event stays
// visible to the caller (useful when the caller wants to log it
// after the send completes).
func (d *driver) emit(ev agent.AgentEvent) {
	d.deliver(ev)
}

// ─── built-in text buffering helpers ──────────────────────────────

// flushBuffer drains `buf` into a single EventAgentText and clears
// it. No-op when the buffer is empty or all-whitespace. When `prefix`
// is non-empty (e.g. thinkingPrefix) it is prepended to the content
// so the gateway can route thinking payloads to the reasoning surface.
//
// The caller must NOT hold d.textMu when calling this — flushBuffer
// acquires the lock internally for the String/Reset/Emit sequence.
func (d *driver) flushBuffer(buf *strings.Builder, prefix string) {
	if buf == nil {
		return
	}
	d.textMu.Lock()
	text := strings.TrimSpace(buf.String())
	buf.Reset()
	d.textMu.Unlock()
	if text == "" {
		return
	}
	if prefix != "" {
		text = prefix + text
	}
	// deliver() stamps SessionID/AgentName/Workspace/Model
	// automatically — no need to fill them here.
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: text,
	})
}

// flushTextBuffers drains both the reply buffer (textBuf) and the
// reasoning buffer (thoughtBuf). Called at turn-end and on tool
// boundaries so no trailing text is silently dropped.
func (d *driver) flushTextBuffers() {
	d.flushBuffer(d.thoughtBuf, d.thinkingPrefix)
	d.flushBuffer(d.textBuf, "")
}

// endsWithSentencePunctuation reports whether s ends with a sentence-
// terminating punctuation mark — ASCII ".!?" or full-width
// "。！？" — after trimming trailing whitespace.
func endsWithSentencePunctuation(s string) bool {
	if s == "" {
		return false
	}
	for i := len(s) - 1; i >= 0; i-- {
		r, size := utf8.DecodeLastRuneInString(s[:i+1])
		if size == 0 {
			return false
		}
		if !unicode.IsSpace(r) {
			switch r {
			case '.', '?', '!', '。', '！', '？':
				return true
			default:
				return false
			}
		}
	}
	return false
}

// Compile-time guarantee that *driver satisfies the package-private
// agent.driver interface (SendBlocks/SendPermission/Reset/Close).
// External callers reach driver via *agent.Agent, which forwards
// the public methods. The package-private starter half is type-checked
// in starter.go via the same agentDriver interface declaration.
var _ agentDriver = (*driver)(nil)

// agentDriver is the local alias for the agent.driver interface so
// this file can compile-time check driver satisfies it without
// importing the unexported name from the agent package.
type agentDriver interface {
	SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error
	SendPermission(resp string) error
	Reset(ctx context.Context) error
	Close() error
}
