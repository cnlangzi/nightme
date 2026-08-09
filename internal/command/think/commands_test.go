package think

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
	if s.Name != "think" {
		t.Fatalf("Spec.Name = %q, want think", s.Name)
	}
}

func TestFactory_Handle_NoArgs_ReportsCurrentMode(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr, "claude")
	input := command.SlashInput{ChatID: "c1", Args: []string{"think"}}

	cs := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	if !strings.Contains(out.Reply, "Current think mode:") {
		t.Fatalf("Reply missing state report: %q", out.Reply)
	}
}

func TestFactory_Handle_OnOffRoundtrip(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr, "claude")
	ctx := context.Background()
	cs := mgr.GetOrCreate("c1", "claude")

	on := command.SlashInput{ChatID: "c1", Args: []string{"think", "on"}}
	out, err := f.Handle(ctx, command.RuntimeServices{}, cs, on)
	if err != nil || !out.Consumed {
		t.Fatalf("Handle on: err=%v consumed=%v", err, out.Consumed)
	}
	if !strings.Contains(out.Reply, "Think mode set to show") {
		t.Fatalf("Reply missing ThinkModeShow: %q", out.Reply)
	}

	off := command.SlashInput{ChatID: "c1", Args: []string{"think", "off"}}
	out, err = f.Handle(ctx, command.RuntimeServices{}, cs, off)
	if err != nil || !out.Consumed {
		t.Fatalf("Handle off: err=%v consumed=%v", err, out.Consumed)
	}
	if !strings.Contains(out.Reply, "Think mode set to hide") {
		t.Fatalf("Reply missing ThinkModeHide: %q", out.Reply)
	}
}

func TestFactory_Handle_UnknownMode_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr, "claude")
	input := command.SlashInput{ChatID: "c1", Args: []string{"think", "maybe"}}

	cs := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Unknown think mode") {
		t.Fatalf("Reply missing usage hint: %q", out.Reply)
	}
}