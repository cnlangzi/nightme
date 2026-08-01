package claudecode

import (
	"context"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// init registers Claude Code as the only built-in agent shipped with
// nightme v0.2.x. The binary name and arguments match the upstream
// Claude Code CLI; user config can override the command path but
// loses the dedicated JSON-IO bridge (it falls back to PTY).
func init() {
	agent.Builtins.Register(New("claude", "claude", nil))
}

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
// On Start success, the returned session has an active process; the
// caller must Close() it when done.
func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.AgentSession, error) {
	args := append([]string(nil), DefaultArgs...)
	args = append(args, a.args...)
	args = append(args, cfg.Args...)

	env := append([]string(nil), cfg.Env...)
	env = append(env, a.command) // ensure command name is in env (defensive)

	return newSession(ctx, a.command, args, env, cfg.Workspace)
}
