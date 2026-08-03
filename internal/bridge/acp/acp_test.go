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

	session, err := NewSession(context.Background(), bridge, "mock", WithWorkspace("/tmp/workspace"))
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer session.Close()

	promptDone := make(chan rpcMessage, 1)
	go func() { promptDone <- readRPCForTest(t, serverReader) }()
	if err := session.SendText("hello"); err != nil {
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

// TestNewSession_EmitsInit asserts that NewSession synthesizes a
// single EventInit on the events channel carrying the ACP session
// id, so the runtime can capture the resume id uniformly with
// Claude Code / Pi. The EventInit is emitted before NewSession
// returns, so the first event on Events() is the init.
func TestNewSession_EmitsInit(t *testing.T) {
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

	session, err := NewSession(context.Background(), bridge, "codex", WithWorkspace("/tmp/ws"))
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer session.Close()

	select {
	case ev := <-session.Events():
		if ev.Kind != agent.EventInit {
			t.Fatalf("first event kind = %v, want EventInit", ev.Kind)
		}
		if ev.Init == nil {
			t.Fatalf("EventInit.Init is nil")
		}
		if ev.Init.SessionID != "sess-acp-abc" {
			t.Errorf("Init.SessionID = %q, want %q", ev.Init.SessionID, "sess-acp-abc")
		}
		if ev.Init.AgentName != "codex" {
			t.Errorf("Init.AgentName = %q, want %q", ev.Init.AgentName, "codex")
		}
		if ev.Init.Workspace != "/tmp/ws" {
			t.Errorf("Init.Workspace = %q, want %q", ev.Init.Workspace, "/tmp/ws")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EventInit")
	}
}

// TestNewSession_NoSessionID_NoInit asserts that when the
// session/new response has no sessionId, NewSession returns an
// error and emits no EventInit.
func TestNewSession_NoSessionID_NoInit(t *testing.T) {
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

	session, err := NewSession(context.Background(), bridge, "codex", WithWorkspace("/tmp/ws"))
	if err == nil {
		t.Fatal("NewSession() error = nil, want non-nil")
	}
	if session != nil {
		t.Errorf("session = %+v, want nil", session)
	}
}

func TestAcpSession_ParseMessageChunkEvent(t *testing.T) {
	s := testSession()
	s.handleMethod(rpcMessage{Method: "message_chunk", Params: json.RawMessage(`{"text":"hello"}`)})
	event := receiveEvent(t, s.events)
	if event.Kind != agent.EventText || event.Text != "hello" {
		t.Fatalf("event = %+v", event)
	}
}

func TestAcpSession_ParsePermissionRequest(t *testing.T) {
	var output bytes.Buffer
	s := &acpSession{ctx: context.Background(), events: make(chan agent.AgentEvent, 1), rpc: newRPCClient(&output)}
	s.handleMethod(rpcMessage{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage(`7`),
		Method:  "session/request_permission",
		Params:  json.RawMessage(`{"action":"run ls","toolCall":{"title":"Bash"},"options":[{"optionId":"allow_once"},{"optionId":"deny"}]}`),
	})
	event := receiveEvent(t, s.events)
	if event.Kind != agent.EventPermission || event.Permission == nil {
		t.Fatalf("event = %+v", event)
	}
	if event.Permission.Tool != "Bash" || len(event.Permission.Options) != 2 {
		t.Fatalf("permission = %+v", event.Permission)
	}
	if err := s.SendPermission("allow_once"); err != nil {
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
	if start.Kind != agent.EventToolStart || start.ToolStart.ID != "tool-1" {
		t.Fatalf("start = %+v", start)
	}
	s.handleMethod(rpcMessage{Method: "tool_end", Params: json.RawMessage(`{"id":"tool-1","name":"Bash","status":"completed"}`)})
	end := receiveEvent(t, s.events)
	if end.Kind != agent.EventToolEnd || end.ToolEnd.ID != "tool-1" || end.ToolEnd.Err != nil {
		t.Fatalf("end = %+v", end)
	}
}

func TestAcpSession_EOFClosesEvents(t *testing.T) {
	client, server := net.Pipe()
	bridge := &mockBridge{Conn: client}
	s := &acpSession{bridge: bridge, rpc: newRPCClient(io.Discard), ctx: context.Background(), events: make(chan agent.AgentEvent, eventBufferSize)}
	go s.readPump()
	_ = server.Close()

	var gotDone bool
	for event := range s.events {
		if event.Kind == agent.EventDone {
			gotDone = true
		}
	}
	if !gotDone {
		t.Fatal("EOF did not emit EventDone")
	}
}

func TestRPCDecodeRejectsWrongVersion(t *testing.T) {
	if _, err := decodeRPCMessage([]byte(`{"jsonrpc":"1.0","id":1,"result":null}`)); err == nil {
		t.Fatal("decodeRPCMessage accepted JSON-RPC 1.0")
	}
}

func testSession() *acpSession {
	return &acpSession{ctx: context.Background(), events: make(chan agent.AgentEvent, eventBufferSize), rpc: newRPCClient(io.Discard)}
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
