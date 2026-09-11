// Tests for the resume package's slash command factory.
package resume_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	resumepkg "github.com/cnlangzi/nightme/internal/command/resume"
	"github.com/cnlangzi/nightme/internal/messages"
)

// homeDir sandboxes $HOME (and USERPROFILE on Windows) to a temp
// dir for the duration of the test, so nightmedir.HandoffDir()
// resolves inside the sandbox rather than touching the real
// user's home. Returns the temp home path so callers can build
// expected absolute paths against it.
func homeDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	return home
}

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
	if s.Usage != "/resume <name>" {
		t.Errorf("Spec.Usage = %q, want /resume <name>", s.Usage)
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
		command.SlashInput{ChatID: "c1", Args: []string{"resume", "demo"}})
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
		command.SlashInput{ChatID: "c1", Args: []string{"resume", "demo"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	replyField(t, out, "no active agent")
}

// /resume requires exactly one positional arg (the name); a
// missing arg is rejected with a usage error.
func TestFactory_Handle_RejectsMissingArg(t *testing.T) {
	homeDir(t)
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
			Args:   []string{"resume"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	replyField(t, out, "missing argument")
}

// /resume rejects extra positional args; mirroring /stop and
// /handoff.
func TestFactory_Handle_RejectsExtraArg(t *testing.T) {
	homeDir(t)
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
			Args:   []string{"resume", "demo", "extra"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	replyField(t, out, "too many arguments")
}

// /resume rejects a name that fails ValidateHandoffName before
// the file system is touched.
func TestFactory_Handle_RejectsInvalidName(t *testing.T) {
	homeDir(t)
	mgr := chatsession.NewManager()
	f := resumepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_bad_name",
		Text:      "/resume ../escape",
		Args:      []string{"resume", "../escape"},
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	text := outReply(t, out, in)
	// "../escape" trips the leading-dot guard before the char
	// loop sees the '/' — both /resume and /handoff tests pin
	// this so a future re-order of ValidateHandoffName's checks
	// would surface as a test diff rather than a silent change.
	if !strings.Contains(text, "must not start with '.'") {
		t.Errorf("expected validator error in %q", text)
	}
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("invalid name must not enqueue; got QueueLen=%d", got)
	}
}

// /resume with cwd + agent but no ~/.nightme/handoff/demo.md on
// disk → OutReply error pointing the user at /handoff. Distinct
// from the preflight Reply path because the queue placeholder
// was already created at MessageQueued time (the framework
// commander emits it for every matched slash command).
func TestFactory_Handle_HandoffMissing_RepliesHint(t *testing.T) {
	homeDir(t)
	mgr := chatsession.NewManager()
	f := resumepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_resume_missing",
		Text:      "/resume demo",
		Args:      []string{"resume", "demo"},
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	text := outReply(t, out, in)
	if !strings.Contains(text, ".nightme/handoff/demo.md") {
		t.Errorf("missing-handoff reply should name .nightme/handoff/demo.md: %q", text)
	}
	if !strings.Contains(text, "/handoff") {
		t.Errorf("missing-handoff reply should suggest /handoff: %q", text)
	}
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("missing handoff must not enqueue anything, got QueueLen=%d", got)
	}
}

// ~/.nightme/handoff/demo.md exists but is zero bytes →
// OutReply error, also no enqueue. Mirrors the missing-file
// branch but tells the user to re-run /handoff.
func TestFactory_Handle_HandoffEmpty_RepliesHint(t *testing.T) {
	home := homeDir(t)
	mgr := chatsession.NewManager()
	f := resumepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	hdir := filepath.Join(home, ".nightme", "handoff")
	if err := os.MkdirAll(hdir, 0o700); err != nil {
		t.Fatalf("MkdirAll handoff dir: %v", err)
	}
	// Truly empty (size==0). Whitespace-only is NOT empty here:
	// the prompt's step 1 reads the file itself, so a stub with
	// whitespace would just confuse the Agent.
	if err := os.WriteFile(filepath.Join(hdir, "demo.md"), []byte{}, 0o644); err != nil {
		t.Fatalf("seed empty handoff: %v", err)
	}

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_resume_empty",
		Text:      "/resume demo",
		Args:      []string{"resume", "demo"},
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

// Happy path: ~/.nightme/handoff/demo.md exists and contains a
// real document → /resume queues exactly one message
// (Kind=MessageKindQueue) and replies with the OutReply
// "Resuming…" ack.
func TestFactory_Handle_QueuesResume(t *testing.T) {
	home := homeDir(t)
	mgr := chatsession.NewManager()
	f := resumepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	hdir := filepath.Join(home, ".nightme", "handoff")
	if err := os.MkdirAll(hdir, 0o700); err != nil {
		t.Fatalf("MkdirAll handoff dir: %v", err)
	}
	doc := "# Handoff\n\n## Task\nFix the bug.\n\n## Completed\n- nothing yet\n"
	if err := os.WriteFile(filepath.Join(hdir, "demo.md"), []byte(doc), 0o644); err != nil {
		t.Fatalf("seed handoff: %v", err)
	}

	in := command.SlashInput{
		ChatID:    "c1",
		MessageID: "m_resume_ok",
		Text:      "/resume demo",
		Args:      []string{"resume", "demo"},
	}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, in)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	text := outReply(t, out, in)
	if !strings.Contains(text, "Resuming") {
		t.Errorf("ack should mention Resuming: %q", text)
	}
	if !strings.Contains(text, ".nightme/handoff/demo.md") {
		t.Errorf("ack should name the slash-form handoff path: %q", text)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Fatalf("QueueLen after /resume: got %d, want 1", got)
	}
}

// input.MessageID == "" → reply with the missing-id diagnostic
// and skip the QueueUserMessage (which silently no-ops on empty
// ID). Mirrors /queue's guard at queue/cmd.go:143.
func TestFactory_Handle_NoMessageID_RepliesDiagnostic(t *testing.T) {
	home := homeDir(t)
	mgr := chatsession.NewManager()
	f := resumepkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	// Need a real handoff on disk so the test reaches the
	// MessageID guard, not the missing-handoff error branch.
	hdir := filepath.Join(home, ".nightme", "handoff")
	if err := os.MkdirAll(hdir, 0o700); err != nil {
		t.Fatalf("MkdirAll handoff dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hdir, "demo.md"), []byte("doc"), 0o644); err != nil {
		t.Fatalf("seed handoff: %v", err)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{
			ChatID: "c1",
			Text:   "/resume demo",
			Args:   []string{"resume", "demo"},
			// MessageID deliberately empty.
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	text := outReply(t, out, command.SlashInput{ChatID: "c1"})
	if !strings.Contains(text, "missing message id") {
		t.Errorf("expected missing-id diagnostic in %q", text)
	}
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("empty MessageID must not enqueue; got QueueLen=%d", got)
	}
}

// RenderResumePrompt: every {{...}} placeholder resolves; no
// per-cwd / forbidden-path form leaks into the rendered prompt.
func TestRenderResumePrompt_SubstitutesPath(t *testing.T) {
	home := homeDir(t)
	got, err := resumepkg.RenderResumePrompt("demo")
	if err != nil {
		t.Fatalf("RenderResumePrompt: %v", err)
	}

	if strings.Contains(got, "{{") || strings.Contains(got, "}}") {
		t.Errorf("rendered prompt contains unreplaced placeholders:\n%s", got)
	}

	wantFile := filepath.Join(home, ".nightme", "handoff", "demo.md")
	if !strings.Contains(got, wantFile) {
		t.Errorf("rendered prompt missing %s; got:\n%s", wantFile, got)
	}

	// Per-cwd handoff forms must not appear — the resume
	// prompt only points the Agent at the canonical home path.
	if strings.Contains(got, "./.nightme/handoff/demo.md") {
		t.Errorf("rendered prompt must not include a per-cwd handoff path")
	}
	if strings.Contains(got, "./handoff/demo.md") {
		t.Errorf("rendered prompt must not include a relative-cwd handoff path")
	}
	if strings.Contains(got, "CURRENT PROJECT") {
		t.Errorf("rendered prompt must not pin the handoff to a single project")
	}
}
