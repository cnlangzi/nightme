package echo

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
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
		Kind:   gateway.OutText,
		Text:   "hello world",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := c.Send(ctx, gateway.OutboundMessage{
		ChatID: "oc_test",
		Kind:   gateway.OutToolStart,
		Text:   "Read(/tmp)",
		Meta:   map[string]any{"tool_name": "Read"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Writer: one line per message.
	got := buf.String()
	for _, want := range []string{"echo: text", "echo: tool_start", "hello world", "Read(/tmp)"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q: %q", want, got)
		}
	}

	// Record: ordered snapshot for test assertions.
	rec := c.Record()
	if len(rec) != 2 {
		t.Fatalf("Record len = %d, want 2", len(rec))
	}
	if rec[0].Kind != gateway.OutText || rec[0].Text != "hello world" {
		t.Errorf("rec[0] = %+v, want OutText/hello world", rec[0])
	}
	if rec[1].Kind != gateway.OutToolStart {
		t.Errorf("rec[1].Kind = %s, want tool_start", rec[1].Kind)
	}
}

func TestEcho_SendWithNilWriterDoesNotPanic(t *testing.T) {
	c := New("echo", nil)
	if err := c.Send(context.Background(), gateway.OutboundMessage{
		ChatID: "oc_x", Kind: gateway.OutText, Text: "x",
	}); err != nil {
		t.Errorf("Send with nil writer err = %v, want nil", err)
	}
	if got := c.Record(); len(got) != 1 {
		t.Errorf("Record len = %d, want 1", len(got))
	}
}

func TestEcho_RecordReturnsCopy(t *testing.T) {
	c := New("echo", nil)
	_ = c.Send(context.Background(), gateway.OutboundMessage{ChatID: "x", Kind: gateway.OutText})
	rec := c.Record()
	rec[0].Text = "mutated"
	// Mutating the returned slice must not affect the Channel.
	if got := c.Record(); got[0].Text == "mutated" {
		t.Errorf("Record() returned a shared slice; mutation leaked back")
	}
}

// TestEcho_AutoHandlesNewKinds verifies that echo serializes every
// OutboundKind by its String() value — adding a new kind (P1 follow-up:
// OutResult / OutUsage / OutCompaction / OutInit) requires zero changes
// to echo's Send path. This test is the contract: any new kind must
// flow through here without breaking.
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
		{gateway.OutUsage, "1.2k tokens", "echo: usage"},
		{gateway.OutCompaction, "✶ Compacting conversation…", "echo: compaction"},
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

// --- v1.1 receipt lifecycle tests ---

func TestEcho_CreateReceiptReturnsHandle(t *testing.T) {
	var buf bytes.Buffer
	c := New("echo", &buf)
	ctx := context.Background()

	blocks := []agent.ContentBlock{{Type: agent.ContentText, Text: "hello"}}
	rcpt, err := c.CreateReceipt(ctx, "chat-1", "user-msg-1", blocks)
	if err != nil {
		t.Fatalf("CreateReceipt err: %v", err)
	}
	if rcpt == nil {
		t.Fatal("CreateReceipt returned nil receipt")
	}
	if !strings.Contains(buf.String(), "created (state=pending") {
		t.Errorf("missing pending log; buf = %q", buf.String())
	}
}

func TestEcho_UpdateReceiptTransitionsState(t *testing.T) {
	var buf bytes.Buffer
	c := New("echo", &buf)
	ctx := context.Background()

	rcpt, err := c.CreateReceipt(ctx, "chat-1", "u-1", []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err != nil {
		t.Fatalf("CreateReceipt: %v", err)
	}
	for _, st := range []channel.ReceiptState{channel.ReceiptExecuting, channel.ReceiptDone, channel.ReceiptError} {
		if err := c.UpdateReceipt(ctx, rcpt, st); err != nil {
			t.Fatalf("UpdateReceipt(%d) err: %v", st, err)
		}
	}
	out := buf.String()
	for _, want := range []string{"state=executing", "state=done", "state=error"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in log; got %q", want, out)
		}
	}
}

func TestEcho_DisposeReceiptLogs(t *testing.T) {
	var buf bytes.Buffer
	c := New("echo", &buf)
	ctx := context.Background()

	rcpt, _ := c.CreateReceipt(ctx, "c", "u", nil)
	if err := c.DisposeReceipt(ctx, rcpt); err != nil {
		t.Fatalf("DisposeReceipt err: %v", err)
	}
	if !strings.Contains(buf.String(), "disposed") {
		t.Errorf("missing dispose log; got %q", buf.String())
	}
}

func TestEcho_UpdateReceiptNilIsNoop(t *testing.T) {
	c := New("echo", nil)
	if err := c.UpdateReceipt(context.Background(), nil, channel.ReceiptDone); err != nil {
		t.Errorf("UpdateReceipt(nil) err = %v, want nil", err)
	}
	if err := c.DisposeReceipt(context.Background(), nil); err != nil {
		t.Errorf("DisposeReceipt(nil) err = %v, want nil", err)
	}
}

func TestEcho_SatisfiesChannelInterface(t *testing.T) {
	var _ channel.Channel = New("echo", nil)
}
