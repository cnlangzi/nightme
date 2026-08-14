// Tests for the shell dispatch route in the router chain.
// Mirrors the commander dispatch tests in dispatch_fallthrough_test.go:
// shell sits between commander and message dispatcher, with its own
// prefix detection owned by internal/shell (parseShell).
package inbound

import (
	"context"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/inbound/teststubs"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestDispatch_ShellConsumed_BypassesMessageHandler verifies
// that a `!cmd` text routed to the shell dispatcher short-
// circuits the message handler. The shell-package equivalent
// of TestDispatch_RecognisedSlash_BypassesMessageHandler.
//
// Note: in F-58 the shell branch does NOT carry a Reply back
// through the inbound.Router. The shell package is responsible
// for posting its own reply card via its Sender (the runtime
// wires cs.Emitter() as the Sender). The inbound layer just
// sees "shell claimed it" (Consumed=true) and stops the chain.
func TestDispatch_ShellConsumed_BypassesMessageHandler(t *testing.T) {
	const chatID = "oc_shell_consumed"

	sh := teststubs.NewShell()
	sh.Recognize("!ls")

	msg := teststubs.NewMessage(chatsession.NewManager())
	r := New(msg, teststubs.AlwaysFallThrough{}, sh, teststubs.NewReaction(true), nil, "primary")

	res, err := r.Dispatch(context.Background(), &messages.InboundMessage{
		ChatID:    chatID,
		Text:      "!ls",
		MessageID: "om_shell",
	})
	// F-59: async dispatch — wait before asserting.
	r.WaitExec()
	if err != nil {
		t.Fatalf("Dispatch(!ls): %v", err)
	}
	if sh.Calls() == 0 {
		t.Fatal("shell dispatcher was not invoked; routing should reach it before MessageHandler")
	}
	if got := msg.Hits(); got != 0 {
		t.Errorf("MessageHandler must NOT run for a shell command; got %d calls", got)
	}
	if res == nil || !res.Consumed {
		t.Errorf("expected Consumed=true from shell branch; got %+v", res)
	}
	// The shell branch returns Consumed=true with no Reply
	// (the reply card is posted by the shell.Dispatcher itself).
	if res.Reply != "" {
		t.Errorf("Reply = %q, want \"\" (shell posts its own reply card)", res.Reply)
	}
}

// TestDispatch_ShellNotConsumed_FallsThroughToMessageHandler
// verifies that when the shell dispatcher reports Consumed=false
// (non-"!" text — shell owns its own prefix detection and
// signals "not me" via this flag), the chain continues to
// the next hop (message handler). Mirrors
// TestDispatch_CommanderFallThrough.
func TestDispatch_ShellNotConsumed_FallsThroughToMessageHandler(t *testing.T) {
	const chatID = "oc_shell_fallthrough"

	sh := teststubs.NewShell() // no shell commands "registered"
	msg := teststubs.NewMessage(chatsession.NewManager())
	r := New(msg, teststubs.AlwaysFallThrough{}, sh, teststubs.NewReaction(true), nil, "primary")

	res, err := r.Dispatch(context.Background(), &messages.InboundMessage{
		ChatID:     chatID,
		Text:       "hello world",
		HasMention: true,
		MessageID:  "om_plain",
	})
	// F-59: async dispatch — wait before asserting.
	r.WaitExec()
	if err != nil {
		t.Fatalf("Dispatch(hello world): %v", err)
	}
	if sh.Calls() == 0 {
		t.Error("shell dispatcher was not invoked at all; routing should consult it before MessageHandler")
	}
	if res == nil || res.Consumed {
		t.Errorf("expected Consumed=false (fall-through); got %+v", res)
	}
	if got := msg.Hits(); got != 1 {
		t.Errorf("MessageHandler should be called once on shell fall-through, got %d", got)
	}
}

// TestDispatch_ShellPriorityAfterCommander pins the order:
// command branch runs BEFORE shell branch. A text starting
// with "/" (a slash command) must never reach the shell
// dispatcher, even if the shell's recognised set would have
// claimed the same text.
func TestDispatch_ShellPriorityAfterCommander(t *testing.T) {
	const chatID = "oc_priority"

	commander := teststubs.NewCommander()
	commander.Recognize("/foo", teststubs.Result{Consumed: true, Reply: "from-commander"})

	sh := teststubs.NewShell()
	sh.Recognize("/foo")

	msg := teststubs.NewMessage(chatsession.NewManager())
	r := New(msg, commander, sh, teststubs.NewReaction(true), nil, "primary")

	res, err := r.Dispatch(context.Background(), &messages.InboundMessage{
		ChatID:     chatID,
		Text:       "/foo",
		HasMention: true,
		MessageID:  "om_priority",
	})
	// F-59: async dispatch — wait before asserting.
	r.WaitExec()
	if err != nil {
		t.Fatalf("Dispatch(/foo): %v", err)
	}
	if res == nil || !res.Consumed {
		t.Errorf("expected Consumed=true from commander branch; got %+v", res)
	}
	// F-59: the reply text is no longer carried on the dispatch
	// result (it's emitted asynchronously via the Emitter). The
	// priority invariant is what this test pins: commander ran
	// before shell, so the dispatcher took the command branch.
	if commander.Calls() != 1 {
		t.Errorf("commander must run exactly once for /foo; got %d calls", commander.Calls())
	}
	if sh.Calls() != 0 {
		t.Errorf("shell dispatcher must not run for a slash command; got %d calls", sh.Calls())
	}
	if got := msg.Hits(); got != 0 {
		t.Errorf("MessageHandler must not run for a slash command; got %d calls", got)
	}
}
