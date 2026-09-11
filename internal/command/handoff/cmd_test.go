// Tests for the handoff package's slash command factory.
package handoff_test

import (
	"context"
	"os"
	"path/filepath"
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
// anchored on input.MessageID AND pre-creates <cwd>/.nightme so
// the Agent doesn't burn a turn on mkdir. The prompt body and
// Kind are asserted by the constant in cmd.go; the count + ack
// + .nightme existence is what this test pins down.
func TestFactory_Handle_QueuesPrompt(t *testing.T) {
	mgr := chatsession.NewManager()
	f := handoffpkg.NewFactory()
	dir := t.TempDir()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	if err := cs.SetSelectedCwd(dir); err != nil {
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
	// <cwd>/.nightme must exist synchronously by the time the
	// slash handler returns — otherwise the Agent wastes a turn
	// on mkdir. MkdirAll is idempotent, so a second /handoff on
	// the same workspace is also covered (directory already
	// exists → no error).
	info, err := os.Stat(filepath.Join(dir, ".nightme"))
	if err != nil {
		t.Fatalf("expected %s/.nightme to exist after /handoff, got: %v", dir, err)
	}
	if !info.IsDir() {
		t.Errorf("%s/.nightme is not a directory", dir)
	}
}

// /handoff on a workspace that already has a .nightme directory
// must not error — MkdirAll is a no-op when the path exists.
// Pins down the idempotency contract so re-runs (e.g. after an
// interrupted first attempt) don't blow up.
func TestFactory_Handle_NightmeExists_StillSucceeds(t *testing.T) {
	mgr := chatsession.NewManager()
	f := handoffpkg.NewFactory()
	dir := t.TempDir()
	nightmeDir := filepath.Join(dir, ".nightme")
	if err := os.MkdirAll(nightmeDir, 0o755); err != nil {
		t.Fatalf("setup MkdirAll: %v", err)
	}
	sentinel := filepath.Join(nightmeDir, "config.yml")
	if err := os.WriteFile(sentinel, []byte("existing"), 0o644); err != nil {
		t.Fatalf("setup write sentinel: %v", err)
	}

	cs, _ := mgr.GetOrCreate("c1", "claude")
	if err := cs.SetSelectedCwd(dir); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID:    "c1",
			MessageID: "m_handoff_existing",
			Text:      "/handoff",
			Args:      []string{"handoff"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ackReply(t, out, "queued")
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("pre-existing sentinel was disturbed: %v", err)
	}
}

// input.MessageID == "" → reply with the missing-id diagnostic
// and skip the QueueUserMessage (which silently no-ops on empty
// ID). Mirrors /queue's guard at queue/cmd.go:143.
func TestFactory_Handle_NoMessageID_RepliesDiagnostic(t *testing.T) {
	mgr := chatsession.NewManager()
	f := handoffpkg.NewFactory()
	dir := t.TempDir()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	if err := cs.SetSelectedCwd(dir); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID: "c1",
			Text:   "/handoff",
			Args:   []string{"handoff"},
			// MessageID deliberately empty.
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ackReply(t, out, "missing message id")
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("empty MessageID must not enqueue; got QueueLen=%d", got)
	}
}
