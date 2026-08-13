//go:build !windows

// Package opencode — Starter (spawn recipe) for the opencode HTTP
// bridge. Adapted to the agent.Info/Starter/Agent/driver three-piece
// model: Starter holds immutable recipe fields; newDriver holds
// runtime state and is wrapped into *agent.Agent at Start time.
//
// Model choice (F-OPENCODE-opencode-bridge §3): opencode is run
// in long-lived mode — one `opencode serve` process per chat session,
// many turns over its lifetime. We expose the bridge surface via
// Starter (template) + *agent.Agent (live handle) just like every
// other bridge in the codebase, even though our internal Start
// path is "one HTTP server, many turns" rather than the typical
// "one CLI process per turn" pattern.
package opencode

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the opencode spawn recipe. Held in agent.Builtins as
// a singleton per agent name.
type Starter struct {
	name    string
	command string
	args    []string
}

// NewStarter constructs the opencode spawn recipe. Entry point used
// at registration time (cmd/nightme/agents.go calls it from init()).
//
// args are the command's protocol flags. They are appended after
// `serve` automatically by newDriver — callers pass bridge-level
// flags only (e.g. the opencode server hostname override). The
// defensive copy means later mutation of the caller's slice does
// not affect us.
func NewStarter(name, command string, args []string) *Starter {
	return &Starter{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
	}
}

// Info returns the fixed metadata for this starter. Observable at
// any time; used by `nightme agents` and any other spec-only
// consumer.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeJSONIO, s.command, s.args, nil)
}

// Spec accessors — the starter itself IS the spec-half; these
// return its immutable fields directly.

func (s *Starter) Name() string    { return s.name }
func (s *Starter) Mode() agent.Mode { return agent.ModeJSONIO }
func (s *Starter) Command() string { return s.command }
func (s *Starter) Args() []string  { return append([]string(nil), s.args...) }
func (s *Starter) Env() []string   { return nil }

// Detect verifies the binary resolves on PATH. Called by Spawner
// before Start; an error aborts session creation with a clear
// "<binary> not found" message to the user.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

// Start spawns the CLI under a PTY-style transport rooted at the
// caller's workspace, opens an SSE subscription against the opencode
// HTTP server, and returns a live *agent.Agent. The caller (typically
// chatsession.AgentSession via the Spawner) must Close() the returned
// handle when done. The Starter is unchanged (reusable across many
// sessions).
//
// cfg.Workspace is the child's cwd. cfg.Args are appended after
// the starter's defaults (user wins). cfg.Env is merged with the
// starter's defaults (cfg wins). cfg.SessionID, when non-empty,
// triggers resume via GET /api/session/{id} before create.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), d.PID(), d.events, d), nil
}

// RunOnce runs a single synchronous turn. Spawns a fresh session,
// sends blocks, drains Events() until the agent produces its final
// text result, closes the session before returning. Used by callers
// like /gtw commit and /gtw pr that want a single turn without
// holding a session handle.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (string, error) {
	a, err := s.Start(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	defer a.Close()
	return agent.RunOnceDrain(ctx, a, blocks, s.Info().Name)
}

// Compact asks the server to compact the conversation history.
// Bridge-specific extension (not on agent.Agent). Callers type-assert
// the *agent.Agent returned by Start back to *Starter (via
// bridge-specific getter) or call directly via the runtime handle's
// unexported path.
//
// To call this from runtime: use the bridge-specific accessor.
//
// We expose it through the Starter's RunOnce-like helpers below.
func (s *Starter) Compact(ctx context.Context, cfg agent.StartConfig) error {
	a, err := s.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer a.Close()
	// Compact is bridge-specific — drive it through a short
	// session, send a sentinel prompt, observe compact event, exit.
	// For now we just trigger the HTTP endpoint directly via the
	// driver; the agent.NewAgent wrapper exposes a way to get
	// the driver back via Driver() (see agent.go).
	d := a.Driver().(interface {
		Compact(ctx context.Context) error
	})
	return d.Compact(ctx)
}

// ListSessions runs a one-shot list-sessions query. Bridge-specific
// extension used by the runtime's resume picker.
//
// Each call spawns and closes a fresh opencode server (just enough
// to hit GET /api/session) — the HTTP server is cheap to spin up
// (~1s) and we don't need the session state to persist.
func (s *Starter) ListSessions(ctx context.Context, cfg agent.StartConfig, limit int) ([]Session, error) {
	a, err := s.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer a.Close()
	d := a.Driver().(interface {
		ListSessions(ctx context.Context, limit int) ([]Session, error)
	})
	return d.ListSessions(ctx, limit)
}

// ListProviders is the bridge-specific one-shot for the /model picker.
func (s *Starter) ListProviders(ctx context.Context, cfg agent.StartConfig) ([]Provider, error) {
	a, err := s.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer a.Close()
	d := a.Driver().(interface {
		ListProviders(ctx context.Context) ([]Provider, error)
	})
	return d.ListProviders(ctx)
}

// ListModels is the bridge-specific one-shot for the /model picker.
func (s *Starter) ListModels(ctx context.Context, cfg agent.StartConfig) (map[string]any, error) {
	a, err := s.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer a.Close()
	d := a.Driver().(interface {
		ListModels(ctx context.Context) (map[string]any, error)
	})
	return d.ListModels(ctx)
}