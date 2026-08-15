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
	"os"
	"strings"
	"sync"
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
}


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
		defer agent.PanicEventHandler(
			"acp:read-pump", panicDeliver,
			"", live.agentName, live.workspace, "")
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
func (d *driver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	_ = ctx
	if d.sessionID == "" {
		return errors.New("bridge/acp: session is not initialized")
	}
	if len(blocks) == 0 {
		return nil
	}
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
	return d.rpc.requestAsync("session/prompt", promptParams{
		SessionID: d.sessionID,
		Prompt:    out,
	})
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

// Stop sends SIGINT to the child process via the PTY transport.
// The child runs with a TTY, so SIGINT is natively interpreted
// the same way a user pressing Ctrl-C in interactive mode would
// be: cancel the in-flight turn, stay alive, await the next
// prompt. The ACP protocol surfaces the settled state via the
// bridge's normal event stream; the chat layer's TryFlush picks up
// the next queued prompt once IsReady() flips.
//
// Stop is fire-and-forget: it does NOT block waiting for the
// settle event.
//
// Returns ErrNotSupported if the transport is not started.
func (d *driver) Stop(ctx context.Context) error {
	_ = ctx
	if d.transport == nil {
		return agent.ErrNotSupported
	}
	return d.transport.Signal(os.Interrupt)
}

// SetModel is not supported on the ACP bridge. ACP has a
// session/set_mode method but no public session/set_model; the
// provider / model is fixed at session creation.
func (d *driver) SetModel(ctx context.Context, providerID, modelID string) error {
	_ = ctx
	_ = providerID
	_ = modelID
	return agent.ErrNotSupported
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
