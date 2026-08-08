package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

type mockBridge struct {
	net.Conn
	pid int
}

func (b *mockBridge) PID() int { return b.pid }

func TestAcpSession_SendText_EncodesCorrectly(t *testing.T) {
	client, server := net.Pipe()
	bridge := &mockBridge{Conn: client, pid: 42}
	defer server.Close()

	serverReader := bufio.NewReader(server)
	go func() {
		initialize := readRPCForTest(t, serverReader)
		writeRPCForTest(t, server, rpcMessage{JSONRPC: jsonRPCVersion, ID: initialize.ID, Result: json.RawMessage(`{"protocolVersion":1}`)})
		newSession := readRPCForTest(t, serverReader)
		writeRPCForTest(t, server, rpcMessage{JSONRPC: jsonRPCVersion, ID: newSession.ID, Result: json.RawMessage(`{"sessionId":"session-1"}`)})
	}()

	a := newAgentForTest(bridge, "mock", "/tmp/workspace")
	if err := a.handshake(context.Background(), "/tmp/workspace"); err != nil {
		t.Fatalf("handshake() error = %v", err)
	}
	defer a.Close()

	promptDone := make(chan rpcMessage, 1)
	go func() { promptDone <- readRPCForTest(t, serverReader) }()
	if err := a.SendText("hello"); err != nil {
		t.Fatalf("SendText() error = %v", err)
	}
	select {
	case request := <-promptDone:
		if request.Method != "session/prompt" {
			t.Fatalf("method = %q, want session/prompt", request.Method)
		}
		var params promptParams
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params.SessionID != "session-1" || len(params.Prompt) != 1 || params.Prompt[0].Text != "hello" {
			t.Fatalf("prompt params = %+v", params)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for prompt")
	}
}

// TestHandshake_EmitsInit asserts that handshake() synthesizes a
// single EventAgentReady on the events channel carrying the ACP session
// id, so the runtime can capture the resume id uniformly with
// Claude Code / Pi. The EventAgentReady is emitted before handshake
// returns, so the first event on Events() is the init.
func TestHandshake_EmitsInit(t *testing.T) {
	client, server := net.Pipe()
	bridge := &mockBridge{Conn: client, pid: 42}
	defer server.Close()

	serverReader := bufio.NewReader(server)
	go func() {
		initialize := readRPCForTest(t, serverReader)
		writeRPCForTest(t, server, rpcMessage{JSONRPC: jsonRPCVersion, ID: initialize.ID, Result: json.RawMessage(`{"protocolVersion":1}`)})
		newSession := readRPCForTest(t, serverReader)
		writeRPCForTest(t, server, rpcMessage{JSONRPC: jsonRPCVersion, ID: newSession.ID, Result: json.RawMessage(`{"sessionId":"sess-acp-abc"}`)})
	}()

	a := newAgentForTest(bridge, "codex", "/tmp/ws")
	if err := a.handshake(context.Background(), "/tmp/ws"); err != nil {
		t.Fatalf("handshake() error = %v", err)
	}
	defer a.Close()

	select {
	case ev := <-a.Events():
		if ev.Kind != agent.EventAgentReady {
			t.Fatalf("first event kind = %v, want EventAgentReady", ev.Kind)
		}
		if ev.SessionID != "sess-acp-abc" {
			t.Errorf("Init.SessionID = %q, want %q", ev.SessionID, "sess-acp-abc")
		}
		if ev.AgentName != "codex" {
			t.Errorf("Init.AgentName = %q, want %q", ev.AgentName, "codex")
		}
		if ev.Workspace != "/tmp/ws" {
			t.Errorf("Init.Workspace = %q, want %q", ev.Workspace, "/tmp/ws")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EventAgentReady")
	}
}

// TestHandshake_NoSessionID_NoInit asserts that when the
// session/new response has no sessionId, handshake returns an
// error and emits no EventAgentReady.
func TestHandshake_NoSessionID_NoInit(t *testing.T) {
	client, server := net.Pipe()
	bridge := &mockBridge{Conn: client, pid: 42}
	defer server.Close()

	serverReader := bufio.NewReader(server)
	go func() {
		initialize := readRPCForTest(t, serverReader)
		writeRPCForTest(t, server, rpcMessage{JSONRPC: jsonRPCVersion, ID: initialize.ID, Result: json.RawMessage(`{"protocolVersion":1}`)})
		newSession := readRPCForTest(t, serverReader)
		// Response has neither sessionId nor session_id.
		writeRPCForTest(t, server, rpcMessage{JSONRPC: jsonRPCVersion, ID: newSession.ID, Result: json.RawMessage(`{}`)})
	}()

	a := newAgentForTest(bridge, "codex", "/tmp/ws")
	if err := a.handshake(context.Background(), "/tmp/ws"); err == nil {
		t.Fatal("handshake() error = nil, want non-nil")
	}
}

func TestAcpSession_ParseMessageChunkEvent(t *testing.T) {
	s := testSession()
	s.handleMethod(rpcMessage{Method: "message_chunk", Params: json.RawMessage(`{"text":"hello"}`)})
	event := receiveEvent(t, s.events)
	if event.Kind != agent.EventAgentText || event.Text != "hello" {
		t.Fatalf("event = %+v", event)
	}
}

func TestAcpSession_ParsePermissionRequest(t *testing.T) {
	var output bytes.Buffer
	a := &Agent{ctx: context.Background(), events: make(chan agent.AgentEvent, 1), rpc: newRPCClient(&output)}
	a.handleMethod(rpcMessage{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage(`7`),
		Method:  "session/request_permission",
		Params:  json.RawMessage(`{"action":"run ls","toolCall":{"title":"Bash"},"options":[{"optionId":"allow_once"},{"optionId":"deny"}]}`),
	})
	event := receiveEvent(t, a.events)
	if event.Kind != agent.EventAgentPermission || event.Permission == nil {
		t.Fatalf("event = %+v", event)
	}
	if event.Permission.Tool != "Bash" || len(event.Permission.Options) != 2 {
		t.Fatalf("permission = %+v", event.Permission)
	}
	if err := a.SendPermission("allow_once"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"optionId":"allow_once"`) {
		t.Fatalf("response = %s", output.String())
	}
}

func TestAcpSession_ParseToolEvents(t *testing.T) {
	s := testSession()
	s.handleMethod(rpcMessage{Method: "tool_start", Params: json.RawMessage(`{"id":"tool-1","name":"Bash","rawInput":{"command":"ls"}}`)})
	start := receiveEvent(t, s.events)
	if start.Kind != agent.EventAgentToolStart || start.ToolStart.ID != "tool-1" {
		t.Fatalf("start = %+v", start)
	}
	s.handleMethod(rpcMessage{Method: "tool_end", Params: json.RawMessage(`{"id":"tool-1","name":"Bash","status":"completed"}`)})
	end := receiveEvent(t, s.events)
	if end.Kind != agent.EventAgentToolEnd || end.ToolEnd.ID != "tool-1" || end.Err != nil {
		t.Fatalf("end = %+v", end)
	}
}

func TestAcpSession_EOFClosesEvents(t *testing.T) {
	client, server := net.Pipe()
	bridge := &mockBridge{Conn: client}
	a := &Agent{bridge: bridge, rpc: newRPCClient(io.Discard), ctx: context.Background(), events: make(chan agent.AgentEvent, eventBufferSize)}
	go a.readPump()
	_ = server.Close()

	var gotDone bool
	for event := range a.events {
		if event.Kind == agent.EventAgentDone {
			gotDone = true
		}
	}
	if !gotDone {
		t.Fatal("EOF did not emit EventAgentDone")
	}
}

func TestRPCDecodeRejectsWrongVersion(t *testing.T) {
	if _, err := decodeRPCMessage([]byte(`{"jsonrpc":"1.0","id":1,"result":null}`)); err == nil {
		t.Fatal("decodeRPCMessage accepted JSON-RPC 1.0")
	}
}

func testSession() *Agent {
	return &Agent{ctx: context.Background(), events: make(chan agent.AgentEvent, eventBufferSize), rpc: newRPCClient(io.Discard)}
}

// newAgentForTest constructs an Agent wired to a mock bridge (no real
// PTY spawn), with rpc / events / ctx wired so handshake() can run
// against the in-process server. Returns an Agent ready to call
// handshake() and then Send* / Close.
func newAgentForTest(bridge Bridge, name, workspace string) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	a := &Agent{
		name:      name,
		command:   "test",
		bridge:    bridge,
		rpc:       newRPCClient(bridge),
		ctx:       ctx,
		cancel:    cancel,
		agentName: name,
		workspace: workspace,
		events:    make(chan agent.AgentEvent, eventBufferSize),
	}
	go a.readPump()
	return a
}

func receiveEvent(t *testing.T, events <-chan agent.AgentEvent) agent.AgentEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return agent.AgentEvent{}
	}
}

func readRPCForTest(t *testing.T, reader *bufio.Reader) rpcMessage {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read RPC: %v", err)
	}
	message, err := decodeRPCMessage(bytes.TrimSpace(line))
	if err != nil {
		t.Fatalf("decode RPC: %v", err)
	}
	return message
}

func writeRPCForTest(t *testing.T, writer io.Writer, message rpcMessage) {
	t.Helper()
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(append(payload, '\n')); err != nil {
		t.Fatal(err)
	}
}
