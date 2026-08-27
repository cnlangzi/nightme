package newcmd

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

func TestFactory_Spec(t *testing.T) {
	f := NewFactory()
	s := f.Spec()
	if s.Name != "new" {
		t.Fatalf("Spec.Name = %q, want new", s.Name)
	}
}

func TestFactory_Handle_NoActiveCwd_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	input := command.SlashInput{ChatID: "c1", Args: []string{"new"}}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Send /cwd") {
		t.Fatalf("Reply missing cwd hint: %q", out.Reply)
	}
}

func TestFactory_Handle_EmptyAgent_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	// Pre-populate activeCwd so the preflight passes; the empty
	// string after /new should trigger the usage reply.
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	input := command.SlashInput{ChatID: "c1", Args: []string{"new", ""}}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /new") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
}

func TestFactory_Handle_NoSessions_RepliesNoSession(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"new"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "No agent session in current workspace to reset") {
		t.Fatalf("Reply missing no-session message: %q", out.Reply)
	}
}
// TestFactory_Handle_ArgvContract pins the issue #291 CLI
// contract on /new: a second positional token used to be
// silently dropped (`/new claude codex` reset only claude) and
// an unknown flag used to be treated as an agent name.
func TestFactory_Handle_ArgvContract(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantText string
	}{
		{"extra positional", []string{"new", "claude", "codex"}, "too many arguments"},
		{"unknown flag", []string{"new", "--all"}, "unknown flag"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgr := chatsession.NewManager()
			f := NewFactory()
			cs, _ := mgr.GetOrCreate("c1", "claude")
			if err := cs.SetSelectedCwd("/tmp"); err != nil {
				t.Fatalf("SetSelectedCwd: %v", err)
			}

			out, err := f.Handle(context.Background(), command.RuntimeServices{}, mgr, cs,
				command.SlashInput{ChatID: "c1", Args: c.args})
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if !strings.Contains(out.Reply, c.wantText) {
				t.Fatalf("Reply missing %q: %q", c.wantText, out.Reply)
			}
			if !strings.Contains(out.Reply, "Usage: /new") {
				t.Fatalf("Reply missing usage hint: %q", out.Reply)
			}
		})
	}
}
