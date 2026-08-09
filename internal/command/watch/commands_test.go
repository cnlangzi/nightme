package watch

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/registry"
)

func TestFactory_Spec(t *testing.T) {
	f := NewFactory(chatsession.NewManager())
	s := f.Spec()
	if s.Name != "watch" {
		t.Fatalf("Spec.Name = %q, want watch", s.Name)
	}
	if !strings.Contains(s.Usage, "on") || !strings.Contains(s.Usage, "off") {
		t.Fatalf("Spec.Usage missing on/off: %q", s.Usage)
	}
}

func TestFactory_Handle_NoArgs_ReportsCurrentMode(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	input := command.SlashInput{ChatID: "c1", Args: []string{"watch"}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	if !strings.Contains(out.Reply, "Current watch mode:") {
		t.Fatalf("Reply missing state report: %q", out.Reply)
	}
}

func TestFactory_Handle_OnOffRoundtrip(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	ctx := context.Background()
	cs, _ := mgr.GetOrCreate("c1", "claude")

	on := command.SlashInput{ChatID: "c1", Args: []string{"watch", "on"}}
	out, err := f.Handle(ctx, command.RuntimeServices{}, cs, on)
	if err != nil || !out.Consumed {
		t.Fatalf("Handle on: err=%v consumed=%v", err, out.Consumed)
	}
	if !strings.Contains(out.Reply, "Watch mode set to "+registry.WatchModeAll.String()) {
		t.Fatalf("Reply missing WatchModeAll: %q", out.Reply)
	}

	off := command.SlashInput{ChatID: "c1", Args: []string{"watch", "off"}}
	out, err = f.Handle(ctx, command.RuntimeServices{}, cs, off)
	if err != nil || !out.Consumed {
		t.Fatalf("Handle off: err=%v consumed=%v", err, out.Consumed)
	}
	if !strings.Contains(out.Reply, "Watch mode set to "+registry.WatchModeMention.String()) {
		t.Fatalf("Reply missing WatchModeMention: %q", out.Reply)
	}
}

func TestFactory_Handle_UnknownMode_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	input := command.SlashInput{ChatID: "c1", Args: []string{"watch", "maybe"}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Unknown watch mode") {
		t.Fatalf("Reply missing usage hint: %q", out.Reply)
	}
}