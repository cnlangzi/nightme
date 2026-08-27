package use

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
	if s.Name != "use" {
		t.Fatalf("Spec.Name = %q, want use", s.Name)
	}
}

func TestFactory_Handle_NoArgs_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "test")
	input := command.SlashInput{ChatID: "c1", Args: []string{"use"}}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /use") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
}

func TestFactory_Handle_NoActiveCwd_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "test")
	input := command.SlashInput{ChatID: "c1", Args: []string{"use", "claude"}}

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
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd("/tmp")

	input := command.SlashInput{ChatID: "c1", Args: []string{"use", "  "}}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /use") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
}

// TestFactory_Handle_ExtraArgs_Rejected pins the issue #291
// arity check. Pre-#291 the Usage string advertised
// `/use <agent> [args...]` and Args[2:] was silently dropped —
// so `/use codex --auto-approve` looked like it had applied a
// spawn flag when nothing forwarded it. Both shapes are now
// hard errors.
func TestFactory_Handle_ExtraArgs_Rejected(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantText string
	}{
		{"extra positional", []string{"use", "claude", "extra"}, "too many arguments"},
		{"flag-shaped tail", []string{"use", "codex", "--auto-approve"}, "unknown flag"},
		{"leading unknown flag", []string{"use", "--auto-approve", "codex"}, "unknown flag"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgr := chatsession.NewManager()
			f := NewFactory()
			cs, _ := mgr.GetOrCreate("c1", "seeded")
			_ = cs.SetSelectedCwd("/tmp")

			input := command.SlashInput{ChatID: "c1", Args: c.args}
			out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if !strings.Contains(out.Reply, c.wantText) {
				t.Fatalf("Reply missing %q: %q", c.wantText, out.Reply)
			}
			if !strings.Contains(out.Reply, "Usage: /use") {
				t.Fatalf("Reply missing usage hint: %q", out.Reply)
			}
			// A parse error must not half-apply the switch: the
			// previously selected agent stays selected.
			if cs.SelectedAgent() != "seeded" {
				t.Fatalf("selectedAgent = %q, want unchanged (seeded) after a parse error", cs.SelectedAgent())
			}
		})
	}
}
