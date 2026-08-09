// Stage 6 cross-bridge compile-time test: every bridge
// registered with agent.Builtins must satisfy the new agent.Agent
// interface (Start + Close + Events + PID + SendText + SendBlocks +
// SendPermission + New + Abort + SetModel).
//
// Lives in package agent_test so we can import the bridge
// packages without creating an import cycle, and so a bridge
// forgetting to implement one of the new methods fails the
// *compile* of this test, not just runtime.
package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
	"github.com/cnlangzi/nightme/internal/bridge/claudecode"
	"github.com/cnlangzi/nightme/internal/bridge/codex"
	"github.com/cnlangzi/nightme/internal/bridge/opencode"
	"github.com/cnlangzi/nightme/internal/bridge/pi"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
)

// TestBuiltinBridges_SatisfyAgentInterface is a compile-time +
// runtime check that every builtin bridge template satisfies the
// agent.Agent interface. Build failure means somebody added a
// method to the interface without also implementing it on the
// bridge side.
func TestBuiltinBridges_SatisfyAgentInterface(t *testing.T) {
	bridges := []struct {
		name string
		a    agent.Agent
	}{
		{"claudecode", claudecode.New("claudecode", "claude", nil)},
		{"codex", codex.New("codex", "codex", nil)},
		{"opencode", opencode.New("opencode", "opencode", nil)},
		{"pi", pi.New("pi", "pi", nil)},
		{"pty", pty.NewAgent("bash", "bash", nil, nil)},
		{"acp", acp.NewAgent("acp", "opencode", []string{"acp"})},
	}
	for _, b := range bridges {
		t.Run(b.name, func(t *testing.T) {
			if b.a.Name() == "" {
				t.Errorf("Name() empty")
			}
			if b.a.Command() == "" {
				t.Errorf("Command() empty")
			}
			// Abort on an unstarted bridge must return a
			// non-nil error so the runtime can surface it.
			if err := b.a.Abort(context.Background()); err == nil {
				t.Errorf("Abort on unstarted bridge = nil, want error")
			}
		})
	}
}

// TestErrNotSupported_AndResetRequired are sanity checks so
// future callers can rely on sentinel error semantics.
func TestErrNotSupported_AndResetRequired(t *testing.T) {
	if agent.ErrNotSupported == nil {
		t.Fatal("ErrNotSupported is nil")
	}
	if !errors.Is(agent.ErrNotSupported, agent.ErrNotSupported) {
		t.Errorf("errors.Is broken for ErrNotSupported")
	}
	if agent.ErrRestartRequired == nil {
		t.Fatal("ErrRestartRequired is nil")
	}
}
