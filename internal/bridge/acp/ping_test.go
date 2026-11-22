package acp

// The liveness probe.
//
// The ACP spec has no ping, so the probe asks for a method the
// agent does not implement. The agent's "method not found" error IS
// the answer: the agent received the request, dispatched it and
// replied. Only the absence of a reply means "not responding".
//
// The probe rides the bridge's own pending-request map: the answer is
// not published as an event, so the probe does not wait for the event
// pipeline to deliver anything. The protocol loop that reads the
// answer also reads the events, so a fully blocked event channel can
// still delay it.

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestPing_ResolvesOnTheAgentsMethodNotFoundError — the probe
// resolves when the fake agent answers with a JSON-RPC error.
func TestPing_ResolvesOnTheAgentsMethodNotFoundError(t *testing.T) {
	client, server := net.Pipe()
	transport := &mockTransport{Conn: client, pid: 42}
	defer server.Close()

	d := &driver{
		transport: transport,
		rpc:       newRPCClient(transport),
		ctx:       context.Background(),
		events:    make(chan agent.AgentEvent, eventBufferSize),
	}
	go d.readPump()

	answered := make(chan error, 1)
	go func() { answered <- d.Ping(context.Background()) }()

	reader := bufio.NewReader(server)
	request := readRPCForTest(t, reader)
	if request.Method != probeMethod {
		t.Fatalf("the probe asked for %q, want %q", request.Method, probeMethod)
	}
	writeRPCForTest(t, server, rpcMessage{
		JSONRPC: jsonRPCVersion,
		ID:      request.ID,
		Error:   &rpcError{Code: -32601, Message: `"Method not found": ` + probeMethod},
	})

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("Ping() = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the probe did not resolve")
	}
}

// TestPing_AnswersWhileATurnIsInFlight — the probe is answered
// while a session/prompt request is still pending: the answer does
// not wait for the turn to settle.
func TestPing_AnswersWhileATurnIsInFlight(t *testing.T) {
	client, server := net.Pipe()
	transport := &mockTransport{Conn: client, pid: 42}
	defer server.Close()

	d := &driver{
		transport: transport,
		rpc:       newRPCClient(transport),
		ctx:       context.Background(),
		events:    make(chan agent.AgentEvent, eventBufferSize),
	}
	go d.readPump()

	reader := bufio.NewReader(server)

	// The turn's prompt request goes out and stays unanswered: the
	// fake agent is busy.
	go func() {
		_ = d.rpc.requestAsync("session/prompt", map[string]any{"sessionId": "s1"})
	}()

	answered := make(chan error, 1)
	go func() { answered <- d.Ping(context.Background()) }()

	// The fake agent answers the probe first; the prompt request is
	// still in flight.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		request := readRPCForTest(t, reader)
		if request.Method != probeMethod {
			continue
		}
		writeRPCForTest(t, server, rpcMessage{
			JSONRPC: jsonRPCVersion,
			ID:      request.ID,
			Error:   &rpcError{Code: -32601, Message: `"Method not found": ` + probeMethod},
		})
		break
	}

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("Ping() = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the probe did not resolve while the turn was in flight")
	}
}

// TestPing_UnansweredProbeFails — no answer at all is the failure
// the daemon acts on.
func TestPing_UnansweredProbeFails(t *testing.T) {
	client, server := net.Pipe()
	transport := &mockTransport{Conn: client, pid: 42}
	defer server.Close()

	d := &driver{
		transport: transport,
		rpc:       newRPCClient(transport),
		ctx:       context.Background(),
		events:    make(chan agent.AgentEvent, eventBufferSize),
	}
	go d.readPump()
	// The fake agent reads the probe and stays silent. The pipe is
	// unbuffered, so somebody has to drain it.
	go func() {
		reader := bufio.NewReader(server)
		for {
			if _, err := reader.ReadBytes('\n'); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := d.Ping(ctx); err == nil {
		t.Fatal("Ping() = nil, want an error when the agent stays silent")
	}
}
