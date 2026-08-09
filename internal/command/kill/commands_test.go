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
	if !strings.Contains(s.Usage, "[<agent>]") {
		t.Fatalf("Spec.Usage = %q, want it to mention optional <agent>", s.Usage)
	}
}

func TestFactory_Handle_NoSession_RepliesNoActive(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	input := command.SlashInput{ChatID: "no-such-chat"}

	cs := mgr.GetOrCreate(input.ChatID, "test")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, cs, input)
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

// /kill on a freshly-created chat with no activeCwd set — the
// RequireActiveCwd preflight fires before any pool lookup.
func TestFactory_Handle_NoActiveCwd_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	cs := mgr.GetOrCreate("c1", "claude")

	out, err := f.Handle(context.Background(), command.RuntimeServices{},
		cs,
	command.SlashInput{ChatID: "c1", Args: []string{"kill"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	if !strings.Contains(out.Reply, "Send /cwd") {
		t.Fatalf("Reply missing cwd hint: %q", out.Reply)
	}
}

// /kill with an explicitly empty trailing arg is a usage error —
// silently falling back to activeAgent would surprise users who
// mistyped the agent name.
func TestFactory_Handle_EmptyAgentArg_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	cs := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetActiveCwd("/tmp")

	out, err := f.Handle(context.Background(), command.RuntimeServices{},
		cs,
	command.SlashInput{ChatID: "c1", Args: []string{"kill", " "}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /kill") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
}

// /kill <agent> when <agent> doesn't exist in the chat's pool —
// ErrAgentNotFound from KillOne is surfaced as a friendly reply.
func TestFactory_Handle_NotInPool_RepliesFriendly(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	cs := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetActiveCwd("/tmp")
	// Materialize (claude, /tmp) so the pool isn't empty.
	if _, err := cs.LookupActiveAgentSession(); err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{},
		cs,
	command.SlashInput{ChatID: "c1", Args: []string{"kill", "codex"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, `No codex session`) || !strings.Contains(out.Reply, "/tmp") {
		t.Fatalf("Reply should name the missing agent + cwd: %q", out.Reply)
	}
	// (claude, /tmp) survives — only the targeted entry was killed
	// (and ErrAgentNotFound is a no-op for the pool).
	if got := len(cs.Pool()); got != 1 {
		t.Errorf("pool size after not-found /kill: want 1, got %d", got)
	}
}

// /kill with no agent arg kills EVERY entry in activeCwd — the
// cwd-wide path. With a single entry the active agent happens to
// be the one killed (a coincidence of the single-entry setup), but
// the underlying call is chatsession.KillAllAgents, not the
// historical "kill the active agent" path.
func TestFactory_Handle_NoArg_KillsAllInCwd(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	cs := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetActiveCwd("/tmp")
	if _, err := cs.LookupActiveAgentSession(); err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}
	if got := cs.ActiveAgent(); got != "claude" {
		t.Fatalf("activeAgent: want claude, got %q", got)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{},
		cs,
	command.SlashInput{ChatID: "c1", Args: []string{"kill"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "claude") || !strings.Contains(out.Reply, "/tmp") {
		t.Fatalf("Reply should name the killed entry: %q", out.Reply)
	}
	if got := len(cs.Pool()); got != 0 {
		t.Errorf("pool size after /kill: want 0, got %d", got)
	}
}

// /kill with no agent arg, multiple entries sharing activeCwd —
// every entry is killed. Verifies the cwd-wide path at the handler
// level (the underlying chatsession.KillAllAgents test covers the
// cwd isolation invariant; this one covers the handler dispatch).
func TestFactory_Handle_NoArg_KillsEveryAgentInCwd(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	cs := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetActiveCwd("/tmp")
	if _, err := cs.LookupActiveAgentSession(); err != nil {
		t.Fatalf("LookupActiveAgentSession (claude): %v", err)
	}
	if err := cs.SetActiveAgent("codex"); err != nil {
		t.Fatalf("SetActiveAgent: %v", err)
	}
	if _, err := cs.LookupActiveAgentSession(); err != nil {
		t.Fatalf("LookupActiveAgentSession (codex): %v", err)
	}
	if got := len(cs.Pool()); got != 2 {
		t.Fatalf("pre: pool size want 2 (claude + codex in /tmp), got %d", got)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{},
		cs,
	command.SlashInput{ChatID: "c1", Args: []string{"kill"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Reply should mention both killed entries (FormatKillResults
	// renders one row per entry).
	if !strings.Contains(out.Reply, "claude") || !strings.Contains(out.Reply, "codex") {
		t.Fatalf("Reply should name both killed entries: %q", out.Reply)
	}
	if got := len(cs.Pool()); got != 0 {
		t.Errorf("pool size after /kill (cwd-wide): want 0, got %d", got)
	}
}

// /kill <agent> targets only the named agent in activeCwd. A
// sibling entry under a different agent in the same cwd survives
// — the cwd-scoped invariant the redesign locks in.
func TestFactory_Handle_NamedAgent_LeavesSiblingsAlone(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr)
	cs := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetActiveCwd("/tmp")
	if _, err := cs.LookupActiveAgentSession(); err != nil {
		t.Fatalf("LookupActiveAgentSession (claude): %v", err)
	}
	if err := cs.SetActiveAgent("codex"); err != nil {
		t.Fatalf("SetActiveAgent: %v", err)
	}
	if _, err := cs.LookupActiveAgentSession(); err != nil {
		t.Fatalf("LookupActiveAgentSession (codex): %v", err)
	}
	if got := len(cs.Pool()); got != 2 {
		t.Fatalf("pre: pool size want 2, got %d", got)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{},
		cs,
	command.SlashInput{ChatID: "c1", Args: []string{"kill", "codex"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "codex") {
		t.Fatalf("Reply should name codex: %q", out.Reply)
	}
	if got := len(cs.Pool()); got != 1 {
		t.Errorf("pool size after /kill codex: want 1 (claude), got %d", got)
	}
}
