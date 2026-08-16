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

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
)

const (
	eventBufferSize = 40960
	startupTimeout  = 10 * time.Second
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
	updateHandler atomic.Pointer[UpdateHandler]

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
				transport: transport,
		rpc:       newRPCClient(transport),
		ctx:       parentCtx,
		cancel:    cancel,
		agentName: s.name,
		workspace: cfg.Workspace,
		events:    make(chan agent.AgentEvent, eventBufferSize),
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

	startupCtx, startupCancel := context.WithTimeout(parentCtx, startupTimeout)
	defer startupCancel()
	if err := live.handshake(startupCtx, cfg.Workspace); err != nil {
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
// Extracted from Start so tests using a mockTransport (no real PTY) can
// drive the handshake against an in-process net.Pipe server without
// going through pty.NewTransport.
func (d *driver) handshake(ctx context.Context, workspace string) error {
	if _, err := d.rpc.request(ctx, "initialize", initializeParams{
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
		return fmt.Errorf("bridge/acp: initialize: %w", err)
	}

	result, err := d.rpc.request(ctx, "session/new", newSessionParams{
		CWD:        workspace,
		MCPServers: []any{},
	})
	if err != nil {
		return fmt.Errorf("bridge/acp: session/new: %w", err)
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
		d.emit(agent.AgentEvent{
			Kind: agent.EventAgentDone,
			Done: &agent.AgentDoneEvent{Reason: "settled"},
		})
		return
	}

	// usage is optional — a successful turn with empty usage
	// emits Done without a Usage field (acceptable: channels
	// render zero-usage turns without a footer line).
	var usage *agent.UsageInfo
	if resp.Usage != nil {
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
		d.emit(agent.AgentEvent{
			Kind: agent.EventAgentError,
			Err:  errors.New("bridge/acp: turn cancelled"),
		})
	case "max_tokens":
		d.emit(agent.AgentEvent{
			Kind: agent.EventAgentError,
			Err:  errors.New("bridge/acp: turn exceeded max_tokens"),
		})
	case "refusal":
		d.emit(agent.AgentEvent{
			Kind: agent.EventAgentError,
			Err:  errors.New("bridge/acp: turn refused by content filter"),
		})
	default:
		// end_turn OR unknown stopReason → success path.
		// Emitting Done{Reason:"settled"} lets the runtime
		// clear the busy guard via pump_events, identical to
		// how the opencode-serve bridge's session.status:idle
		// translation worked.
		done := &agent.AgentDoneEvent{Reason: "settled", Usage: usage}
		d.emit(agent.AgentEvent{Kind: agent.EventAgentDone, Done: done})
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
	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	result, err := d.rpc.request(startupCtx, "session/new", newSessionParams{
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

// SetUpdateHandler installs a bridge-specific translator for
// session/update notifications. Subsequent updates route through
// the handler BEFORE the built-in 4-case fallback runs; the
// handler is responsible for emitting AgentEvents and may either
// fully replace the default behaviour (most common) or layer on
// top (rare; needed if a future acp spec adds a sessionUpdate
// variant that the fallback should still handle).
//
// Must be called BEFORE the readPump observes the first
// session/update. The runtime sets the handler from
// `Starter.Start`'s post-handshake window — see opencode bridge
// for the canonical pattern. Calling after the read pump has
// started racing updates is racy (some updates may go to the old
// handler or the fallback).
//
// Pass nil to revert to the built-in 4-case behaviour.
//
// Designed for bridge-specific translators that need to handle
// sessionUpdate variants the generic acp bridge does not
// recognize (opencode's agent_thought_chunk, future per-agent
// acp backends). The opencode bridge is the first user
// (F-OPENCODE-ACP-MIGRATION).
func (d *driver) SetUpdateHandler(h UpdateHandler) {
	if h == nil {
		d.updateHandler.Store(nil)
		return
	}
	d.updateHandler.Store(&h)
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
		Emit:       d.emit,
		SessionID:  func() string { return d.sessionID },
		AgentName:  d.agentName,
		Workspace:  d.workspace,
	}
}

// ─── internals ───

// setSessionID parses the session/new response and synthesizes the
// EventAgentReady so the runtime can capture the resume id uniformly
// with Claude Code / Pi.
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
	ev := agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: d.sessionID,
		AgentName: d.agentName,
		Workspace: d.workspace,
	}
	select {
	case d.events <- ev:
	case <-d.ctx.Done():
	}
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
	}

	switch kind {
	case "agent_message_chunk":
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(update.Content, &content) == nil && content.Text != "" {
			d.emit(agent.AgentEvent{Kind: agent.EventAgentText, Text: content.Text})
		}
	case "tool_call":
		d.handleToolStart(params.Update)
	case "tool_call_update":
		d.handleToolEnd(params.Update)
	case "message_chunk":
		var content struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(update.Content, &content) == nil {
			d.emit(agent.AgentEvent{Kind: agent.EventAgentText, Text: content.Text})
		}
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

func (d *driver) emit(event agent.AgentEvent) {
	select {
	case d.events <- event:
	case <-d.ctx.Done():
	}
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
