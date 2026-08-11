// Tests for the shell dispatch route in the gateway chain.
// Mirrors the commander dispatch tests in dispatch_fallthrough_test.go:
// shell sits between commander and message dispatcher, with its own
// prefix detection owned by internal/shell (parseShell).
package gateway

import (
	"context"
	"sync/atomic"
	"testing"
)

// TestDispatchInbound_ShellConsumed_BypassesMessageDispatcher verifies
// that a `!cmd` text routed to the shell dispatcher short-circuits
// the message dispatcher (no AcceptInbound call). This is the
// shell-package equivalent of TestDispatchInbound_RecognisedSlash_BypassesMessageDispatcher.
func TestDispatchInbound_ShellConsumed_BypassesMessageDispatcher(t *testing.T) {
	const chatID = "oc_shell_consumed"

	var dispatched int32
	shellCalled := false
	md := MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
		atomic.AddInt32(&dispatched, 1)
		return nil
	})

	gw := New(md, &noopEmitter{}).(*Router)
	gw.WithShellDispatch(func(_ context.Context, msg *InboundMessage) (*CommandResult, error) {
		if msg.Text == "!ls" {
			shellCalled = true
			return &CommandResult{Consumed: true, Reply: "shell-ok"}, nil
		}
		return nil, nil
	})

	res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
		ChatID:    chatID,
		Text:      "!ls",
		MessageID: "om_shell",
	})
	if err != nil {
		t.Fatalf("DispatchInbound(!ls): %v", err)
	}
	if !shellCalled {
		t.Fatal("shell shim was not invoked; routing should reach it before messageDispatcher")
	}
	if got := atomic.LoadInt32(&dispatched); got != 0 {
		t.Errorf("messageDispatcher must NOT run for a shell command; got %d calls", got)
	}
	if res == nil || !res.Consumed {
		t.Errorf("expected Consumed=true from shell branch; got %+v", res)
	}
	if res.Reply != "shell-ok" {
		t.Errorf("Reply = %q, want %q", res.Reply, "shell-ok")
	}
}

// TestDispatchInbound_ShellNotConsumed_FallsThroughToMessageDispatcher
// verifies that when the shell dispatcher reports Consumed=false
// (non-"!" text — shell owns its own prefix detection and signals
// "not me" via this flag), the chain continues to the next hop
// (message dispatcher). Mirrors TestDispatchInbound_CommanderFallThrough.
func TestDispatchInbound_ShellNotConsumed_FallsThroughToMessageDispatcher(t *testing.T) {
	const chatID = "oc_shell_fallthrough"

	var dispatched int32
	shellCalled := false
	md := MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
		atomic.AddInt32(&dispatched, 1)
		return nil
	})

	gw := New(md, &noopEmitter{}).(*Router)
	gw.WithShellDispatch(func(_ context.Context, _ *InboundMessage) (*CommandResult, error) {
		// Shim is wired but the dispatcher's prefix detection
		// (parseShell) didn't match — return Consumed=false so
		// the chain continues.
		shellCalled = true
		return &CommandResult{Consumed: false}, nil
	})

	res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
		ChatID:    chatID,
		Text:      "hello agent",
		MessageID: "om_fallthrough",
	})
	if err != nil {
		t.Fatalf("DispatchInbound(hello): %v", err)
	}
	if !shellCalled {
		t.Fatal("shell shim should be called for every text (it owns the ! prefix check)")
	}
	if got := atomic.LoadInt32(&dispatched); got != 1 {
		t.Errorf("messageDispatcher must run after shell's Consumed=false; got %d calls", got)
	}
	if res.Dropped {
		t.Errorf("expected non-Dropped result (message dispatcher ran), got %+v", res)
	}
}

// TestDispatchInbound_NoShellDispatchInstalled_SkipsShell verifies
// that if WithShellDispatch was never called (or the runtime hasn't
// wired shell yet), the chain transparently skips the shell slot
// and continues to message dispatch.
func TestDispatchInbound_NoShellDispatchInstalled_SkipsShell(t *testing.T) {
	var dispatched int32
	md := MessageDispatcher(func(_ context.Context, _ *InboundMessage) error {
		atomic.AddInt32(&dispatched, 1)
		return nil
	})

	gw := New(md, &noopEmitter{}).(*Router)
	// No WithShellDispatch call.

	res, err := gw.DispatchInbound(context.Background(), &InboundMessage{
		ChatID:    "oc_no_shell",
		Text:      "!echo hi",
		MessageID: "om_no_shell",
	})
	if err != nil {
		t.Fatalf("DispatchInbound(!echo hi): %v", err)
	}
	if got := atomic.LoadInt32(&dispatched); got != 1 {
		t.Errorf("messageDispatcher must run when shell slot is empty; got %d calls", got)
	}
	if res.Dropped {
		t.Errorf("expected non-Dropped result (message dispatcher ran), got %+v", res)
	}
}

