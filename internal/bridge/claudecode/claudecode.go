package claudecode

import (
	"context"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Agent is the agent.Agent descriptor for Claude Code. It returns
// agent.ModeJSONIO and spawns a stream-json session on Start.
//
// ModeJSONIO is a new value in the agent.Mode enum (added for v0.2 to
// distinguish Claude Code's bespoke JSON-IO from generic ACP / SDK /
// PTY modes). See docs/feat/F-24-claudecode-bridge.md §8.3.
type Agent struct {
	name    string
	command string
	args    []string
}

// New constructs a Claude Code agent descriptor. name is the registry key
// (typically "claude"); command is the CLI binary name on PATH (typically
// "claude"); args are extra flags appended after DefaultArgs.
func New(name, command string, args []string) *Agent {
	return &Agent{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
	}
}

// Name returns the registry key.
func (a *Agent) Name() string { return a.name }

// Mode reports the JSON-IO mode (introduced for v0.2).
func (a *Agent) Mode() agent.Mode { return agent.ModeJSONIO }

// Command returns the configured CLI binary (typically "claude").
// Surfaced by `nightme agents` so users can see what /run would spawn.
func (a *Agent) Command() string { return a.command }

// Args returns a defensive copy of the constructor args. Callers may
// not mutate the returned slice.
func (a *Agent) Args() []string {
	return append([]string(nil), a.args...)
}

// Detect verifies the `claude` binary resolves on PATH. Call before Start
// to surface a friendly "claude not installed" error rather than a
// confusing exec failure.
func (a *Agent) Detect() error {
	_, err := exec.LookPath(a.command)
	return err
}

// Start spawns Claude Code in stream-json mode and returns an
// AgentSession that streams parsed events on its Events channel.
//
// Workspace is the child process's cwd. cfg.Args are appended after the
// agent's defaults (DefaultArgs + a.args). cfg.Env is appended to
// os.Environ() for the child.
//
// cfg.PermissionMode overrides the --permission-mode flag baked into
// DefaultArgs. Empty string falls back to PermissionBypass (preserves
// v0.1 behaviour). Unknown values are forwarded as-is — Claude Code
// itself validates the set of legal modes.
//
// On Start success, the returned session has an active process; the
// caller must Close() it when done.
func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.AgentSession, error) {
	args := buildArgs(a.args, cfg)

	env := append([]string(nil), cfg.Env...)
	env = append(env, a.command) // ensure command name is in env (defensive)

	return newSession(ctx, a.name, a.command, args, env, cfg.Workspace)
}

// buildArgs concatenates DefaultArgs + extraArgs + cfg.Args, rewriting
// the --permission-mode placeholder baked into DefaultArgs with the
// effective mode from cfg.PermissionMode (PermissionBypass when empty).
//
// Extracted as a package-private helper so tests can assert on the
// produced argv without spawning a process. Mirrors the contract of
// Agent.Start exactly.
func buildArgs(extraArgs []string, cfg agent.StartConfig) []string {
	mode := cfg.PermissionMode
	if mode == "" {
		mode = PermissionBypass
	}

	// Walk DefaultArgs; when we see "--permission-mode" the next
	// element is the placeholder — replace it with the effective
	// mode instead of copying the placeholder verbatim.
	out := make([]string, 0, len(DefaultArgs)+len(extraArgs)+len(cfg.Args))
	for i := 0; i < len(DefaultArgs); i++ {
		if DefaultArgs[i] == "--permission-mode" && i+1 < len(DefaultArgs) {
			out = append(out, "--permission-mode", mode)
			i++ // skip the placeholder value
			continue
		}
		out = append(out, DefaultArgs[i])
	}
	out = append(out, extraArgs...)
	out = append(out, cfg.Args...)
	return out
}
