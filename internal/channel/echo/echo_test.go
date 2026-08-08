package echo

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/gateway"
)

func TestEcho_Name(t *testing.T) {
	c := New("echo", nil)
	if got := c.Name(); got != "echo" {
		t.Errorf("Name() = %q, want %q", got, "echo")
	}
}

func TestEcho_DefaultName(t *testing.T) {
	c := New("", nil)
	if got := c.Name(); got != "echo" {
		t.Errorf("Name() = %q, want default %q", got, "echo")
	}
}

func TestEcho_IncomingNeverProduces(t *testing.T) {
	c := New("echo", nil)
	ch := c.Incoming()
	select {
	case msg, ok := <-ch:
		t.Errorf("echo.Incoming() should not produce; got %+v ok=%v", msg, ok)
	default:
		// Good: nothing yielded.
	}
}

func TestEcho_SendRecordsAndWrites(t *testing.T) {
	var buf bytes.Buffer
	c := New("echo", &buf)
	ctx := context.Background()
	if err := c.Send(ctx, gateway.OutboundMessage{
		ChatID: "oc_test",
		Kind:   gateway.OutReply,
		Text:   "hello world",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := c.Send(ctx, gateway.OutboundMessage{
		ChatID: "oc_test",
		Kind:   gateway.OutToolStart,
		Text:   "Read(/tmp)",
		Tool:   &gateway.ToolInfo{Name: "Read", Args: "/tmp"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Writer: one line per message.
	got := buf.String()
	for _, want := range []string{"echo: reply", "echo: tool_start", "hello world", "Read(/tmp)"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q: %q", want, got)
		}
	}

	// Record: ordered snapshot for test assertions.
	rec := c.Record()
	if len(rec) != 2 {
		t.Fatalf("Record len = %d, want 2", len(rec))
	}
	if rec[0].Kind != gateway.OutReply || rec[0].Text != "hello world" {
		t.Errorf("rec[0] = %+v, want OutReply/hello world", rec[0])
	}
	if rec[1].Kind != gateway.OutToolStart {
		t.Errorf("rec[1].Kind = %s, want tool_start", rec[1].Kind)
	}
}

func TestEcho_SendWithNilWriterDoesNotPanic(t *testing.T) {
	c := New("echo", nil)
	if err := c.Send(context.Background(), gateway.OutboundMessage{
		ChatID: "oc_x", Kind: gateway.OutReply, Text: "x",
	}); err != nil {
		t.Errorf("Send with nil writer err = %v, want nil", err)
	}
	if got := c.Record(); len(got) != 1 {
		t.Errorf("Record len = %d, want 1", len(got))
	}
}

func TestEcho_RecordReturnsCopy(t *testing.T) {
	c := New("echo", nil)
	_ = c.Send(context.Background(), gateway.OutboundMessage{ChatID: "x", Kind: gateway.OutReply})
	rec := c.Record()
	rec[0].Text = "mutated"
	// Mutating the returned slice must not affect the Channel.
	if got := c.Record(); got[0].Text == "mutated" {
		t.Errorf("Record() returned a shared slice; mutation leaked back")
	}
}

// TestEcho_AutoHandlesNewKinds verifies that echo serializes every
// OutboundKind by its String() value — adding a new kind (P1 follow-up:
// OutResult / OutUsage / OutInit) requires zero changes to echo's
// Send path. (F-49: OutCompaction kind deleted — not in this list
// anymore; the runtime now consumes EventAgentCompaction directly via
// AgentSession.RecordCompaction() and produces no OutboundMessage.)
// This test is the contract: any new kind must flow through here
// without breaking.
func TestEcho_AutoHandlesNewKinds(t *testing.T) {
	var buf bytes.Buffer
	c := New("echo", &buf)
	ctx := context.Background()

	cases := []struct {
		kind gateway.OutboundKind
		text string
		want string // substring expected in the writer output
	}{
		{gateway.OutResult, "完成", "echo: result"},
		{gateway.OutInit, "session initialized", "echo: init"},
	}
	for _, tc := range cases {
		if err := c.Send(ctx, gateway.OutboundMessage{
			ChatID: "oc_test", Kind: tc.kind, Text: tc.text,
		}); err != nil {
			t.Fatalf("Send %v: %v", tc.kind, err)
		}
	}

	out := buf.String()
	for _, tc := range cases {
		if !strings.Contains(out, tc.want) {
			t.Errorf("output missing %q (kind=%s): %q", tc.want, tc.kind, out)
		}
	}

	// And Record() captures every kind for assertions.
	rec := c.Record()
	if len(rec) != len(cases) {
		t.Fatalf("Record len = %d, want %d", len(rec), len(cases))
	}
	for i, tc := range cases {
		if rec[i].Kind != tc.kind {
			t.Errorf("rec[%d].Kind = %s, want %s", i, rec[i].Kind, tc.kind)
		}
	}
}

// --- v1.3 (SPEC §0.1): receipt lifecycle tests removed ---
// Gateway no longer owns a Receipt FSM; Channel owns its own
// receipt objects. Echo's receipt stubs are gone. The Send-only
// interface is exercised by TestEcho_Send below.

func TestEcho_SatisfiesChannelInterface(t *testing.T) {
	var _ channel.Channel = New("echo", nil)
}
