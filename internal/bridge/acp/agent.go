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
	"os/exec"
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
// separate *Agent so concurrent Start calls from different chats do
// not interfere with each other.
type Agent struct {
	// ─── template fields (set by NewAgent; immutable) ───
	name    string
	command string
	args    []string
	env     []string
	cols    int
	rows    int

	// ─── runtime fields (zero before Start; populated on the clone) ───
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

// NewAgent constructs the template Agent. This is the entry point
// used at registration time (cmd/nightme/agents.go calls it from
// init()); the returned *Agent is held by agent.Builtins as the
// singleton for `name`.
//
// args are the command's protocol flags (e.g. the ACP server flag).
// Defensively copied.
func NewAgent(name, command string, args []string) *Agent {
	return &Agent{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
	}
}

// ─── spec-half methods (valid in any state) ───

func (a *Agent) Name() string { return a.name }

func (a *Agent) Mode() agent.Mode { return agent.ModeACP }

// Command returns the CLI binary the agent wraps. Surfaced by
// `nightme agents` so users can see what /run would spawn.
func (a *Agent) Command() string { return a.command }

// Args returns a defensive copy of the spawn recipe's default argv.
// Callers may not mutate the returned slice.
func (a *Agent) Args() []string {
	return append([]string(nil), a.args...)
}

// Env returns a defensive copy of the spawn recipe's default env.
// Callers may not mutate the returned slice.
func (a *Agent) Env() []string {
	return append([]string(nil), a.env...)
}

func (a *Agent) Detect() error {
	_, err := exec.LookPath(a.command)
	return err
}

// ─── lifecycle ───

// Start spawns the CLI under a PTY, runs the ACP initialize +
// session/new handshake, and returns a live Agent. The caller
// (typically chatsession.AgentSession via the Spawner) must Close()
// the returned *Agent when done.
//
// Start clones the receiver — the template in Builtins is untouched.
// The clone gets template fields copied (defensively), runtime
// fields zeroed, then NewTransport is called to spawn the PTY, the
// read pump is kicked off, and the JSON-RPC handshake runs
// synchronously before Start returns.
func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.Agent, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cols, rows := a.cols, a.rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	// arg order: agent defaults, then user overrides (user wins).
	args := append([]string(nil), a.args...)
	args = append(args, cfg.Args...)
	// env order: agent defaults, then per-session overrides (cfg wins).
	env := append([]string(nil), a.env...)
	env = append(env, cfg.Env...)

	transport, err := pty.NewTransport(cfg.Workspace, a.command, args, env, cols, rows)
	if err != nil {
		return nil, err
	}

	parentCtx, cancel := context.WithCancel(ctx)
	live := &Agent{
		name:      a.name,
		command:   a.command,
		args:      append([]string(nil), a.args...),
		env:       append([]string(nil), a.env...),
		cols:      cols,
		rows:      rows,
		transport: transport,
		rpc:       newRPCClient(transport),
		ctx:       parentCtx,
		cancel:    cancel,
		agentName: a.name,
		workspace: cfg.Workspace,
		events:    make(chan agent.AgentEvent, eventBufferSize),
	}
	go live.readPump()
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
func (a *Agent) handshake(ctx context.Context, workspace string) error {
	if _, err := a.rpc.request(ctx, "initialize", initializeParams{
		ProtocolVersion: protocolVersion,
		ClientCapabilities: map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		ClientInfo: clientInfo{
			Name:    clientName,
			Title:   "nightme (" + a.agentName + ")",
			Version: clientVersion,
		},
	}); err != nil {
		return fmt.Errorf("bridge/acp: initialize: %w", err)
	}

	result, err := a.rpc.request(ctx, "session/new", newSessionParams{
		CWD:        workspace,
		MCPServers: []any{},
	})
	if err != nil {
		return fmt.Errorf("bridge/acp: session/new: %w", err)
	}
	if err := a.setSessionID(result); err != nil {
		return err
	}
	return nil
}

// ─── live-half methods (valid only between Start and Close) ───

func (a *Agent) Events() <-chan agent.AgentEvent { return a.events }

func (a *Agent) PID() int {
	if a.transport == nil {
		return 0
	}
	return a.transport.PID()
}

// SendText submits a prompt and returns after the JSON-RPC request
// is written. The prompt response only marks completion of that turn;
// it does not end the reusable ACP session, so it is deliberately
// consumed asynchronously.
func (a *Agent) SendText(text string) error {
	if text == "" {
		return nil
	}
	if a.sessionID == "" {
		return errors.New("bridge/acp: session is not initialized")
	}
	return a.rpc.requestAsync("session/prompt", promptParams{
		SessionID: a.sessionID,
		Prompt:    []contentBlock{{Type: "text", Text: text}},
	})
}

// SendBlocks submits a structured prompt. ACP's content-block
// protocol supports text + image + file natively; the bridge
// translates agent.ContentBlock values into the wire shape. Today
// only Text is exercised by production agents (Codex / OpenCode
// have not yet landed), so the type-safe Path-based blocks are
// preserved here for Phase 2.
func (a *Agent) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	_ = ctx
	if a.sessionID == "" {
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
	return a.rpc.requestAsync("session/prompt", promptParams{
		SessionID: a.sessionID,
		Prompt:    out,
	})
}

// SendPermission replies to the oldest outstanding
// session/request_permission request. ACP represents the selected
// option as the optionId supplied by the agent; callers pass that
// opaque value through unchanged.
func (a *Agent) SendPermission(response string) error {
	a.permissionMu.Lock()
	if len(a.permissions) == 0 {
		a.permissionMu.Unlock()
		return errors.New("bridge/acp: no pending permission request")
	}
	call := a.permissions[0]
	a.permissions = a.permissions[1:]
	a.permissionMu.Unlock()

	if call.legacy {
		return a.rpc.requestAsync("permission_response", map[string]any{
			"response": response,
		})
	}
	return a.rpc.respond(call.id, map[string]any{
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
func (a *Agent) New(ctx context.Context) error {
	if a.transport == nil {
		return errors.New("bridge/acp: nil transport")
	}
	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	result, err := a.rpc.request(startupCtx, "session/new", newSessionParams{
		CWD:        a.workspace,
		MCPServers: []any{},
	})
	if err != nil {
		return fmt.Errorf("bridge/acp: session/new: %w", err)
	}
	// Re-arm connectedSent so emitConnected fires again with the
	// new id. We reuse permissionMu (which already serializes
	// connectedSent writes through setSessionID/emitConnected) as
	// a memory barrier.
	a.permissionMu.Lock()
	a.connectedSent = false
	a.permissionMu.Unlock()
	if err := a.setSessionID(result); err != nil {
		return err
	}
	return nil
}

// Abort sends SIGINT to the child process. ACP has no structured
// "cancel" method on the bridge's PTY-based transport, so the
// closest portable action is the same Ctrl-C signal a user would
// press. The user-facing semantic is "interrupt the in-flight
// turn" — the child interprets that as best it can.
func (a *Agent) Abort(ctx context.Context) error {
	_ = ctx
	if a.transport == nil {
		return agent.ErrNotSupported
	}
	return a.transport.Signal(os.Interrupt)
}

// SetModel is not supported on the ACP bridge. ACP has a
// session/set_mode method but no public session/set_model; the
// provider / model is fixed at session creation.
func (a *Agent) SetModel(ctx context.Context, providerID, modelID string) error {
	_ = ctx
	_ = providerID
	_ = modelID
	return agent.ErrNotSupported
}

// Close terminates the session by cancelling the per-session ctx
// and closing the underlying transport. Idempotent.
func (a *Agent) Close() error {
	var err error
	a.closeOnce.Do(func() {
		// Closing the transport is the portable ACP shutdown
		// operation. A JSON-RPC notification could block on an
		// uncooperative PTY peer, so cleanup never waits for an
		// optional server acknowledgement.
		a.cancel()
		if a.transport != nil {
			err = a.transport.Close()
		}
	})
	return err
}

// ─── internals ───

// setSessionID parses the session/new response and synthesizes the
// EventAgentReady so the runtime can capture the resume id uniformly
// with Claude Code / Pi.
func (a *Agent) setSessionID(result json.RawMessage) error {
	var response struct {
		SessionID      string `json:"sessionId"`
		SessionIDSnake string `json:"session_id"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return fmt.Errorf("bridge/acp: decode session/new response: %w", err)
	}
	a.sessionID = response.SessionID
	if a.sessionID == "" {
		a.sessionID = response.SessionIDSnake
	}
	if a.sessionID == "" {
		return errors.New("bridge/acp: session/new response has no sessionId")
	}
	// Synthesize an EventAgentReady. Idempotent via connectedSent.
	a.emitConnected()
	return nil
}

// emitConnected publishes a single EventAgentReady carrying the ACP
// session id. The send blocks until the consumer drains (or ctx is
// done). Sized to absorb a sustained backlog — same producer-side
// contract as pi/claudecode/pty.
func (a *Agent) emitConnected() {
	if a.connectedSent {
		return
	}
	a.connectedSent = true
	ev := agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: a.sessionID,
		AgentName: a.agentName,
		Workspace: a.workspace,
	}
	select {
	case a.events <- ev:
	case <-a.ctx.Done():
	}
}

func (a *Agent) readPump() {
	defer close(a.events)

	scanner := bufio.NewScanner(a.transport)
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
		if a.rpc.handleResponse(message) {
			continue
		}
		if message.Method != "" {
			a.handleMethod(message)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		a.emit(agent.AgentEvent{Kind: agent.EventAgentError, Err: err})
	}
	a.rpc.failPending(io.EOF)
	a.emit(agent.AgentEvent{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: -1}})
}

func (a *Agent) handleMethod(message rpcMessage) {
	switch message.Method {
	case "session/update":
		a.handleSessionUpdate(message.Params)
	case "session/request_permission", "permission_request":
		a.handlePermission(message.ID, message.Params)
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
			a.emit(agent.AgentEvent{Kind: agent.EventAgentText, Text: text})
		}
	case "tool_start":
		a.handleToolStart(message.Params)
	case "tool_end":
		a.handleToolEnd(message.Params)
	case "session_end":
		a.emit(agent.AgentEvent{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 0}})
	case "initialize", "session/new", "session/prompt":
		// A PTY may echo client requests before the ACP server
		// disables terminal echo. They are outbound methods, not
		// server calls.
		return
	default:
		if len(message.ID) > 0 {
			_ = a.rpc.respond(message.ID, nil, &rpcError{Code: -32601, Message: "method not found"})
		}
	}
}

func (a *Agent) handleSessionUpdate(raw json.RawMessage) {
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
			a.emit(agent.AgentEvent{Kind: agent.EventAgentText, Text: content.Text})
		}
	case "tool_call":
		a.handleToolStart(params.Update)
	case "tool_call_update":
		a.handleToolEnd(params.Update)
	case "message_chunk":
		var content struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(update.Content, &content) == nil {
			a.emit(agent.AgentEvent{Kind: agent.EventAgentText, Text: content.Text})
		}
	}
}

func (a *Agent) handlePermission(id json.RawMessage, raw json.RawMessage) {
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
	a.permissionMu.Lock()
	a.permissions = append(a.permissions, permissionCall{id: append(json.RawMessage(nil), id...), legacy: len(id) == 0})
	a.permissionMu.Unlock()
	a.emit(agent.AgentEvent{Kind: agent.EventAgentPermission, Permission: &agent.AgentPermissionRequest{
		Tool: toolName, Action: params.Action, Options: options, ResponseCh: make(chan string, 1),
	}})
}

func (a *Agent) handleToolStart(raw json.RawMessage) {
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
	a.emit(agent.AgentEvent{Kind: agent.EventAgentToolStart, ToolStart: &agent.AgentToolStartEvent{ID: id, Name: name, Args: string(event.Args)}})
}

func (a *Agent) handleToolEnd(raw json.RawMessage) {
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
	a.emit(agent.AgentEvent{
		Kind:    agent.EventAgentToolEnd,
		ToolEnd: &agent.AgentToolEndEvent{ID: id, Name: name},
		Err:     toolErr,
	})
}

func (a *Agent) emit(event agent.AgentEvent) {
	select {
	case a.events <- event:
	case <-a.ctx.Done():
	}
}

// Compile-time guarantee that *Agent satisfies agent.Agent (the
// merged spec+live interface). The template-half of *Agent (set by
// NewAgent) satisfies agent.AgentSpec implicitly because the new
// agent.Agent interface embeds AgentSpec.
var _ agent.Agent = (*Agent)(nil)
