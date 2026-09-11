// Tests for the handoff package's slash command factory.
//
// Mirrors the layout used by internal/command/steer/cmd_test.go:
// spec sanity, preflight early-exits, and the happy path that
// proves the prompt lands in the queue. The collector + file
// write side of /handoff (runHandoff) is exercised by the
// production goroutine, not by these unit tests; e2e coverage
// for the on-disk handoff.md lives in the integration harness
// wired through the echo bridge (see e2e_slash_test.go for the
// pattern).
package handoff_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	handoffpkg "github.com/cnlangzi/nightme/internal/command/handoff"
)

// ackReply asserts that out is the immediate-ack path: Consumed,
// a Reply set, no Outbound entries. /handoff uses
// command.Reply for its ack so the runtime shim posts the
// OutCommandReply through cs.Emitter.
func ackReply(t *testing.T, out *command.SlashOutput, contains string) {
	t.Helper()
	if out == nil {
		t.Fatal("SlashOutput is nil")
	}
	if !out.Consumed {
		t.Error("Consumed = false, want true")
	}
	if !strings.Contains(out.Reply, contains) {
		t.Errorf("Reply = %q, want substring %q", out.Reply, contains)
	}
	if len(out.Outbound) != 0 {
		t.Errorf("ack should not produce Outbound entries, got %d", len(out.Outbound))
	}
}

// TestFactory_Spec covers the static spec advertised in /help.
func TestFactory_Spec(t *testing.T) {
	f := handoffpkg.NewFactory()
	s := f.Spec()
	if s.Name != "handoff" {
		t.Fatalf("Spec.Name = %q, want handoff", s.Name)
	}
	if s.Usage != "/handoff" {
		t.Errorf("Spec.Usage = %q, want /handoff", s.Usage)
	}
	if s.Summary == "" {
		t.Errorf("Spec.Summary is empty")
	}
}

// No active ChatSession → reply "No active chat session".
// Stays on Reply (no placeholder card exists yet for the nil-cs
// branch).
func TestFactory_Handle_NoSession_RepliesNoActive(t *testing.T) {
	f := handoffpkg.NewFactory()

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, nil,
		command.SlashInput{ChatID: "no-such-chat"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ackReply(t, out, "No active chat session")
}

// SelectedCwd is empty → RequireActiveCwd short-circuits with
// the canonical "send /cwd first" hint.
func TestFactory_Handle_NoActiveCwd_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := handoffpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"handoff"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ackReply(t, out, "Send /cwd")
}

// Cwd set but no selectedAgent → reply "no active agent; run /use".
// Defensive: ChatSession seeds selectedAgent from the primary
// at construction, so the only way to exercise this branch is
// to construct the chat with an empty primary.
func TestFactory_Handle_NoSelectedAgent_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := handoffpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "")
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"handoff"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ackReply(t, out, "no active agent")
}

// /handoff accepts no flags and no positional args; a stray
// token must surface as a usage error instead of being silently
// dropped (issue #291).
func TestFactory_Handle_RejectsTrailingArgs(t *testing.T) {
	mgr := chatsession.NewManager()
	f := handoffpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID: "c1",
			Args:   []string{"handoff", "extra"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ackReply(t, out, "unexpected positional argument")
}

// /handoff with all preflights green queues exactly one message
// anchored on input.MessageID. The prompt body and Kind are
// asserted by the constant in cmd.go; the count + ack is what
// this test pins down (same shape as the /steer tests in
// internal/command/steer/cmd_test.go).
func TestFactory_Handle_QueuesPrompt(t *testing.T) {
	mgr := chatsession.NewManager()
	f := handoffpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_handoff",
		Text:      "/handoff",
		Args:      []string{"handoff"},
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Ack path: Reply with the queued message; no Outbound yet
	// (the goroutine that does the actual work posts the final
	// reply through cs.Emitter after the Agent finishes).
	ackReply(t, out, "queued")

	if got := cs.QueueLen(); got != 1 {
		t.Fatalf("QueueLen after /handoff: got %d, want 1", got)
	}
}
