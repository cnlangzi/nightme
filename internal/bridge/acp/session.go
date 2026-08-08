package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

const (
	eventBufferSize = 64
	startupTimeout  = 10 * time.Second
)

type acpSession struct {
	bridge Bridge
	rpc    *rpcClient
	ctx    context.Context
	cancel context.CancelFunc

	// agentName and workspace are captured at NewSession for the
	// EventAgentConnected payload. ACP does not currently tell the runtime
	// about its session id through the channel — we synthesize one
	// here so the rest of the runtime can capture the resume id
	// uniformly (other bridges do this via the bridge's own init
	// event).
	agentName string
	workspace string

	sessionID string
	events    chan agent.AgentEvent

	// connectedSent guards the synthesized EventAgentConnected. We emit at most
	// once per session, after the first successful session/new.
	connectedSent bool

	permissionMu sync.Mutex
	permissions  []permissionCall
	closeOnce    sync.Once
}

type permissionCall struct {
	id     json.RawMessage
	legacy bool
}

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

// NewSession starts an ACP JSON-RPC session over bridge. The server is
// initialized and a session/new request is completed before the function
// returns. Options are optional; without WithWorkspace, cwd is empty.
func NewSession(ctx context.Context, bridge Bridge, agentName string, options ...func(*SessionOptions)) (agent.AgentSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if bridge == nil {
		return nil, errors.New("bridge/acp: nil transport")
	}

	parentCtx, cancel := context.WithCancel(ctx)
	s := &acpSession{
		bridge:    bridge,
		rpc:       newRPCClient(bridge),
		ctx:       parentCtx,
		cancel:    cancel,
		agentName: agentName,
		events:    make(chan agent.AgentEvent, eventBufferSize),
	}
	go s.readPump()
	go func() {
		<-parentCtx.Done()
		_ = s.Close()
	}()

	config := SessionOptions{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}

	startupCtx, startupCancel := context.WithTimeout(parentCtx, startupTimeout)
	defer startupCancel()
	if _, err := s.rpc.request(startupCtx, "initialize", initializeParams{
		ProtocolVersion: protocolVersion,
		ClientCapabilities: map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		ClientInfo: clientInfo{
			Name:    clientName,
			Title:   "nightme (" + agentName + ")",
			Version: clientVersion,
		},
	}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("bridge/acp: initialize: %w", err)
	}

	result, err := s.rpc.request(startupCtx, "session/new", newSessionParams{
		CWD:        config.Workspace,
		MCPServers: []any{},
	})
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("bridge/acp: session/new: %w", err)
	}
	s.workspace = config.Workspace
	if err := s.setSessionID(result); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *acpSession) setSessionID(result json.RawMessage) error {
	var response struct {
		SessionID      string `json:"sessionId"`
		SessionIDSnake string `json:"session_id"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return fmt.Errorf("bridge/acp: decode session/new response: %w", err)
	}
	s.sessionID = response.SessionID
	if s.sessionID == "" {
		s.sessionID = response.SessionIDSnake
	}
	if s.sessionID == "" {
		return errors.New("bridge/acp: session/new response has no sessionId")
	}
	// Synthesize an EventAgentConnected so the runtime can capture the resume
	// id uniformly with Claude Code / Pi. Idempotent via connectedSent.
	s.emitConnected()
	return nil
}

// emitConnected publishes a single EventAgentConnected on s.events carrying the
// ACP session id so the runtime can persist it as the AgentSession's
// resume id. Safe to call from setSessionID; emits are non-blocking
// with a ctx.Done fallback to avoid wedging the new-session path.
func (s *acpSession) emitConnected() {
	if s.connectedSent {
		return
	}
	s.connectedSent = true
	ev := agent.AgentEvent{
		Kind: agent.EventAgentConnected,
		Connected: &agent.AgentConnectedEvent{
			SessionID: s.sessionID,
			AgentName: s.agentName,
			Workspace: s.workspace,
		},
	}
	select {
	case s.events <- ev:
	case <-s.ctx.Done():
	default:
		// events buffer is full — drop. The runtime will treat a
		// missing resume id as "no resume", which is the safe
		// default; the next init/emit cycle still has connectedSent=true
		// so we don't busy-loop.
	}
}

func (s *acpSession) Events() <-chan agent.AgentEvent { return s.events }

func (s *acpSession) PID() int {
	if s.bridge == nil {
		return 0
	}
	return s.bridge.PID()
}

// SendText submits a prompt and returns after the JSON-RPC request is written.
// The prompt response only marks completion of that turn; it does not end the
// reusable ACP session, so it is deliberately consumed asynchronously.
//
// SendText is a convenience wrapper around SendBlocks for the
// text-only path. Image / file attachments are not yet encoded into
// ACP's content-block protocol here — Phase 2 will revisit when an
// ACP-compatible agent (Codex, OpenCode) actually supports inline
// images. Today those blocks degrade to "@<path>" text so the
// agent can still read the file via its tools.
func (s *acpSession) SendText(text string) error {
	if s.sessionID == "" {
		return errors.New("bridge/acp: session is not initialized")
	}
	return s.rpc.requestAsync("session/prompt", promptParams{
		SessionID: s.sessionID,
		Prompt:    []contentBlock{{Type: "text", Text: text}},
	})
}

// SendBlocks submits a structured prompt. ACP's content-block
// protocol supports text + image + file natively; the bridge
// translates agent.ContentBlock values into the wire shape. Today
// only Text is exercised by production agents (Codex / OpenCode
// have not yet landed), so the type-safe Path-based blocks are
// preserved here for Phase 2.
func (s *acpSession) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	if s.sessionID == "" {
		return errors.New("bridge/acp: session is not initialized")
	}
	if len(blocks) == 0 {
		return nil
	}
	// Derive a per-call ctx so the bridge layer owns its own
	// cancellation surface. ACP's requestAsync is fire-and-forget
	// (no ack wait), so callCtx mostly exists for symmetry with
	// pi / claudecode — future bridge-level semantics (per-call
	// timeout, retry budget, trace values) attach here without
	// leaking into the ChatSession layer. cancelCall is still
	// wired in case callCtx is ever passed to a downstream call
	// that does honor it (currently none — requestAsync is sync).
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	_ = callCtx

	out := make([]contentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			if b.Text == "" {
				continue
			}
			out = append(out, contentBlock{Type: "text", Text: b.Text})
		case agent.ContentImage, agent.ContentFile:
			// Phase 2: encode as proper ACP image/file blocks.
			// For now, fall back to a "@<path>" annotation so the
			// agent can read the file via its tools.
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
	return s.rpc.requestAsync("session/prompt", promptParams{
		SessionID: s.sessionID,
		Prompt:    out,
	})
}

// SendPermission replies to the oldest outstanding session/request_permission
// request. ACP represents the selected option as the optionId supplied by the
// agent; callers pass that opaque value through unchanged.
func (s *acpSession) SendPermission(response string) error {
	s.permissionMu.Lock()
	if len(s.permissions) == 0 {
		s.permissionMu.Unlock()
		return errors.New("bridge/acp: no pending permission request")
	}
	call := s.permissions[0]
	s.permissions = s.permissions[1:]
	s.permissionMu.Unlock()

	if call.legacy {
		return s.rpc.requestAsync("permission_response", map[string]any{
			"response": response,
		})
	}
	return s.rpc.respond(call.id, map[string]any{
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
// We reset s.connectedSent so emitConnected fires again with the new sessionId,
// letting the runtime's eventHandler capture it via SetResumeID
// (cmd/nightme/run.go newEventHandler).
func (s *acpSession) New(ctx context.Context) error {
	if s.bridge == nil {
		return errors.New("bridge/acp: nil transport")
	}
	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	result, err := s.rpc.request(startupCtx, "session/new", newSessionParams{
		CWD:        s.workspace,
		MCPServers: []any{},
	})
	if err != nil {
		return fmt.Errorf("bridge/acp: session/new: %w", err)
	}
	// Re-arm connectedSent so emitConnected fires again with the new id.
	// We can't reuse the connectedSent guard as a one-shot across the
	// session lifetime now that New can reset it; instead we use
	// the existing permissionMu (which already serializes connectedSent
	// writes through setSessionID/emitConnected) as a memory barrier.
	s.permissionMu.Lock()
	s.connectedSent = false
	s.permissionMu.Unlock()
	if err := s.setSessionID(result); err != nil {
		return err
	}
	return nil
}

func (s *acpSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		// Closing the transport is the portable ACP shutdown operation. A
		// JSON-RPC notification could block on an uncooperative PTY peer,
		// so cleanup never waits for an optional server acknowledgement.
		s.cancel()
		err = s.bridge.Close()
	})
	return err
}

func (s *acpSession) readPump() {
	defer close(s.events)

	scanner := bufio.NewScanner(s.bridge)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		message, err := decodeRPCMessage([]byte(line))
		if err != nil {
			// PTY-backed ACP servers may print a startup banner or terminal
			// control sequence. JSON-RPC framing remains authoritative.
			continue
		}
		if s.rpc.handleResponse(message) {
			continue
		}
		if message.Method != "" {
			s.handleMethod(message)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		s.emit(agent.AgentEvent{Kind: agent.EventError, Error: &agent.ErrorEvent{Err: err}})
	}
	s.rpc.failPending(io.EOF)
	s.emit(agent.AgentEvent{Kind: agent.EventDone, Done: &agent.DoneEvent{ExitCode: -1}})
}

func (s *acpSession) handleMethod(message rpcMessage) {
	switch message.Method {
	case "session/update":
		s.handleSessionUpdate(message.Params)
	case "session/request_permission", "permission_request":
		s.handlePermission(message.ID, message.Params)
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
			s.emit(agent.AgentEvent{Kind: agent.EventText, Text: text})
		}
	case "tool_start":
		s.handleToolStart(message.Params)
	case "tool_end":
		s.handleToolEnd(message.Params)
	case "session_end":
		s.emit(agent.AgentEvent{Kind: agent.EventDone, Done: &agent.DoneEvent{ExitCode: 0}})
	case "initialize", "session/new", "session/prompt":
		// A PTY may echo client requests before the ACP server disables
		// terminal echo. They are outbound methods, not server calls.
		return
	default:
		if len(message.ID) > 0 {
			_ = s.rpc.respond(message.ID, nil, &rpcError{Code: -32601, Message: "method not found"})
		}
	}
}

func (s *acpSession) handleSessionUpdate(raw json.RawMessage) {
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
			s.emit(agent.AgentEvent{Kind: agent.EventText, Text: content.Text})
		}
	case "tool_call":
		s.handleToolStart(params.Update)
	case "tool_call_update":
		s.handleToolEnd(params.Update)
	case "message_chunk":
		var content struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(update.Content, &content) == nil {
			s.emit(agent.AgentEvent{Kind: agent.EventText, Text: content.Text})
		}
	}
}

func (s *acpSession) handlePermission(id json.RawMessage, raw json.RawMessage) {
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
	s.permissionMu.Lock()
	s.permissions = append(s.permissions, permissionCall{id: append(json.RawMessage(nil), id...), legacy: len(id) == 0})
	s.permissionMu.Unlock()
	s.emit(agent.AgentEvent{Kind: agent.EventPermission, Permission: &agent.PermissionRequest{
		Tool: toolName, Action: params.Action, Options: options, ResponseCh: make(chan string, 1),
	}})
}

func (s *acpSession) handleToolStart(raw json.RawMessage) {
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
	s.emit(agent.AgentEvent{Kind: agent.EventToolStart, ToolStart: &agent.ToolStartEvent{ID: id, Name: name, Args: string(event.Args)}})
}

func (s *acpSession) handleToolEnd(raw json.RawMessage) {
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
	s.emit(agent.AgentEvent{Kind: agent.EventToolEnd, ToolEnd: &agent.ToolEndEvent{ID: id, Name: name, Err: toolErr}})
}

func (s *acpSession) emit(event agent.AgentEvent) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

var _ agent.AgentSession = (*acpSession)(nil)
