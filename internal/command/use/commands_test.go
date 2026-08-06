package use

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
	if s.Name != "use" {
		t.Fatalf("Spec.Name = %q, want use", s.Name)
	}
}

func TestFactory_Handle_NoArgs_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr, "claude")
	input := command.SlashInput{ChatID: "c1", Args: []string{"use"}}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /use") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
}

func TestFactory_Handle_NoActiveCwd_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr, "claude")
	input := command.SlashInput{ChatID: "c1", Args: []string{"use", "claude"}}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, input)
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
	cs := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetActiveCwd("/tmp")

	input := command.SlashInput{ChatID: "c1", Args: []string{"use", "  "}}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /use") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
}