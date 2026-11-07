package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
)

// TestSetConfigOption_SendsConfigIDAndRecordsModel verifies the
// session/set_config_option request carries the option id as
// "configId" (the server rejects "optionId" with -32602) and that
// the model the bridge recorded follows the refreshed option list.
func TestSetConfigOption_SendsConfigIDAndRecordsModel(t *testing.T) {
	client, server := net.Pipe()
	transport := &mockTransport{Conn: client, pid: 42}
	defer server.Close()

	serverReader := bufio.NewReader(server)
	seen := make(chan rpcMessage, 1)
	go func() {
		initialize := readRPCForTest(t, serverReader)
		writeRPCForTest(t, server, rpcMessage{JSONRPC: jsonRPCVersion, ID: initialize.ID, Result: json.RawMessage(`{"protocolVersion":1}`)})
		newSession := readRPCForTest(t, serverReader)
		writeRPCForTest(t, server, rpcMessage{
			JSONRPC: jsonRPCVersion,
			ID:      newSession.ID,
			Result:  json.RawMessage(`{"sessionId":"session-1","configOptions":[{"id":"model","category":"model","type":"select","currentValue":"opencode/crouton-alpha-free","options":[{"value":"opencode/crouton-alpha-free","name":"opencode/crouton-alpha-free"}]}]}`),
		})
		set := readRPCForTest(t, serverReader)
		seen <- set
		writeRPCForTest(t, server, rpcMessage{
			JSONRPC: jsonRPCVersion,
			ID:      set.ID,
			Result:  json.RawMessage(`{"configOptions":[{"id":"model","category":"model","type":"select","currentValue":"minimax-cn/MiniMax-M3","options":[{"value":"minimax-cn/MiniMax-M3","name":"minimax-cn/MiniMax-M3"}]}]}`),
		})
	}()

	a := newAgentForTest(transport, "opencode", "/tmp/workspace")
	if err := a.handshake(context.Background(), "/tmp/workspace"); err != nil {
		t.Fatalf("handshake() error = %v", err)
	}
	defer a.Close()

	if got := a.recordedModel(); got != "opencode/crouton-alpha-free" {
		t.Fatalf("model after handshake = %q, want opencode/crouton-alpha-free", got)
	}

	raw, err := a.SetConfigOption("model", "minimax-cn/MiniMax-M3")
	if err != nil {
		t.Fatalf("SetConfigOption() error = %v", err)
	}
	if !strings.Contains(string(raw), "minimax-cn/MiniMax-M3") {
		t.Errorf("response = %s, want the refreshed option list", raw)
	}

	request := <-seen
	if request.Method != "session/set_config_option" {
		t.Fatalf("method = %q, want session/set_config_option", request.Method)
	}
	var params struct {
		SessionID string `json:"sessionId"`
		ConfigID  string `json:"configId"`
		Value     string `json:"value"`
		OptionID  string `json:"optionId"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.ConfigID != "model" {
		t.Errorf("configId = %q, want model", params.ConfigID)
	}
	if params.OptionID != "" {
		t.Errorf("request carried optionId = %q, want none", params.OptionID)
	}
	if params.SessionID != "session-1" {
		t.Errorf("sessionId = %q, want session-1", params.SessionID)
	}
	if params.Value != "minimax-cn/MiniMax-M3" {
		t.Errorf("value = %q, want minimax-cn/MiniMax-M3", params.Value)
	}

	if got := a.recordedModel(); got != "minimax-cn/MiniMax-M3" {
		t.Errorf("model after set = %q, want minimax-cn/MiniMax-M3", got)
	}
}

// TestSetConfigOption_Rejected verifies a server-side rejection is
// reported as an error and leaves the recorded model alone — the
// session keeps the model opencode picked.
func TestSetConfigOption_Rejected(t *testing.T) {
	client, server := net.Pipe()
	transport := &mockTransport{Conn: client, pid: 42}
	defer server.Close()

	serverReader := bufio.NewReader(server)
	go func() {
		initialize := readRPCForTest(t, serverReader)
		writeRPCForTest(t, server, rpcMessage{JSONRPC: jsonRPCVersion, ID: initialize.ID, Result: json.RawMessage(`{"protocolVersion":1}`)})
		newSession := readRPCForTest(t, serverReader)
		writeRPCForTest(t, server, rpcMessage{
			JSONRPC: jsonRPCVersion,
			ID:      newSession.ID,
			Result:  json.RawMessage(`{"sessionId":"session-1","configOptions":[{"id":"model","category":"model","type":"select","currentValue":"opencode/crouton-alpha-free"}]}`),
		})
		set := readRPCForTest(t, serverReader)
		writeRPCForTest(t, server, rpcMessage{
			JSONRPC: jsonRPCVersion,
			ID:      set.ID,
			Error:   &rpcError{Code: -32602, Message: "Invalid params: model not found: nope/none"},
		})
	}()

	a := newAgentForTest(transport, "opencode", "/tmp/workspace")
	if err := a.handshake(context.Background(), "/tmp/workspace"); err != nil {
		t.Fatalf("handshake() error = %v", err)
	}
	defer a.Close()

	if _, err := a.SetConfigOption("model", "nope/none"); err == nil {
		t.Fatal("SetConfigOption() error = nil, want non-nil")
	}
	if got := a.recordedModel(); got != "opencode/crouton-alpha-free" {
		t.Errorf("model after rejected set = %q, want opencode/crouton-alpha-free", got)
	}
}

// TestSetConfigOption_WithoutSession verifies the setter refuses to
// talk to a session that was never opened.
func TestSetConfigOption_WithoutSession(t *testing.T) {
	a := &driver{ctx: context.Background(), rpc: newRPCClient(io.Discard)}
	if _, err := a.SetConfigOption("model", "minimax-cn/MiniMax-M3"); err == nil {
		t.Fatal("SetConfigOption() error = nil, want non-nil")
	}
}

// recordedModel reads the bridge-local model under the mutex.
func (d *driver) recordedModel() string {
	d.modelMu.Lock()
	defer d.modelMu.Unlock()
	return d.model
}
