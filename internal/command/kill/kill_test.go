// Tests for the kill package's public API.
//
// These tests exercise kill.KillAgent / kill.KillAllAgents through
// ChatSession's public lifecycle surface only — no private
// injection helpers. The chat session's lifecycle accessors
// (AgentSessionsInCwd, DropAgentSession) and the public
// LookupActiveAgentSession spawn path provide everything we need
// to verify the kill orchestration's behavior at the package
// boundary.
package kill_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	killpkg "github.com/cnlangzi/nightme/internal/command/kill"
)

// fakeCh is a minimal chatsession.Channel for tests in this package.
// Defined inline to avoid a shared test-helpers package; mirrors the
// shape in internal/chatsession/test_helpers_test.go.
type nopCh struct{}
func (nopCh) Send(_ context.Context, _ chatsession.OutboundMessage) error { return nil }
func (nopCh) SendCard(_ context.Context, _ chatsession.OutboundMessage) (string, error) { return "", nil }
func (nopCh) Patch(_ context.Context, _ chatsession.OutboundMessage) error { return nil }
func newTestChannel() chatsession.Channel { return nopCh{} }


// TestKillAgent_NilCS — kill.KillAgent with nil CS returns
// kill.ErrNoContext (defensive — every cmd preflights before
// calling, but the package must not panic).
func TestKillAgent_NilCS(t *testing.T) {
	cs, _ := chatsession.New("chat-nil", "cc", newTestChannel())
	defer cs.WithPersistence(nil, nil)
	// Force the CS reference inside Cmd to be nil.
	_, err := killpkg.KillAgent(&killpkg.Cmd{CS: nil, Ctx: context.Background()}, "cc")
	if err == nil {
		t.Fatal("want error when CS is nil")
	}
	if !strings.Contains(err.Error(), "ChatSession") {
		t.Errorf("want error mentioning ChatSession, got %v", err)
	}
	_ = cs
}

// TestKillAllAgents_NilCS — kill.KillAllAgents with nil CS returns
// kill.ErrNoContext.
func TestKillAllAgents_NilCS(t *testing.T) {
	_, err := killpkg.KillAllAgents(&killpkg.Cmd{CS: nil, Ctx: context.Background()})
	if err == nil {
		t.Fatal("want error when CS is nil")
	}
	if !strings.Contains(err.Error(), "ChatSession") {
		t.Errorf("want error mentioning ChatSession, got %v", err)
	}
}

// TestKillAllAgents_NoActiveCwd — kill.KillAllAgents on a chat
// without an activeCwd is a no-op (no error, nil results). The
// /kill handler preflights with RequireActiveCwd, but the kill
// function itself is well-defined when activeCwd is empty.
func TestKillAllAgents_NoActiveCwd(t *testing.T) {
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	// No SetActiveCwd.

	cmd := &killpkg.Cmd{CS: cs, Ctx: context.Background()}
	results, err := killpkg.KillAllAgents(cmd)
	if err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}
	if results != nil {
		t.Errorf("want nil results when activeCwd empty, got %v", results)
	}
}

// TestKillAgent_NotFound — kill.KillAgent with an agent not in
// the pool returns chatsession.ErrAgentNotFound.
func TestKillAgent_NotFound(t *testing.T) {
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	cs.WithPersistence(nil, nil)
	if err := cs.SetActiveCwd("/tmp"); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	// No LookupActiveAgentSession — pool is empty.

	cmd := &killpkg.Cmd{CS: cs, Ctx: context.Background()}
	_, err := killpkg.KillAgent(cmd, "claude")
	if !errors.Is(err, chatsession.ErrAgentNotFound) {
		t.Fatalf("want ErrAgentNotFound, got %v", err)
	}
}

// TestKillAllAgents_EmptyPool — kill.KillAllAgents on a chat with
// activeCwd set but an empty pool returns (nil, nil).
func TestKillAllAgents_EmptyPool(t *testing.T) {
	mgr := chatsession.NewManager()
	cs, _ := mgr.GetOrCreate("c1", "claude")
	cs.WithPersistence(nil, nil)
	if err := cs.SetActiveCwd("/tmp"); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}

	cmd := &killpkg.Cmd{CS: cs, Ctx: context.Background()}
	results, err := killpkg.KillAllAgents(cmd)
	if err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}
	if results != nil {
		t.Errorf("want nil results for empty pool, got %v", results)
	}
}

// TestFormatKillResults_Empty — kill.FormatKillResults on nil/empty
// renders the canonical "No active agents to kill." message that
// the /kill handler relies on for the empty-state reply.
func TestFormatKillResults_Empty(t *testing.T) {
	cases := []struct {
		name   string
		inputs []killpkg.Result
		want   string
	}{
		{"nil slice", nil, "No active agents to kill."},
		{"empty slice", []killpkg.Result{}, "No active agents to kill."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := killpkg.FormatKillResults(tc.inputs)
			if got != tc.want {
				t.Errorf("want %q, got %q", tc.want, got)
			}
		})
	}
}

// TestFormatKillResults_Killed — kill.FormatKillResults renders
// killed entries with the ✓ icon and (agent, cwd) annotation.
func TestFormatKillResults_Killed(t *testing.T) {
	results := []killpkg.Result{
		{Agent: "claude", Cwd: "/code/A", BeforeState: chatsession.StatusRunning, Action: "killed"},
	}
	got := killpkg.FormatKillResults(results)
	if !strings.Contains(got, "claude") || !strings.Contains(got, "/code/A") {
		t.Errorf("want reply to name killed entry, got %q", got)
	}
	if !strings.Contains(got, "Stopped 1") {
		t.Errorf("want header mentioning 'Stopped 1', got %q", got)
	}
}

// TestHandler_NoSession — the /kill handler with a nil cs
// replies with the canonical "No active chat session to kill."
// message.
func TestHandler_NoSession(t *testing.T) {
	mgr := chatsession.NewManager()
	f := killpkg.NewFactory(mgr)

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil,
		command.SlashInput{ChatID: "c1", Args: []string{"kill"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !out.Consumed {
		t.Fatal("want Consumed=true")
	}
	if !strings.Contains(out.Reply, "No active chat session to kill") {
		t.Errorf("want reply mentioning 'No active chat session to kill', got %q", out.Reply)
	}
}