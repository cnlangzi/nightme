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
	"log/slog"
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
// cfg.SessionID, when non-empty, is forwarded as Pi's native
// `--session-id <id>` CLI flag at spawn time so the spawned
// process resumes the named session (the bridge's "opaque
// SessionID" contract translates cleanly: nightme stores pi's own
// sessionId — captured from get_state — and feeds it back here on
// the next spawn). Empty means "no --session-id; start a fresh
// session". Mirrors Claude Code's `--resume <id>` flow in
// internal/bridge/claudecode/claudecode.go: same field, each
// bridge translates to its own CLI flag.
//
// On Start success, the returned session has an active process and
// has already completed the get_state handshake. The caller must
// Close() it when done.
func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.AgentSession, error) {
	args := buildArgs(a.args, cfg)

	env := append([]string(nil), cfg.Env...)
	env = append(env, a.command) // ensure command name is in env (defensive)

	return newSession(ctx, a.name, a.command, args, env, cfg.Workspace)
}

// buildArgs concatenates DefaultArgs + extraArgs + cfg.Args. Extracted
// as a package-private helper so tests can assert on the produced
// argv without spawning a process. Mirrors the contract of
// Agent.Start exactly.
// buildArgs concatenates DefaultArgs + extraArgs + cfg.Args, then
// appends Pi's `--session-id <id>` when cfg.SessionID is non-empty.
// Extracted as a package-private helper so tests can assert on
// the produced argv without spawning a process. Mirrors the
// contract of Agent.Start exactly.
//
// Order rationale: resume flag goes LAST so user-supplied cfg.Args
// (typically model/provider overrides) remain grep-visible before
// the session identifier. Same convention as the claudecode
// bridge in buildArgs().
//
// Conflict resolution: cfg.Args may legitimately carry session-
// selection flags of its own (--session-id, --session, --no-session).
// When cfg.SessionID is non-empty the runtime-persisted identity
// must win — see filterSessionFlags for the stripping logic.
// When cfg.SessionID is empty, user-supplied session-selection
// flags pass through (legitimate "spawn a fresh session at this
// id" use case).
func buildArgs(extraArgs []string, cfg agent.StartConfig) []string {
	args := filterSessionFlags(cfg.Args, cfg.SessionID, slog.Default())
	out := make([]string, 0, len(DefaultArgs)+len(extraArgs)+len(args)+2)
	out = append(out, DefaultArgs...)
	out = append(out, extraArgs...)
	out = append(out, args...)
	if cfg.SessionID != "" {
		out = append(out, "--session-id", cfg.SessionID)
	}
	return out
}

// filterSessionFlags strips any session-selection flags the caller
// placed in args when SessionID is set. When SessionID is empty the
// args pass through unchanged so the user can intentionally spawn
// a fresh session with --session-id <their-id>.
//
// Returns a fresh slice (does not mutate input). The logger may be
// nil — when nil, suppressed-strip warnings are skipped silently.
// Stripped flags + their value-taking flag's value are dropped
// from the returned slice (e.g. --session-id abc contributes 2
// elements to the source slice, 0 to the returned slice).
func filterSessionFlags(args []string, sessionID string, logger *slog.Logger) []string {
	if sessionID == "" {
		return args
	}
	out := make([]string, 0, len(args))
	skipNext := false
	stripped := false
	for _, a := range args {
		if skipNext {
			// The value arg belonging to a session-selection flag
			// we already chose to drop.
			skipNext = false
			stripped = true
			continue
		}
		switch a {
		case "--session-id", "--session":
			skipNext = true
			stripped = true
			continue
		case "--no-session":
			stripped = true
			continue
		}
		out = append(out, a)
	}
	if stripped && logger != nil {
		logger.Debug("pi buildArgs: cfg.Args carried session-selection flags; runtime SessionID wins",
			slog.String("resume_id", sessionID))
	}
	return out
}

// Compile-time guarantee that *Agent satisfies agent.Agent.
var _ agent.Agent = (*Agent)(nil)
