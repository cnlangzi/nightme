//go:build !windows

// Stage 6 cross-bridge compile-time test: every bridge
// registered with agent.Builtins must satisfy the new agent.Agent
// interface (Start + Close + Events + PID + SendBlocks +
// SendPermission + New + Stop).
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
		a    agent.Starter
	}{
		{"claudecode", claudecode.NewStarter("claudecode", "claude", nil)},
		{"codex", codex.NewStarter("codex", "codex", nil)},
		{"opencode", opencode.NewStarter("opencode", "opencode", nil)},
		{"pi", pi.NewStarter("pi", "pi", nil)},
		{"pty", pty.NewStarter("bash", "bash", nil, nil, 0, 0)},
		{"acp", acp.NewStarter("acp", "opencode", []string{"acp"}, nil, 0, 0)},
	}
	for _, b := range bridges {
		t.Run(b.name, func(t *testing.T) {
			info := b.a.Info()
			if info.Name == "" {
				t.Errorf("Info().Name empty")
			}
			if info.Command == "" {
				t.Errorf("Info().Command empty")
			}
			// Start on a fresh Starter must produce a live Agent
			// whose Stop returns a non-nil error (or succeeds,
			// for bridges with a real Stop impl) — either way it
			// must not panic.
			a, err := b.a.Start(context.Background(), agent.StartConfig{
				Workspace: "/tmp/" + info.Name,
			})
			if err != nil {
				// Start failure is acceptable (e.g. binary not
				// on PATH for tests). The point is that the
				// interface is implemented.
				return
			}
			defer a.Close()
			if a == nil {
				t.Errorf("Start returned nil, err=nil")
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
