// Package pty also provides the Agent wrapper that turns a CLI
// command into an agent.AgentSpec backed by the PTY transport defined
// in this package. The wrapper is the safe fallback for any binary
// that does not yet speak ACP / SDK / JSON-IO — bytes flow through
// the PTY as EventAgentText and the session manager drives them.
//
// Lives in bridge/pty/ (not in a separate agent package) so the
// whole PTY story is one tree. See docs/feat/F-21-agent-modes.md §5.3.
//
// Agent is BOTH the template (registered with agent.Builtins) and
// the live handle (returned by Start). The template half is set once
// by NewAgent and is immutable thereafter; Start clones the receiver
// and populates runtime fields on the clone. The two states share
// one type so the registry, the Spawner, and AgentSession.handle
// all deal with a single agent.Agent — no separate session struct.
package pty

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// sessionBufferSize is the capacity of the AgentEvent channel. The
// reader goroutine pushes one event per Read call; the channel is
// sized large enough to absorb a sustained backlog rather than
// dropping, matching the pi / acp / claudecode bridges' producer-
// side contract (no timeout, no default-drop).
const sessionBufferSize = 40960

// Agent is the PTY-mode bridge descriptor.
//
// Two states share one type:
//
//   - Template state (after NewAgent, before Start): only the
//     spec-half fields are populated. Registered in agent.Builtins
//     and held there as a long-lived singleton per name.
//
//   - Live state (after Start, before Close): the receiver is a
//     freshly-allocated clone with runtime fields populated (transport,
//     events, closed). Calls to Events / PID / Send* / New / Close
//     are valid here. Spec-half fields are still readable.
//
// The template (in Builtins) is never mutated; Start returns a
// separate *Agent so concurrent Start calls from different chats do
// not interfere with each other.
type Agent struct {
	// ─── template fields (set by NewAgent; immutable) ───
	name    string
	command string
	args    []string
	env     []string

	// Cols and Rows set the initial PTY size. Zero values fall back
	// to 80x24, matching config.SessionConfig defaults.
	Cols int
	Rows int

	// ─── runtime fields (zero before Start; populated on the clone) ───
	transport Transport
	events    chan agent.AgentEvent
	closed    bool
}

// NewAgent constructs the template Agent. This is the entry point
// used at registration time (cmd/nightme/agents.go calls it from
// init()); the returned *Agent is held by agent.Builtins as the
// singleton for `name`.
//
// args are appended after the binary at Start time; env entries are
// KEY=VALUE strings merged into the child environment. Both are
// defensively copied.
func NewAgent(name, command string, args, env []string) *Agent {
	return &Agent{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
		env:     append([]string(nil), env...),
	}
}

// ─── spec-half methods (valid in any state) ───

// Name returns the agent identifier used in the registry and config.
func (a *Agent) Name() string { return a.name }

// Mode reports ModePTY so the SessionManager routes through the PTY
// backend.
func (a *Agent) Mode() agent.Mode { return agent.ModePTY }

// Command returns the CLI binary the agent wraps. Surfaced by
// `nightme agents` so users can see what /run would spawn.
func (a *Agent) Command() string { return a.command }

// Args returns a defensive copy of the spawn recipe's default argv.
// Callers may not mutate the returned slice.
func (a *Agent) Args() []string {
	return append([]string(nil), a.args...)
}

// Env returns a defensive copy of the spawn recipe's default env.
// Callers may not mutate the returned slice.
func (a *Agent) Env() []string {
	return append([]string(nil), a.env...)
}

// Detect verifies the underlying CLI binary is on PATH. Callers
// should invoke this before Start to produce a friendly "X not found"
// error rather than letting Start fail deep inside the PTY layer.
func (a *Agent) Detect() error {
	_, err := exec.LookPath(a.command)
	return err
}

// ─── lifecycle ───

// Start spawns the CLI under a PTY and returns a live Agent. The
// caller (typically chatsession.AgentSession via the Spawner) must
// Close() the returned *Agent when done.
//
// Start clones the receiver — the template in Builtins is untouched.
// The clone gets its template fields copied (defensively), runtime
// fields zeroed, then NewTransport is called to spawn the process and
// the read loop is kicked off.
//
// cfg.Workspace is the child's working directory; cfg.Args are
// appended after the agent's defaults (user wins); cfg.Env is merged
// with the agent's defaults in that order (cfg wins).
func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.Agent, error) {
	cols, rows := a.Cols, a.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	// arg order: agent defaults, then user overrides (user wins).
	args := append([]string(nil), a.args...)
	args = append(args, cfg.Args...)
	// env order: agent defaults, then per-session overrides (cfg wins).
	env := append([]string(nil), a.env...)
	env = append(env, cfg.Env...)

	// ctx is currently unused — Start blocks synchronously. Reserved
	// for a future cancellation hook that propagates to the child
	// process via gopty.CmdContext.
	_ = ctx

	transport, err := NewTransport(cfg.Workspace, a.command, args, env, cols, rows)
	if err != nil {
		return nil, err
	}

	live := &Agent{
		name:      a.name,
		command:   a.command,
		args:      append([]string(nil), a.args...),
		env:       append([]string(nil), a.env...),
		Cols:      cols,
		Rows:      rows,
		transport: transport,
		events:    make(chan agent.AgentEvent, sessionBufferSize),
	}
	go live.readLoop()
	return live, nil
}

// ─── live-half methods (valid only between Start and Close) ───

// Events returns the live event stream. The read loop closes the
// channel after pushing the terminal EventAgentDone / EventAgentError,
// so callers can `for ev := range a.Events()` to drain.
func (a *Agent) Events() <-chan agent.AgentEvent { return a.events }

// PID returns the child process PID recorded by the underlying
// Transport. 0 if Start has not been called.
func (a *Agent) PID() int {
	if a.transport == nil {
		return 0
	}
	return a.transport.PID()
}

// SendText writes raw user input to the PTY stdin. Newline
// normalization is the Channel adapter's job (see F-19 §4.2).
func (a *Agent) SendText(text string) error {
	if text == "" {
		return nil
	}
	if a.transport == nil {
		return fmt.Errorf("pty: send on un-started agent")
	}
	_, err := a.transport.Write([]byte(text))
	return err
}

// SendBlocks writes a structured user turn to the PTY stdin as a
// single text payload. Block encoding for PTY mode:
//
//	ContentText   -> verbatim text + "\n"
//	ContentImage  -> "@<path>\n"   (Claude Code TUI file-ref syntax)
//	ContentFile   -> "@<path>\n"
//
// Blocks are concatenated so a single turn arrives atomically
// (matching the single-write atomicity guarantee of the Claude Code
// stream-json path).
//
// Empty blocks slice is a no-op. Image/file blocks with empty Path
// are dropped (silent — no warn log since PTY mode is best-effort).
func (a *Agent) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	_ = ctx
	if a.transport == nil {
		return fmt.Errorf("pty: send on un-started agent")
	}
	if len(blocks) == 0 {
		return nil
	}
	var b strings.Builder
	for _, blk := range blocks {
		switch blk.Type {
		case agent.ContentText:
			if blk.Text == "" {
				continue
			}
			b.WriteString(blk.Text)
			b.WriteString("\n")
		case agent.ContentImage, agent.ContentFile:
			if blk.Path == "" {
				continue
			}
			fmt.Fprintf(&b, "@%s\n", blk.Path)
		default:
			continue
		}
	}
	if b.Len() == 0 {
		return nil
	}
	_, err := a.transport.Write([]byte(b.String()))
	return err
}

// SendPermission is best-effort in PTY mode: the bridge has no
// notion of a structured permission decision, so the response is
// written verbatim to stdin. The CLI is expected to be currently
// blocking on its own permission prompt ("Allow? [Y/n]") and accept
// the bytes as input.
func (a *Agent) SendPermission(resp string) error {
	if a.transport == nil {
		return fmt.Errorf("pty: send on un-started agent")
	}
	_, err := a.transport.Write([]byte(resp))
	return err
}

// New signals that the PTY bridge cannot reset conversation context
// in-place. PTY is a protocol-less byte pipe (F-34 §3.2 + product
// clarification 2026-08-04: "pty 是删掉进程, 重启进程"). The wrapper
// layer (chatsession.AgentSession.New) catches this sentinel and
// falls back to kill-and-respawn via the configured Spawner.
func (a *Agent) New(ctx context.Context) error {
	_ = ctx
	return agent.ErrRestartRequired
}

// Close terminates the session by closing the PTY. Idempotent.
func (a *Agent) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	if a.transport == nil {
		return nil
	}
	return a.transport.Close()
}

// ptyIdleTimeout is how long RunOnce waits with no new output before
// declaring the turn done. PTY has no structured result event, so the
// heuristic is "agent went quiet for this long". 3s is enough to
// cover inter-tool-call pauses in shell agents without dragging out
// genuinely-stuck sessions (the parent push wraps RunOnce in a 5-min
// deadline as a backstop).
const ptyIdleTimeout = 3 * time.Second

// RunOnce is the one-shot counterpart to Start for PTY-backed agents.
// It opens a live PTY session, writes blocks to stdin, and collects
// EventAgentText until either EventAgentDone arrives or no new bytes
// have arrived for ptyIdleTimeout. Returns the concatenated text.
//
// PTY has no structured "result" event — every byte from the child
// is emitted as EventAgentText, and the only terminal signal is
// EventAgentDone{ExitCode: -1} on transport EOF. The idle heuristic
// is the only practical way to detect "the agent finished writing".
func (a *Agent) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (string, error) {
	live, err := a.Start(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("agent %s: spawn: %w", a.name, err)
	}
	defer live.Close()

	if err := live.SendBlocks(ctx, blocks); err != nil {
		return "", fmt.Errorf("agent %s: send: %w", a.name, err)
	}

	// The idle timer is "first-byte" — it starts ONLY after the
	// first EventAgentText arrives. Without this guard, a slow
	// PTY-wrapped CLI whose first byte takes >ptyIdleTimeout to
	// appear (e.g. shell wrapper initialization) would be declared
	// done prematurely with an empty reply.
	var sb strings.Builder
	var idle *time.Timer
	resetIdle := func() {
		if idle != nil {
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
		}
		idle = time.NewTimer(ptyIdleTimeout)
	}
	defer func() {
		if idle != nil {
			idle.Stop()
		}
	}()

	for {
		select {
		case ev, ok := <-live.Events():
			if !ok {
				return strings.TrimSpace(sb.String()), nil
			}
			switch ev.Kind {
			case agent.EventAgentText:
				sb.WriteString(ev.Text)
				resetIdle() // first byte arms the timer; subsequent bytes reset it
			case agent.EventAgentError:
				if ev.Err != nil {
					return "", fmt.Errorf("agent %s: %w", a.name, ev.Err)
				}
				return "", fmt.Errorf("agent %s: error event with nil payload", a.name)
			case agent.EventAgentDone:
				return strings.TrimSpace(sb.String()), nil
			}
		case <-idle.C:
			return strings.TrimSpace(sb.String()), nil
		case <-ctx.Done():
			// On ctx cancellation, drop the partial text — PTY has
			// no structured result event, so any output we collected
			// is "what the agent was in the middle of writing", not
			// a final answer. Returning it alongside the error
			// would mislead the caller (push command) into thinking
			// the agent had produced something usable.
			if errors.Is(ctx.Err(), context.Canceled) {
				return "", fmt.Errorf("agent %s: canceled: %w", a.name, ctx.Err())
			}
			return "", fmt.Errorf("agent %s: %w", a.name, ctx.Err())
		}
	}
}

// ─── internals ───

// readLoop drains the transport until EOF or a read error, then emits a
// terminal EventAgentDone and closes the events channel.
//
// Bytes are wrapped in EventAgentText with the raw payload — no
// transformation, no aggregation. Aggregation is the Channel
// adapter's job (see F-19 §3).
//
// Kick off via `go a.readLoop()` from Start (production) or directly
// from tests that construct an Agent with a fake Transport.
func (a *Agent) readLoop() {
	defer close(a.events)

	buf := make([]byte, 4096)
	for {
		n, err := a.transport.Read(buf)
		if n > 0 {
			a.events <- agent.AgentEvent{
				Kind: agent.EventAgentText,
				Text: string(buf[:n]),
			}
		}
		if err != nil {
			a.events <- agent.AgentEvent{
				Kind: agent.EventAgentDone,
				Done: &agent.AgentDoneEvent{ExitCode: -1},
			}
			return
		}
	}
}

// Compile-time guarantee that *Agent satisfies agent.Agent (the
// merged spec+live interface). The template-half of *Agent (set by
// NewAgent) satisfies agent.AgentSpec implicitly because the new
// agent.Agent interface embeds AgentSpec.
var _ agent.Agent = (*Agent)(nil)
