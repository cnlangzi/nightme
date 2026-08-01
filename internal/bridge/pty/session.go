// Package pty also provides the PTY-mode AgentSession that wraps a
// pty.Bridge behind the agent.AgentSession contract. See
// docs/feat/F-21-agent-modes.md §5.3.
package pty

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
)

// sessionBufferSize is the capacity of the AgentEvent channel. The
// reader goroutine pushes one event per Read call, so 64 covers a
// burst of fast writes without back-pressuring the PTY.
const sessionBufferSize = 64

// ptySession adapts a pty.Bridge to the agent.AgentSession interface.
// Bytes read from the bridge become EventText; EOF or a read error
// terminates the session with EventDone.
type ptySession struct {
	bridge Bridge
	events chan agent.AgentEvent
	closed bool
}

// NewPtySession wraps an existing Bridge. The caller is responsible
// for invoking Start to spawn the read loop.
func NewPtySession(b Bridge) *ptySession {
	return &ptySession{
		bridge: b,
		events: make(chan agent.AgentEvent, sessionBufferSize),
	}
}

// Start kicks off the read loop in a background goroutine. The
// goroutine owns the events channel and will close it after pushing
// the terminal EventDone. Safe to call exactly once per session.
func (s *ptySession) Start() {
	go s.readLoop()
}

// Events returns the read-only event stream. The implementation
// closes the channel after a terminal EventDone / EventError, so
// callers can `for ev := range session.Events()` to drain.
func (s *ptySession) Events() <-chan agent.AgentEvent { return s.events }

// PID returns the child process PID recorded by the underlying
// Bridge. May be 0 if the child has not been started yet (which
// should not happen for the v0.1 PTY backend).
func (s *ptySession) PID() int {
	if s.bridge == nil {
		return 0
	}
	return s.bridge.PID()
}

// SendText writes raw user input to the PTY stdin. The bytes go in
// unmodified — newline normalization is the Channel adapter's job
// (see F-19 §4.2).
//
// SendText is a convenience wrapper around SendBlocks for the
// text-only path. Implementations that need image / file
// attachments must use SendBlocks directly.
func (s *ptySession) SendText(text string) error {
	_, err := s.bridge.Write([]byte(text))
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
func (s *ptySession) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
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
	_, err := s.bridge.Write([]byte(b.String()))
	return err
}

// SendPermission is best-effort in PTY mode: the bridge has no
// notion of a structured permission decision, so the response is
// written verbatim to stdin. The CLI is expected to be currently
// blocking on its own permission prompt ("Allow? [Y/n]") and accept
// the bytes as input.
func (s *ptySession) SendPermission(resp string) error {
	_, err := s.bridge.Write([]byte(resp))
	return err
}

// Close terminates the session by closing the PTY. Idempotent.
func (s *ptySession) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.bridge.Close()
}

// readLoop drains the bridge until EOF or a read error, then emits a
// terminal EventDone and closes the events channel.
//
// Bytes are wrapped in EventText with the raw payload — no
// transformation, no aggregation. Aggregation is the Channel
// adapter's job (see F-19 §3).
func (s *ptySession) readLoop() {
	defer close(s.events)

	buf := make([]byte, 4096)
	for {
		n, err := s.bridge.Read(buf)
		if n > 0 {
			s.events <- agent.AgentEvent{
				Kind: agent.EventText,
				Text: string(buf[:n]),
			}
		}
		if err != nil {
			s.events <- agent.AgentEvent{
				Kind: agent.EventDone,
				Done: &agent.DoneEvent{ExitCode: -1},
			}
			return
		}
	}
}

// Compile-time guarantee that *ptySession satisfies AgentSession.
var _ agent.AgentSession = (*ptySession)(nil)
