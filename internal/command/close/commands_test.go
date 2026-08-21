package close

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

func testCwd(t *testing.T) string {
	t.Helper()
	// SetSelectedCwd cleans; pathutil.Clean matches OS separators.
	return pathutil.Clean("/tmp")
}

func TestFactory_Spec(t *testing.T) {
	f := NewFactory()
	s := f.Spec()
	if s.Name != "close" {
		t.Fatalf("Spec.Name = %q, want close", s.Name)
	}
	if !strings.Contains(s.Usage, "[<agent>]") {
		t.Fatalf("Spec.Usage = %q, want it to mention optional <agent>", s.Usage)
	}
}

func TestFactory_Handle_NoSession_RepliesNoActive(t *testing.T) {
	f := NewFactory()
	input := command.SlashInput{ChatID: "no-such-chat"}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, nil, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatalf("Consumed = false, want true")
	}
	if !strings.Contains(out.Reply, "No active chat session to close") {
		t.Fatalf("Reply missing no-active message: %q", out.Reply)
	}
}

// /close on a freshly-created chat with no activeCwd set — the
// RequireActiveCwd preflight fires before any pool lookup.
func TestFactory_Handle_NoActiveCwd_RepliesHint(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"close"}})
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

// /close with an explicitly empty trailing arg is a usage error —
// silently falling back to activeAgent would surprise users who
// mistyped the agent name.
func TestFactory_Handle_EmptyAgentArg_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	_ = cs.SetSelectedCwd(testCwd(t))

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"close", " "}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /close") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
}

// /close <agent> when <agent> doesn't exist in the chat's pool —
// ErrAgentNotFound from CloseOne is surfaced as a friendly reply.
func TestFactory_Handle_NotInPool_RepliesFriendly(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	cwd := testCwd(t)
	_ = cs.SetSelectedCwd(cwd)
	// Materialize (claude, cwd) so the pool isn't empty.
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"close", "codex"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, `No codex session`) || !strings.Contains(out.Reply, cwd) {
		t.Fatalf("Reply should name the missing agent + cwd: %q", out.Reply)
	}
	// (claude, /tmp) survives — only the targeted entry was closed
	// (and ErrAgentNotFound is a no-op for the pool).
	if got := len(cs.Pool()); got != 1 {
		t.Errorf("pool size after not-found /close: want 1, got %d", got)
	}
}

// /close with no agent arg closes EVERY bridge process in activeCwd
// — the cwd-wide path. With a single entry the active agent happens
// to be the one closed (a coincidence of the single-entry setup),
// but the underlying call is chatsession.CloseAllAgents, not the
// historical "close the active agent" path.
//
// /close kills the bridge process but preserves the AgentSession
// entry in the pool, so the next user message triggers a respawn
// that resumes the conversation via --resume <sessionID>.
func TestFactory_Handle_NoArg_ClosesAllInCwd(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	cwd := testCwd(t)
	_ = cs.SetSelectedCwd(cwd)
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	if got := cs.SelectedAgent(); got != "claude" {
		t.Fatalf("activeAgent: want claude, got %q", got)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"close"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "claude") || !strings.Contains(out.Reply, cwd) {
		t.Fatalf("Reply should name the closed entry: %q", out.Reply)
	}
	// /close preserves the AgentSession entry — next user message
	// will respawn via --resume. Pool size unchanged.
	if got := len(cs.Pool()); got != 1 {
		t.Errorf("pool size after /close: want 1 (session preserved), got %d", got)
	}
}

// /close with no agent arg, multiple entries sharing activeCwd —
// every bridge process is closed. Pool entries are preserved for
// each. Verifies the cwd-wide path at the handler level (the
// underlying chatsession.CloseAllAgents test covers the cwd
// isolation invariant; this one covers the handler dispatch).
func TestFactory_Handle_NoArg_ClosesEveryAgentInCwd(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	cwd := testCwd(t)
	_ = cs.SetSelectedCwd(cwd)
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		t.Fatalf("LookupSelectedAgentSession (claude): %v", err)
	}
	if err := cs.SetSelectedAgent("codex"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		t.Fatalf("LookupSelectedAgentSession (codex): %v", err)
	}
	if got := len(cs.Pool()); got != 2 {
		t.Fatalf("pre: pool size want 2 (claude + codex in %s), got %d", cwd, got)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"close"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Reply should mention both closed entries (FormatResults
	// renders one row per entry).
	if !strings.Contains(out.Reply, "claude") || !strings.Contains(out.Reply, "codex") {
		t.Fatalf("Reply should name both closed entries: %q", out.Reply)
	}
	// Pool size unchanged: every entry is preserved.
	if got := len(cs.Pool()); got != 2 {
		t.Errorf("pool size after /close (cwd-wide): want 2 (sessions preserved), got %d", got)
	}
}

// /close <agent> targets only the named agent's bridge process in
// activeCwd. A sibling entry under a different agent in the same
// cwd survives (its bridge process is untouched). Both entries
// remain in the pool — the named one with the bridge killed, the
// sibling still running.
func TestFactory_Handle_NamedAgent_LeavesSiblingsAlone(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	cwd := testCwd(t)
	_ = cs.SetSelectedCwd(cwd)
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		t.Fatalf("LookupSelectedAgentSession (claude): %v", err)
	}
	if err := cs.SetSelectedAgent("codex"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		t.Fatalf("LookupSelectedAgentSession (codex): %v", err)
	}
	if got := len(cs.Pool()); got != 2 {
		t.Fatalf("pre: pool size want 2, got %d", got)
	}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs,
		command.SlashInput{ChatID: "c1", Args: []string{"close", "codex"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "codex") {
		t.Fatalf("Reply should name codex: %q", out.Reply)
	}
	// Both pool entries survive — /close preserved them.
	if got := len(cs.Pool()); got != 2 {
		t.Errorf("pool size after /close codex: want 2 (both preserved), got %d", got)
	}
}