package kill

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
	if s.Name != "kill" {
		t.Fatalf("Spec.Name = %q, want kill", s.Name)
	}
}

func TestFactory_Handle_NoSession_RepliesNoActive(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	input := command.SlashInput{ChatID: "no-such-chat"}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	if !strings.Contains(out.Reply, "No active chat session to kill") {
		t.Fatalf("Reply missing no-active message: %q", out.Reply)
	}
}

func TestFactory_Handle_NoSessionsInPool_RepliesZero(t *testing.T) {
	// /kill on a freshly-created chat with no spawned sessions
	// should report "killed 0" — i.e. FormatKillResults with no
	// rows, not the "no active chat session" path.
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	cs := mgr.GetOrCreate("c1", "claude")
	_ = cs // ensure session exists

	out, err := f.Handle(context.Background(), command.RuntimeServices{},
		command.SlashInput{ChatID: "c1"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	// chatsession.FormatKillResults on an empty list returns
	// the "No active agents to kill." header.
	if !strings.Contains(out.Reply, "No active agents to kill") {
		t.Fatalf("Reply unexpected for empty pool: %q", out.Reply)
	}
}