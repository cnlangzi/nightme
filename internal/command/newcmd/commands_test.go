package newcmd

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

func TestFactory_Spec(t *testing.T) {
	f := NewFactory(chatsession.NewManager(), "claude")
	s := f.Spec()
	if s.Name != "new" {
		t.Fatalf("Spec.Name = %q, want new", s.Name)
	}
}

func TestFactory_Handle_NoActiveCwd_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr, "claude")
	input := command.SlashInput{ChatID: "c1", Args: []string{"new"}}

	cs := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Send /cwd") {
		t.Fatalf("Reply missing cwd hint: %q", out.Reply)
	}
}

func TestFactory_Handle_EmptyAgent_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr, "claude")
	// Pre-populate activeCwd so the preflight passes; the empty
	// string after /new should trigger the usage reply.
	cs := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetActiveCwd("/tmp")

	input := command.SlashInput{ChatID: "c1", Args: []string{"new", ""}}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /new") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
}

func TestFactory_Handle_NoSessions_RepliesNoSession(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr, "claude")
	cs := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetActiveCwd("/tmp")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"new"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "No agent session in current workspace to reset") {
		t.Fatalf("Reply missing no-session message: %q", out.Reply)
	}
}