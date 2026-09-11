// Tests for the resume package's slash command factory.
//
// Mirrors internal/command/handoff/cmd_test.go's layout: spec
// sanity, preflight early-exits, the no-handoff / empty-handoff
// branches, and the happy path that proves the resume body
// (prefix + handoff.md contents) lands in the queue.
package resume_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	resumepkg "github.com/cnlangzi/nightme/internal/command/resume"
	"github.com/cnlangzi/nightme/internal/messages"
)

// outReply asserts that out is the OutReply path: Consumed, no
// Reply, exactly one Outbound entry that is messages.OutReply
// anchored on input.MessageID. /resume uses OutReply so the
// channel adapter folds the ack into the same rolling-log card
// as the Agent's continuation (mirrors /queue and /steer).
func outReply(t *testing.T, out *command.SlashOutput, input command.SlashInput) string {
	t.Helper()
	if out == nil {
		t.Fatal("SlashOutput is nil")
	}
	if !out.Consumed {
		t.Error("Consumed = false, want true")
	}
	if len(out.Outbound) != 1 {
		t.Fatalf("Outbound len = %d, want 1", len(out.Outbound))
	}
	ob := out.Outbound[0]
	if ob.Kind != messages.OutReply {
		t.Errorf("Outbound[0].Kind = %s, want %s", ob.Kind, messages.OutReply)
	}
	if ob.ChatID != input.ChatID {
		t.Errorf("Outbound[0].ChatID = %q, want %q", ob.ChatID, input.ChatID)
	}
	if ob.ReplyTo != input.MessageID {
		t.Errorf("Outbound[0].ReplyTo = %q, want %q", ob.ReplyTo, input.MessageID)
	}
	return ob.Text
}

// replyField asserts that out uses the one-shot Reply path
// (Consumed, no Outbound, Reply contains substring). /resume
// uses this for preflight errors and missing-handoff errors
// when no placeholder card has been established yet.
func replyField(t *testing.T, out *command.SlashOutput, contains string) {
	t.Helper()
	if out == nil {
		t.Fatal("SlashOutput is nil")
	}
	if !out.Consumed {
		t.Error("Consumed = false, want true")
	}
	if len(out.Outbound) != 0 {
		t.Errorf("preflight should not produce Outbound entries, got %d", len(out.Outbound))
	}
	if !strings.Contains(out.Reply, contains) {
		t.Errorf("Reply = %q, want substring %q", out.Reply, contains)
	}
}

// TestFactory_Spec covers the static spec advertised in /help.
func TestFactory_Spec(t *testing.T) {
	f := resumepkg.NewFactory()
	s := f.Spec()
	if s.Name != "resume" {
		t.Fatalf("Spec.Name = %q, want resume", s.Name)
	}
	if s.Usage != "/resume" {
		t.Errorf("Spec.Usage = %q, want /resume", s.Usage)
	}
	if s.Summary == "" {
		t.Errorf("Spec.Summary is empty")
	}
}

// No active ChatSession → reply "No active chat session".
func TestFactory_Handle_NoSession_RepliesNoActive(t *testing.T) {
	f := resumepkg.NewFactory()

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, nil,
		command.SlashInput{ChatID: "no-such-chat"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	replyField(t, out, "No active chat session")
}

// SelectedCwd is empty → RequireActiveCwd short-circuits with
// the canonical "send /cwd first" hint. Stays on Reply (no
// placeholder card yet).
func TestFactory_Handle_NoActiveCwd_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := resumepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"resume"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	replyField(t, out, "Send /cwd")
}

// Cwd set but no selectedAgent → reply "no active agent; run /use".
// Defensive: ChatSession seeds selectedAgent from the primary
// at construction, so the only way to exercise this branch is
// to construct the chat with an empty primary.
func TestFactory_Handle_NoSelectedAgent_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := resumepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "")
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"resume"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	replyField(t, out, "no active agent")
}

// /resume accepts no flags and no positional args; a stray
// token must surface as a usage error instead of being silently
// dropped (issue #291).
func TestFactory_Handle_RejectsTrailingArgs(t *testing.T) {
	mgr := chatsession.NewManager()
	f := resumepkg.NewFactory()
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
			Args:   []string{"resume", "extra"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	replyField(t, out, "unexpected positional argument")
}

// /resume with cwd + agent but no ./handoff.md in the workspace
// → OutReply error pointing the user at /handoff. Distinct from
// the preflight Reply path because the queue placeholder was
// already created at MessageQueued time (the framework commander
// emits it for every matched slash command).
func TestFactory_Handle_HandoffMissing_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := resumepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	// Empty temp dir — no handoff.md on disk.
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_resume_missing",
		Text:      "/resume",
		Args:      []string{"resume"},
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	text := outReply(t, out, in)
	if !strings.Contains(text, "handoff.md") {
		t.Errorf("missing-handoff reply should name handoff.md: %q", text)
	}
	if !strings.Contains(text, "/handoff") {
		t.Errorf("missing-handoff reply should suggest /handoff: %q", text)
	}
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("missing handoff must not enqueue anything, got QueueLen=%d", got)
	}
}

// handoff.md exists but is empty → OutReply error, also no
// enqueue. Mirrors the missing-file branch but tells the user
// to re-run /handoff (their previous run produced a blank doc).
func TestFactory_Handle_HandoffEmpty_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := resumepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	dir := t.TempDir()
	if err := cs.SetSelectedCwd(dir); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	// Write a whitespace-only handoff.md.
	if err := os.WriteFile(filepath.Join(dir, "handoff.md"), []byte("   \n\n"), 0o644); err != nil {
		t.Fatalf("seed handoff.md: %v", err)
	}

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_resume_empty",
		Text:      "/resume",
		Args:      []string{"resume"},
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	text := outReply(t, out, in)
	if !strings.Contains(text, "empty") {
		t.Errorf("empty-handoff reply should mention 'empty': %q", text)
	}
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("empty handoff must not enqueue anything, got QueueLen=%d", got)
	}
}

// Happy path: handoff.md exists and contains a real document →
// /resume queues exactly one message (Kind=MessageKindQueue)
// and replies with the OutReply "Resuming…" ack.
func TestFactory_Handle_QueuesResume(t *testing.T) {
	mgr := chatsession.NewManager()
	f := resumepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	dir := t.TempDir()
	if err := cs.SetSelectedCwd(dir); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	doc := "# Handoff\n\n## Task\nFix the bug.\n\n## Completed\n- nothing yet\n"
	if err := os.WriteFile(filepath.Join(dir, "handoff.md"), []byte(doc), 0o644); err != nil {
		t.Fatalf("seed handoff.md: %v", err)
	}

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_resume_ok",
		Text:      "/resume",
		Args:      []string{"resume"},
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	text := outReply(t, out, in)
	if !strings.Contains(text, "Resuming") {
		t.Errorf("ack should mention Resuming: %q", text)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Fatalf("QueueLen after /resume: got %d, want 1", got)
	}
}
