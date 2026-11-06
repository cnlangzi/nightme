package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// This file covers the acp bridge's exact-text accumulator — the
// per-turn raw reply the bridge holds alongside its flushed, trimmed
// reply buffer.
//
// The bridge flushes its reply buffer per sentence and trims every
// flushed chunk, so the whitespace that separated the chunks is gone
// by the time the shared drain reassembles the reply from the event
// stream. The accumulator keeps the raw text, so a pull-request body
// keeps every blank line the agent wrote.

// ─── helpers ───────────────────────────────────────────────────────

// chunkUpdate builds the session/update params carrying an
// agent_message_chunk with the given text.
func chunkUpdate(text string) json.RawMessage {
	content, _ := json.Marshal(map[string]string{"type": "text", "text": text})
	params, _ := json.Marshal(map[string]any{
		"sessionId": "test-session",
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       json.RawMessage(content),
		},
	})
	return params
}

// toolCallUpdate builds the session/update params carrying a
// tool_call. A tool boundary forces the bridge to flush its reply
// buffer — the deterministic way to split a reply across two
// flushed chunks.
func toolCallUpdate() json.RawMessage {
	params, _ := json.Marshal(map[string]any{
		"sessionId": "test-session",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "tool-1",
			"title":         "Bash",
		},
	})
	return params
}

// acpTestStarter is a Starter whose Start hands back a live
// *agent.Agent backed by the acp driver under test.
type acpTestStarter struct {
	name string
	d    *driver
}

func (s *acpTestStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, "mock", nil, nil)
}
func (s *acpTestStarter) Detect() error { return nil }
func (s *acpTestStarter) Init(string)   {}

func (s *acpTestStarter) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return agent.NewAgent(s.Info(), 0, s.d.events, s.d), nil
}

func (s *acpTestStarter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunSession(ctx, s, cfg, blocks, opts...)
}

func (s *acpTestStarter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunSession(ctx, s, cfg, []agent.ContentBlock{{Type: agent.ContentText, Text: "review"}}, opts...)
}

// ─── the accumulator ───────────────────────────────────────────────

// TestTurnText_KeepsBlankLinesAcrossFlushBoundary: the accumulator
// holds the reply as the agent wrote it — every blank line included
// — while the flushed chunks the chat channel sees are trimmed.
func TestTurnText_KeepsBlankLinesAcrossFlushBoundary(t *testing.T) {
	d := newFlushTestDriver(t)

	first := strings.Repeat("First paragraph sentence. ", 12) + "\n\n"
	second := strings.Repeat("Second paragraph sentence. ", 12) + "\n"

	d.handleSessionUpdate(chunkUpdate(first))
	d.flushTextBuffers()
	d.handleSessionUpdate(chunkUpdate(second))
	d.flushTextBuffers()

	if got := d.TurnText(); got != first+second {
		t.Errorf("TurnText() = %q, want the raw reply %q", got, first+second)
	}

	// The streamed chunks are trimmed — the paragraph break that
	// separated them is gone. This is the loss the accumulator
	// exists to prevent.
	var streamed []string
	for len(streamed) < 2 {
		select {
		case ev := <-d.events:
			if ev.Kind == agent.EventAgentText {
				streamed = append(streamed, ev.Text)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for the flushed chunks, got %d", len(streamed))
		}
	}
	joined := strings.Join(streamed, "\n")
	if strings.Contains(joined, "\n\n") {
		t.Errorf("streamed chunks kept the blank line (%q); the fixture no longer exercises the loss", joined)
	}
	if joined == first+second {
		t.Errorf("streamed chunks equal the raw reply (%q); the fixture no longer exercises the loss", joined)
	}
}

// TestTurnText_ResetsOnNewTurn: the accumulator holds the current
// turn's text only — a follow-up turn never inherits the previous
// turn's reply.
func TestTurnText_ResetsOnNewTurn(t *testing.T) {
	d := newFlushTestDriver(t)

	d.handleSessionUpdate(chunkUpdate("first turn\n"))
	if got := d.TurnText(); got != "first turn\n" {
		t.Fatalf("TurnText() = %q, want %q", got, "first turn\n")
	}

	d.resetTurnState()
	if got := d.TurnText(); got != "" {
		t.Errorf("TurnText() = %q after the turn reset, want empty", got)
	}
}

// ─── the one-shot turn ─────────────────────────────────────────────

// readServerRPC reads one JSON-RPC frame from the mock server's
// reader. It reports the error instead of failing the test: it runs
// on the server goroutine, where t.Fatal is not allowed.
func readServerRPC(r *bufio.Reader) (rpcMessage, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return rpcMessage{}, err
	}
	return decodeRPCMessage(trimNewline(line))
}

// trimNewline drops the frame's trailing newline.
func trimNewline(line []byte) []byte {
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line
}

// TestRunSession_KeepsReplyBlankLines drives a full one-shot turn
// over the session transport: a mock ACP server streams a reply in
// two chunks separated by a tool boundary, and the drain must hand
// back the agent's raw text — paragraph break included.
func TestRunSession_KeepsReplyBlankLines(t *testing.T) {
	client, server := net.Pipe()
	transport := &mockTransport{Conn: client, pid: 42}
	defer server.Close()

	first := strings.Repeat("First paragraph sentence. ", 12) + "\n\n"
	second := strings.Repeat("Second paragraph sentence. ", 12) + "\n"

	serverReader := bufio.NewReader(server)
	go func() {
		initialize, err := readServerRPC(serverReader)
		if err != nil {
			return
		}
		writeRPCForTest(t, server, rpcMessage{
			JSONRPC: jsonRPCVersion, ID: initialize.ID,
			Result: json.RawMessage(`{"protocolVersion":1}`),
		})
		newSession, err := readServerRPC(serverReader)
		if err != nil {
			return
		}
		writeRPCForTest(t, server, rpcMessage{
			JSONRPC: jsonRPCVersion, ID: newSession.ID,
			Result: json.RawMessage(`{"sessionId":"session-1"}`),
		})
		prompt, err := readServerRPC(serverReader)
		if err != nil {
			return
		}
		writeRPCForTest(t, server, rpcMessage{
			JSONRPC: jsonRPCVersion, Method: "session/update", Params: chunkUpdate(first),
		})
		writeRPCForTest(t, server, rpcMessage{
			JSONRPC: jsonRPCVersion, Method: "session/update", Params: toolCallUpdate(),
		})
		writeRPCForTest(t, server, rpcMessage{
			JSONRPC: jsonRPCVersion, Method: "session/update", Params: chunkUpdate(second),
		})
		writeRPCForTest(t, server, rpcMessage{
			JSONRPC: jsonRPCVersion, ID: prompt.ID,
			Result: json.RawMessage(`{"stopReason":"end_turn"}`),
		})
	}()

	d := newAgentForTest(transport, "mock", "/tmp/workspace")
	if err := d.handshake(context.Background(), "/tmp/workspace"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	s := &acpTestStarter{name: "mock", d: d}
	res, err := agent.RunSession(context.Background(), s,
		agent.StartConfig{Workspace: "/tmp/workspace"},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "write a pr body"}})
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if res.Text != first+second {
		t.Errorf("Text = %q, want the agent's raw reply %q", res.Text, first+second)
	}
}
