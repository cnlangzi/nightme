// Tests for the handoff package's slash command factory.
package handoff_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	handoffpkg "github.com/cnlangzi/nightme/internal/command/handoff"
	"github.com/cnlangzi/nightme/internal/nightmedir"
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
	if s.Usage != "/handoff <name>" {
		t.Errorf("Spec.Usage = %q, want /handoff <name>", s.Usage)
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
		command.SlashInput{ChatID: "c1", Args: []string{"handoff", "demo"}})
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
		command.SlashInput{ChatID: "c1", Args: []string{"handoff", "demo"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ackReply(t, out, "no active agent")
}

// /handoff requires exactly one positional arg (the name); a
// missing arg must surface as a usage error rather than be
// silently dropped (issue #291).
func TestFactory_Handle_RejectsMissingArg(t *testing.T) {
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
			Args:   []string{"handoff"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ackReply(t, out, "missing argument")
}

// /handoff with more than one positional arg is rejected.
// Mirrors /stop / /gtw close: tail tokens are not silently
// dropped.
func TestFactory_Handle_RejectsExtraArg(t *testing.T) {
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
			Args:   []string{"handoff", "demo", "extra"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ackReply(t, out, "too many arguments")
}

// A name that fails ValidateHandoffName is rejected before any
// mkdir touches disk. The Agent never sees the prompt; the user
// gets the validator's error verbatim.
func TestFactory_Handle_RejectsInvalidName(t *testing.T) {
	homeDir(t)
	mgr := chatsession.NewManager()
	f := handoffpkg.NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	cases := []struct {
		name     string
		input    string
		contains string
	}{
		// Lexer classifies these as positional (don't start with
		// '-'), so they reach ValidateHandoffName and are rejected
		// by its character-set / sentinel / leading-byte rules.
		// "../escape" trips the leading-dot check before the char
		// loop sees the '/' — the dot guard runs first and
		// returns the leading-dot message.
		{"escape_dot_dot", "../escape", "must not start with '.'"},
		{"leading_dot", ".hidden", "must not start with '.'"},
		{"slash_inside", "name/with/slash", "invalid character"},
		{"backslash_inside", `name\backslash`, "invalid character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs2, _ := mgr.GetOrCreate("c1", "claude")
			if err := cs2.SetSelectedCwd(t.TempDir()); err != nil {
				t.Fatalf("SetSelectedCwd: %v", err)
			}
			if err := cs2.SetSelectedAgent("claude"); err != nil {
				t.Fatalf("SetSelectedAgent: %v", err)
			}
			out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs2,
				command.SlashInput{
					ChatID:    "c1",
					MessageID: "m_bad_" + tc.name,
					Text:      "/handoff " + tc.input,
					Args:      []string{"handoff", tc.input},
				})
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			ackReply(t, out, tc.contains)
			if got := cs2.QueueLen(); got != 0 {
				t.Errorf("invalid name must not enqueue; got QueueLen=%d", got)
			}
		})
	}
}

// A name that starts with '-' is rejected by the lexer as an
// unknown flag before ValidateHandoffName ever runs. The error
// message names the offending flag and the Usage string, so the
// user can recover without guessing.
func TestFactory_Handle_RejectsFlagLikeName(t *testing.T) {
	homeDir(t)
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
			ChatID:    "c1",
			MessageID: "m_flag",
			Text:      "/handoff -flag-like",
			Args:      []string{"handoff", "-flag-like"},
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ackReply(t, out, "unknown flag")
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("flag-shaped name must not enqueue; got QueueLen=%d", got)
	}
}

// /handoff with all preflights green queues exactly one message
// anchored on input.MessageID AND pre-creates $HOME/.nightme/
// handoff so the Agent doesn't burn a turn on mkdir. The
// per-user .nightme lives outside any git repo, so no
// .gitignore sync is needed (and none is performed).
func TestFactory_Handle_QueuesPrompt(t *testing.T) {
	homeDir(t)
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
		Text:      "/handoff demo",
		Args:      []string{"handoff", "demo"},
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
	// $HOME/.nightme/handoff must exist synchronously by the
	// time the slash handler returns — otherwise the Agent
	// wastes a turn on mkdir. MkdirAll is idempotent, so a
	// second /handoff on the same HOME is also covered
	// (directory already exists → no error).
	hdir, err := nightmedir.HandoffDir()
	if err != nil {
		t.Fatalf("HandoffDir: %v", err)
	}
	info, err := os.Stat(hdir)
	if err != nil {
		t.Fatalf("expected %s to exist after /handoff, got: %v", hdir, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", hdir)
	}
}

// /handoff on a HOME that already has .nightme/handoff with a
// pre-existing sibling file must not error — MkdirAll is a
// no-op when the path exists. Pins down the idempotency
// contract so re-runs (e.g. after an interrupted first attempt)
// don't blow up, and pre-existing user-managed files in the
// directory are left untouched.
func TestFactory_Handle_HandoffDirExists_StillSucceeds(t *testing.T) {
	home := homeDir(t)
	hdir := filepath.Join(home, ".nightme", "handoff")
	if err := os.MkdirAll(hdir, 0o700); err != nil {
		t.Fatalf("setup MkdirAll: %v", err)
	}
	sentinel := filepath.Join(hdir, "unrelated.txt")
	if err := os.WriteFile(sentinel, []byte("existing"), 0o644); err != nil {
		t.Fatalf("setup write sentinel: %v", err)
	}

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
			ChatID:    "c1",
			MessageID: "m_handoff_existing",
			Text:      "/handoff demo",
			Args:      []string{"handoff", "demo"},
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
	homeDir(t)
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
			Text:   "/handoff demo",
			Args:   []string{"handoff", "demo"},
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

// RenderHandoffPrompt: every {{...}} placeholder resolves to a
// real absolute path; none survive in the rendered output.
func TestRenderHandoffPrompt_SubstitutesPaths(t *testing.T) {
	home := homeDir(t)
	got, err := handoffpkg.RenderHandoffPrompt("demo")
	if err != nil {
		t.Fatalf("RenderHandoffPrompt: %v", err)
	}

	if strings.Contains(got, "{{") || strings.Contains(got, "}}") {
		t.Errorf("rendered prompt contains unreplaced placeholder markers:\n%s", got)
	}

	wantDir := filepath.Join(home, ".nightme", "handoff")
	wantFile := filepath.Join(home, ".nightme", "handoff", "demo.md")
	if !strings.Contains(got, wantDir) {
		t.Errorf("rendered prompt missing %s; got:\n%s", wantDir, got)
	}
	if !strings.Contains(got, wantFile) {
		t.Errorf("rendered prompt missing %s; got:\n%s", wantFile, got)
	}

	// Per-cwd handoff paths must NOT appear anywhere — the
	// whole point of the rewrite is that the handoff lives in
	// $HOME, not in <cwd>/.nightme/. The Agent only sees one
	// canonical path; forbidding the per-cwd form removes a
	// class of "I wrote it locally" mistakes.
	for _, banned := range []string{"./.nightme/", "demo.md (relative", "demo.md (cwd"} {
		if strings.Contains(got, banned) {
			t.Errorf("rendered prompt contains forbidden-path form %q", banned)
		}
	}
	if strings.Contains(got, "./.nightme/handoff/demo.md") {
		t.Errorf("rendered prompt must not include the per-cwd handoff path")
	}
	if strings.Contains(got, "./handoff/demo.md") {
		t.Errorf("rendered prompt must not include a relative-cwd handoff path")
	}
}

// RenderHandoffPrompt: the templated prompt still has the
// structural content the Agent needs (h1, sections, persistence
// rules) — the substitution must not have mangled the body.
func TestRenderHandoffPrompt_PreservesStructure(t *testing.T) {
	homeDir(t)
	got, err := handoffpkg.RenderHandoffPrompt("demo")
	if err != nil {
		t.Fatalf("RenderHandoffPrompt: %v", err)
	}

	want := []string{
		"# Handoff",
		"## Task",
		"## Completed",
		"## In Progress",
		"## Blocked",
		"## Planned",
		"### Confirmed Working",
		"### Unknowns / Risks",
		"## Persistence Requirements",
		"## Output Behavior",
	}
	for _, s := range want {
		if !strings.Contains(got, s) {
			t.Errorf("rendered prompt missing required heading %q", s)
		}
	}
}
