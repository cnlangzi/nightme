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
	"fmt"
	"strings"

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
// separate *driver so concurrent Start calls from different chats do
// not interfere with each other.
type driver struct {
	// ─── runtime fields (zero before Start; populated by newDriver) ───
	transport Transport
	events    chan agent.AgentEvent
	closed    bool
}

// NewAgent constructs the template Agent. This is the entry point
// used at registration time (cmd/nightme/agents.go calls it from
// init()); the returned *driver is held by agent.Builtins as the
// singleton for `name`.
//
// args are appended after the binary at Start time; env entries are
// KEY=VALUE strings merged into the child environment. Both are
// defensively copied.
func NewStarter(name, command string, args, env []string, cols, rows int) *Starter {
	return &Starter{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
		env:     append([]string(nil), env...),
		cols:    cols,
		rows:    rows,
	}
}

// ─── lifecycle ───

// Start spawns the CLI under a PTY and returns a live Agent. The
// caller (typically chatsession.AgentSession via the Spawner) must
// Close() the returned *driver when done.
//
// Start clones the receiver — the template in Builtins is untouched.
// The clone gets its template fields copied (defensively), runtime
// fields zeroed, then NewTransport is called to spawn the process and
// the read loop is kicked off.
//
// cfg.Workspace is the child's working directory; cfg.Args are
// appended after the agent's defaults (user wins); cfg.Env is merged
// with the agent's defaults in that order (cfg wins).
func newDriver(ctx context.Context, s *Starter, cfg agent.StartConfig) (*driver, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cols, rows := s.cols, s.rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	// arg order: agent defaults, then user overrides (user wins).
	args := append([]string(nil), s.args...)
	args = append(args, cfg.Args...)
	// env order: agent defaults, then per-session overrides (cfg wins).
	env := append([]string(nil), s.env...)
	env = append(env, cfg.Env...)

	// ctx is currently unused — Start blocks synchronously. Reserved
	// for a future cancellation hook that propagates to the child
	// process via gopty.CmdContext.
	_ = ctx

	transport, err := NewTransport(cfg.Workspace, s.command, args, env, cols, rows)
	if err != nil {
		return nil, err
	}

	live := &driver{
		transport: transport,
		events:    make(chan agent.AgentEvent, sessionBufferSize),
		closed:    false,
	}
	go live.readLoop()
	return live, nil
}

// ─── live-half methods (valid only between Start and Close) ───

// Events returns the live event stream. The read loop closes the
// channel after pushing the terminal EventAgentDone / EventAgentError,
// so callers can `for ev := range a.Events()` to drain.
func (d *driver) Events() <-chan agent.AgentEvent { return d.events }

// PID returns the child process PID recorded by the underlying
// Transport. 0 if Start has not been called.
func (d *driver) PID() int {
	if d.transport == nil {
		return 0
	}
	return d.transport.PID()
}

// SendText writes raw user input to the PTY stdin. Newline
// normalization is the Channel adapter's job (see F-19 §4.2).
func (d *driver) SendText(text string) error {
	if text == "" {
		return nil
	}
	if d.transport == nil {
		return fmt.Errorf("pty: send on un-started agent")
	}
	_, err := d.transport.Write([]byte(text))
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
func (d *driver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	_ = ctx
	if d.transport == nil {
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
	_, err := d.transport.Write([]byte(b.String()))
	return err
}

// SendPermission is best-effort in PTY mode: the bridge has no
// notion of a structured permission decision, so the response is
// written verbatim to stdin. The CLI is expected to be currently
// blocking on its own permission prompt ("Allow? [Y/n]") and accept
// the bytes as input.
func (d *driver) SendPermission(resp string) error {
	if d.transport == nil {
		return fmt.Errorf("pty: send on un-started agent")
	}
	_, err := d.transport.Write([]byte(resp))
	return err
}

// New signals that the PTY bridge cannot reset conversation context
// in-place. PTY is a protocol-less byte pipe (F-34 §3.2 + product
// clarification 2026-08-04: "pty 是删掉进程, 重启进程"). The wrapper
// layer (chatsession.AgentSession.New) catches this sentinel and
// falls back to kill-and-respawn via the configured Spawner.
// Reset is the agent.driver interface name for New.
func (d *driver) Reset(ctx context.Context) error { return d.New(ctx) }

func (d *driver) New(ctx context.Context) error {
	_ = ctx
	return agent.ErrRestartRequired
}

// Close terminates the session by closing the PTY. Idempotent.
func (d *driver) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	if d.transport == nil {
		return nil
	}
	return d.transport.Close()
}

// ─── internals ───

// readLoop drains the transport until EOF or a read error, then emits a
// terminal EventAgentDone and closes the events channel.
//
// Bytes are wrapped in EventAgentText with the raw payload — no
// transformation, no aggregation. Aggregation is the Channel
// adapter's job (see F-19 §3).
//
// Kick off via `go d.readLoop()` from Start (production) or directly
// from tests that construct an Agent with a fake Transport.
func (d *driver) readLoop() {
	defer close(d.events)

	buf := make([]byte, 4096)
	for {
		n, err := d.transport.Read(buf)
		if n > 0 {
			d.events <- agent.AgentEvent{
				Kind: agent.EventAgentText,
				Text: string(buf[:n]),
			}
		}
		if err != nil {
			d.events <- agent.AgentEvent{
				Kind: agent.EventAgentDone,
				Done: &agent.AgentDoneEvent{ExitCode: -1},
			}
			return
		}
	}
}

// Compile-time guarantee that *driver satisfies agent.Agent (the
// merged spec+live interface). The template-half of *driver (set by
// NewAgent) satisfies agent.AgentSpec implicitly because the new
// agent.Agent interface embeds AgentSpec.
var _ agentDriver = (*driver)(nil)
