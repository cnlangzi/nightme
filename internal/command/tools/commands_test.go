package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

func TestFactory_Spec(t *testing.T) {
	f := NewFactory(chatsession.NewManager())
	s := f.Spec()
	if s.Name != "tools" {
		t.Fatalf("Spec.Name = %q, want tools", s.Name)
	}
}

func TestFactory_Handle_NoArgs_ReportsCurrentMode(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	input := command.SlashInput{ChatID: "c1", Args: []string{"tools"}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	if !strings.Contains(out.Reply, "Current tools mode:") {
		t.Fatalf("Reply missing state report: %q", out.Reply)
	}
}

func TestFactory_Handle_OnOffRoundtrip(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	ctx := context.Background()
	cs, _ := mgr.GetOrCreate("c1", "claude")

	on := command.SlashInput{ChatID: "c1", Args: []string{"tools", "on"}}
	out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, on)
	if err != nil || !out.Consumed {
		t.Fatalf("Handle on: err=%v consumed=%v", err, out.Consumed)
	}
	if !strings.Contains(out.Reply, "Tools mode set to "+chatsession.ToolsModeShow.String()) {
		t.Fatalf("Reply missing ToolsModeShow: %q", out.Reply)
	}

	off := command.SlashInput{ChatID: "c1", Args: []string{"tools", "off"}}
	out, err = f.Handle(ctx, command.RuntimeServices{}, nil, cs, off)
	if err != nil || !out.Consumed {
		t.Fatalf("Handle off: err=%v consumed=%v", err, out.Consumed)
	}
	if !strings.Contains(out.Reply, "Tools mode set to "+chatsession.ToolsModeHide.String()) {
		t.Fatalf("Reply missing ToolsModeHide: %q", out.Reply)
	}
}

func TestFactory_Handle_UnknownMode_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	input := command.SlashInput{ChatID: "c1", Args: []string{"tools", "maybe"}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Unknown tools mode") {
		t.Fatalf("Reply missing usage hint: %q", out.Reply)
	}
}