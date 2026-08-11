// Tests for the close package's public API.
//
// These tests exercise close.CloseAgent / close.CloseAllAgents through
// ChatSession's public lifecycle surface only — no private
// injection helpers. The chat session's lifecycle accessors
// (AgentSessionsInCwd, DropAgentSession) and the public
// LookupSelectedAgentSession spawn path provide everything we need
// to verify the close orchestration's behavior at the package
// boundary.
package close_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	closepkg "github.com/cnlangzi/nightme/internal/command/close"
)

// TestCloseAgent_NilCS — close.CloseAgent with nil CS returns
// close.ErrNoContext (defensive — every cmd preflights before
// calling, but the package must not panic).
// nopCh satisfies chatsession.Channel for tests that need a
// non-nil channel to construct a ChatSession but don't exercise
// the channel surface.
type nopCh struct{}

func (nopCh) Send(_ context.Context, _ chatsession.OutboundMessage) error { return nil }
func (nopCh) SendCard(_ context.Context, _ chatsession.OutboundMessage) (string, error) {
	return "", nil
}
func (nopCh) Patch(_ context.Context, _ chatsession.OutboundMessage) error { return nil }

func TestCloseAgent_NilCS(t *testing.T) {
	cs, _ := chatsession.New("chat-nil", "cc", nopCh{})
	defer cs.WithPersistence(nil, nil)
	// Force the CS reference inside Cmd to be nil.
	_, err := closepkg.CloseAgent(&closepkg.Cmd{CS: nil, Ctx: context.Background()}, "cc")
	if err == nil {
		t.Fatal("want error when CS is nil")
	}
	if !strings.Contains(err.Error(), "ChatSession") {
		t.Errorf("want error mentioning ChatSession, got %v", err)
	}
	_ = cs
}

// TestCloseAllAgents_NilCS — close.CloseAllAgents with nil CS returns
// close.ErrNoContext.
func TestCloseAllAgents_NilCS(t *testing.T) {
	_, err := closepkg.CloseAllAgents(&closepkg.Cmd{CS: nil, Ctx: context.Background()})
	if err == nil {
		t.Fatal("want error when CS is nil")
	}
	if !strings.Contains(err.Error(), "ChatSession") {
		t.Errorf("want error mentioning ChatSession, got %v", err)
	}
}

// TestCloseAllAgents_NoActiveCwd — close.CloseAllAgents on a chat
// without an activeCwd is a no-op (no error, nil results). The
// /close handler preflights with RequireActiveCwd, but the close
// function itself is well-defined when activeCwd is empty.
func TestCloseAllAgents_NoActiveCwd(t *testing.T) {
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	// No SetSelectedCwd.

	cmd := &closepkg.Cmd{CS: cs, Ctx: context.Background()}
	results, err := closepkg.CloseAllAgents(cmd)
	if err != nil {
		t.Fatalf("CloseAllAgents: %v", err)
	}
	if results != nil {
		t.Errorf("want nil results when activeCwd empty, got %v", results)
	}
}

// TestCloseAgent_NotFound — close.CloseAgent with an agent not in
// the pool returns chatsession.ErrAgentNotFound.
func TestCloseAgent_NotFound(t *testing.T) {
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	cs.WithPersistence(nil, nil)
	if err := cs.SetSelectedCwd("/tmp"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	// No LookupSelectedAgentSession — pool is empty.

	cmd := &closepkg.Cmd{CS: cs, Ctx: context.Background()}
	_, err := closepkg.CloseAgent(cmd, "claude")
	if !errors.Is(err, chatsession.ErrAgentNotFound) {
		t.Fatalf("want ErrAgentNotFound, got %v", err)
	}
}

// TestCloseAllAgents_EmptyPool — close.CloseAllAgents on a chat with
// activeCwd set but an empty pool returns (nil, nil).
func TestCloseAllAgents_EmptyPool(t *testing.T) {
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	cs.WithPersistence(nil, nil)
	if err := cs.SetSelectedCwd("/tmp"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	cmd := &closepkg.Cmd{CS: cs, Ctx: context.Background()}
	results, err := closepkg.CloseAllAgents(cmd)
	if err != nil {
		t.Fatalf("CloseAllAgents: %v", err)
	}
	if results != nil {
		t.Errorf("want nil results for empty pool, got %v", results)
	}
}

// TestFormatResults_Empty — close.FormatResults on nil/empty
// renders the canonical "No active agents to close." message that
// the /close handler relies on for the empty-state reply.
func TestFormatResults_Empty(t *testing.T) {
	cases := []struct {
		name   string
		inputs []closepkg.Result
		want   string
	}{
		{"nil slice", nil, "No active agents to close."},
		{"empty slice", []closepkg.Result{}, "No active agents to close."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := closepkg.FormatResults(tc.inputs)
			if got != tc.want {
				t.Errorf("want %q, got %q", tc.want, got)
			}
		})
	}
}

// TestFormatResults_Closed — close.FormatResults renders
// closed entries with the ✓ icon and (agent, cwd) annotation.
func TestFormatResults_Closed(t *testing.T) {
	results := []closepkg.Result{
		{Agent: "claude", Cwd: "/code/A", BeforeState: chatsession.StatusRunning, Action: "closed"},
	}
	got := closepkg.FormatResults(results)
	if !strings.Contains(got, "claude") || !strings.Contains(got, "/code/A") {
		t.Errorf("want reply to name closed entry, got %q", got)
	}
	if !strings.Contains(got, "Closed 1") {
		t.Errorf("want header mentioning 'Closed 1', got %q", got)
	}
}

// TestHandler_NoSession — the /close handler with an unknown chat
// ID replies with the canonical "No active chat session to close."
// message.
func TestHandler_NoSession(t *testing.T) {
	mgr := chatsession.NewManager()
	f := closepkg.NewFactory(mgr)

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil,
		command.SlashInput{ChatID: "no-such-chat"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatal("want Consumed=true")
	}
	if !strings.Contains(out.Reply, "No active chat session to close") {
		t.Errorf("want reply mentioning 'No active chat session to close', got %q", out.Reply)
	}
}