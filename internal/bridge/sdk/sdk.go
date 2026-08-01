// Package sdk contains adapters for vendor SDKs that expose a native
// structured agent session. Claude Code's official Agent SDK currently ships
// only Python and TypeScript bindings, so the Go adapter reports a precise
// fallback error until an official Go binding is available.
package sdk

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ErrNotImplemented indicates that the vendor SDK is unavailable for this
// language. Callers should use the PTY adapter for the same CLI instead.
var ErrNotImplemented = errors.New("bridge/sdk: Claude Code Agent SDK has no official Go binding; use PTY")

// Agent describes an SDK-backed vendor agent. command is retained for
// capability detection and for the eventual CLI fallback configuration.
type Agent struct {
	name    string
	command string
	args    []string
}

// New constructs an SDK agent. The optional args preserve compatibility with
// the v0.1 two-argument constructor while allowing registry entries to retain
// vendor-specific launch arguments.
func New(name, command string, args ...[]string) *Agent {
	a := &Agent{name: name, command: command}
	if len(args) > 0 {
		a.args = append([]string(nil), args[0]...)
	}
	return a
}

func (a *Agent) Name() string { return a.name }

func (a *Agent) Mode() agent.Mode { return agent.ModeSDK }

// Command returns the configured CLI binary. Surfaced by `nightme agents`.
func (a *Agent) Command() string { return a.command }

// Args returns a defensive copy of the constructor args. Callers may
// not mutate the returned slice.
func (a *Agent) Args() []string {
	return append([]string(nil), a.args...)
}

// Detect verifies that the configured CLI is present. The SDK itself is not a
// Go package, but checking the CLI gives the user a useful configuration error
// before Start returns the SDK availability sentinel.
func (a *Agent) Detect() error {
	if a.command == "" {
		return errors.New("bridge/sdk: empty Claude Code command")
	}
	_, err := exec.LookPath(a.command)
	return err
}

func (a *Agent) Start(context.Context, agent.StartConfig) (agent.AgentSession, error) {
	return nil, fmt.Errorf("%w: configured command %q", ErrNotImplemented, a.command)
}

var _ agent.Agent = (*Agent)(nil)
