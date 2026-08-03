// Package pi implements a bridge to the Pi coding agent using its
// native `pi --mode rpc` long-lived JSONL protocol.
//
// Pi does not speak ACP natively. The most portable way to drive it
// from a non-Node host is the official RPC mode: a real stdio pipe
// carrying strictly LF-delimited JSON commands, responses, and events.
// This package owns that protocol: it spawns the binary, drives the
// request/response correlation, and translates Pi events into
// agent.AgentEvent values consumed by the rest of nightme.
//
// Design reference: docs/feat/F-32-pi-rpc-bridge.md.
package pi

import (
	"context"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// DefaultArgs is the canonical argv used when spawning `pi` in RPC
// mode. The flag set is intentionally minimal: --mode rpc is the only
// behavioral switch. We deliberately do not pass --model or
// --thinking; selection is the user's choice via Pi's own config or
// future /use flags. We do not pass --permission-mode either; Pi
// has no equivalent of Claude Code's bypassPermissions, and the
// F-32 MVP does not support /abort or extension UI forwarding.
var DefaultArgs = []string{
	"--mode", "rpc",
}

// Agent is the agent.Agent descriptor for Pi. It returns
// agent.ModeJSONIO because the bridge drives Pi over a structured
// JSON I/O channel, even though the wire format is Pi's own
// command/response/event JSONL rather than Claude Code stream-json.
type Agent struct {
	name    string
	command string
	args    []string
}

// New constructs a Pi agent descriptor. name is the registry key
// (typically "pi"); command is the CLI binary name on PATH
// (typically "pi"); args are extra flags appended after DefaultArgs.
func New(name, command string, args []string) *Agent {
	return &Agent{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
	}
}

// Name returns the registry key.
func (a *Agent) Name() string { return a.name }

// Mode reports the structured JSON-IO mode. The runtime does not
// branch on Mode; the label is for `nightme agents` and logs.
func (a *Agent) Mode() agent.Mode { return agent.ModeJSONIO }

// Command returns the configured CLI binary (typically "pi").
func (a *Agent) Command() string { return a.command }

// Args returns a defensive copy of the constructor args. Callers may
// not mutate the returned slice.
func (a *Agent) Args() []string {
	return append([]string(nil), a.args...)
}

// Detect verifies the `pi` binary resolves on PATH. Call before
// Start to surface a friendly "pi not installed" error rather than a
// confusing exec failure.
func (a *Agent) Detect() error {
	_, err := exec.LookPath(a.command)
	return err
}

// Start spawns Pi in RPC mode and returns an AgentSession that
// streams events on its Events channel.
//
// Workspace is the child process's cwd. cfg.Args are appended after
// the agent's defaults (DefaultArgs + a.args). cfg.Env is appended
// to os.Environ() for the child.
//
// cfg.PermissionMode is ignored: Pi has no equivalent CLI flag and
// the F-32 MVP does not surface Pi's extension UI to the channel.
// Kept in the signature for forward compatibility with future
// /use flags that may translate to Pi commands.
//
// On Start success, the returned session has an active process and
// has already completed the get_state handshake. The caller must
// Close() it when done.
func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.AgentSession, error) {
	args := buildArgs(a.args, cfg.Args)

	env := append([]string(nil), cfg.Env...)
	env = append(env, a.command) // ensure command name is in env (defensive)

	return newSession(ctx, a.name, a.command, args, env, cfg.Workspace)
}

// buildArgs concatenates DefaultArgs + extraArgs + cfg.Args. Extracted
// as a package-private helper so tests can assert on the produced
// argv without spawning a process. Mirrors the contract of
// Agent.Start exactly.
func buildArgs(extraArgs, cfgArgs []string) []string {
	out := make([]string, 0, len(DefaultArgs)+len(extraArgs)+len(cfgArgs))
	out = append(out, DefaultArgs...)
	out = append(out, extraArgs...)
	out = append(out, cfgArgs...)
	return out
}

// Compile-time guarantee that *Agent satisfies agent.Agent.
var _ agent.Agent = (*Agent)(nil)
