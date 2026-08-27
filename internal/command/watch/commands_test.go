package watch

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
	if s.Name != "watch" {
		t.Fatalf("Spec.Name = %q, want watch", s.Name)
	}
	if !strings.Contains(s.Usage, "on") || !strings.Contains(s.Usage, "off") {
		t.Fatalf("Spec.Usage missing on/off: %q", s.Usage)
	}
}

func TestFactory_Handle_NoArgs_ReportsCurrentMode(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	input := command.SlashInput{ChatID: "c1", Args: []string{"watch"}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input)
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
	f := NewFactory()
	ctx := context.Background()
	cs, _ := mgr.GetOrCreate("c1", "claude")

	on := command.SlashInput{ChatID: "c1", Args: []string{"watch", "on"}}
	out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, on)
	if err != nil || !out.Consumed {
		t.Fatalf("Handle on: err=%v consumed=%v", err, out.Consumed)
	}
	if !strings.Contains(out.Reply, "Watch mode set to "+chatsession.WatchModeAll.String()) {
		t.Fatalf("Reply missing WatchModeAll: %q", out.Reply)
	}

	off := command.SlashInput{ChatID: "c1", Args: []string{"watch", "off"}}
	out, err = f.Handle(ctx, command.RuntimeServices{}, nil, cs, off)
	if err != nil || !out.Consumed {
		t.Fatalf("Handle off: err=%v consumed=%v", err, out.Consumed)
	}
	if !strings.Contains(out.Reply, "Watch mode set to "+chatsession.WatchModeMention.String()) {
		t.Fatalf("Reply missing WatchModeMention: %q", out.Reply)
	}
}

func TestFactory_Handle_UnknownMode_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	input := command.SlashInput{ChatID: "c1", Args: []string{"watch", "maybe"}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Unknown watch mode") {
		t.Fatalf("Reply missing usage hint: %q", out.Reply)
	}
}
// TestFactory_Handle_UnknownFlag_Rejected pins the issue #291
// CLI contract on /watch: an undeclared flag is a hard error,
// not a silent no-op that toggles the mode anyway.
func TestFactory_Handle_UnknownFlag_Rejected(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "test")
	before := cs.WatchMode()

	input := command.SlashInput{ChatID: "c1", Args: []string{"watch", "--mention", "all"}}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "unknown flag") {
		t.Fatalf("Reply missing 'unknown flag': %q", out.Reply)
	}
	if cs.WatchMode() != before {
		t.Fatalf("watch mode changed to %v despite parse error", cs.WatchMode())
	}
}

// TestFactory_Handle_ExtraArgs_Rejected pins the arity check:
// `/watch all mention` used to silently drop "mention".
func TestFactory_Handle_ExtraArgs_Rejected(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "test")
	before := cs.WatchMode()

	input := command.SlashInput{ChatID: "c1", Args: []string{"watch", "all", "mention"}}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "too many arguments") {
		t.Fatalf("Reply missing 'too many arguments': %q", out.Reply)
	}
	if cs.WatchMode() != before {
		t.Fatalf("watch mode changed to %v despite parse error", cs.WatchMode())
	}
}
