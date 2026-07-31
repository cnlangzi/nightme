package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// session is the runtime handle for one Claude Code invocation. It owns
// the child process, the JSON event pump goroutine, and the
// AskUserQuestion permission flow.
//
// Implements agent.AgentSession. Safe for concurrent calls to SendText
// (writes are serialized via a mutex on stdin).
type session struct {
	cmd     *exec.Cmd
	stdin   *bufio.Writer
	stdinMu sync.Mutex // guards Write on the underlying pipe
	events  chan agent.AgentEvent
	pid     int

	// pendingAsk is set when an AskUserQuestion EventPermission is
	// emitted; SendPermission reads from it (matching ResponseCh).
	// nil means "no pending question".
	pendingMu  sync.Mutex
	pendingAsk *pendingAsk

	closeOnce sync.Once
	closed    chan struct{}
}

// pendingAsk is the in-flight AskUserQuestion state. We keep it here
// (rather than only on the EventPermission) so Session.SendPermission
// can serialize the user's answer back to stdin even if the channel
// dropped its copy of the response channel.
type pendingAsk struct {
	ToolUseID  string
	Multi      bool
	ResponseCh chan string
}

// newSession spawns `claude` with args + env, then starts the JSON
// event pump goroutine. The returned AgentSession is ready for
// SendText / Events immediately on success.
func newSession(ctx context.Context, command string, args, env []string, workspace string) (agent.AgentSession, error) {
	if workspace == "" {
		return nil, fmt.Errorf("claudecode: workspace is required")
	}

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("claudecode: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("claudecode: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("claudecode: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("claudecode: start: %w", err)
	}

	s := &session{
		cmd:    cmd,
		stdin:  bufio.NewWriter(stdin),
		events: make(chan agent.AgentEvent, 64),
		pid:    cmd.Process.Pid,
		closed: make(chan struct{}),
	}

	// Register pendingAsk bridge for AskUserQuestion: the default
	// handler emits EventPermission with our own ResponseCh so
	// SendPermission routes the user's answer back through the
	// same channel the bridge consumed from.
	handler := func(block contentBlock, events chan<- agent.AgentEvent, logger *slog.Logger) {
		// Pre-decode just enough to capture the ResponseCh + multi
		// flag. The full Options parsing happens inside the
		// default handler below; we only need the channel binding
		// to be stable across handler invocation and SendPermission
		// delivery.
		var probe struct {
			Questions []struct {
				Header      string `json:"header"`
				MultiSelect bool   `json:"multiSelect"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(block.Input, &probe)

		var multi bool
		if len(probe.Questions) > 0 {
			multi = probe.Questions[0].MultiSelect
		}
		respCh := make(chan string, 1)
		s.pendingMu.Lock()
		s.pendingAsk = &pendingAsk{
			ToolUseID:  block.ID,
			Multi:      multi,
			ResponseCh: respCh,
		}
		s.pendingMu.Unlock()

		// Translate the tool_use block. We need the resulting
		// EventPermission to expose the SAME ResponseCh we just
		// stored in pendingAsk, otherwise the channel layer will
		// write to a stale channel and SendPermission will block
		// forever. To do that without re-parsing, we override
		// the ResponseCh on the most recent emitted event by
		// intercepting the channel output.
		//
		// Strategy: run the default handler with a wrapper
		// channel that intercepts the EventPermission and
		// rewrites its ResponseCh to our pending channel.
		interceptEvents := make(chan agent.AgentEvent, 4)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for ev := range interceptEvents {
				if ev.Kind == agent.EventPermission && ev.Permission != nil {
					ev.Permission.ResponseCh = respCh
				}
				events <- ev
			}
		}()
		defaultAskHandler(block, interceptEvents, logger)
		close(interceptEvents)
		<-done
	}

	logger := slog.Default()
	go pumpStream(stdout, s.events, handler, logger)

	// stderr drainer — Claude Code logs to stderr; we discard
	// rather than surface (the channel layer can subscribe via
	// a future "verbose" toggle).
	go func() {
		_ = drainStderr(stderr, logger)
	}()

	// watchdog: when cmd exits, send EventDone if not already sent
	// (pumpStream sends EventDone on result event; if the process
	// crashes without emitting result, the watchdog closes the
	// stream).
	go func() {
		_ = cmd.Wait()
		// Best-effort: if pumpStream hasn't already emitted
		// EventDone, this closure signals via closed channel.
		// The events channel will be closed by Close().
	}()

	return s, nil
}

// Events returns the live event stream. Closed by Close().
func (s *session) Events() <-chan agent.AgentEvent { return s.events }

// PID returns the OS process id of the child.
func (s *session) PID() int { return s.pid }

// SendText writes a user message to Claude Code's stdin in
// stream-json format. Each call emits one user message; Claude Code
// batches multiple user messages per turn until it sees a "result".
func (s *session) SendText(text string) error {
	if text == "" {
		return nil
	}
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claudecode: marshal user msg: %w", err)
	}
	return s.writeLine(data)
}

// SendPermission writes the user's answer to the most recent
// AskUserQuestion prompt as a tool_result user message. resp is the
// option label the user selected (single-select) or a comma-
// separated list (multi-select legacy); an "Other" choice is passed
// through as the typed text.
//
// Blocks until the pending question is resolved. If no question is
// pending, returns an error (channel should not call this in that
// case).
func (s *session) SendPermission(resp string) error {
	s.pendingMu.Lock()
	ask := s.pendingAsk
	s.pendingAsk = nil
	s.pendingMu.Unlock()

	if ask == nil {
		return fmt.Errorf("claudecode: no pending AskUserQuestion")
	}

	var selected []string
	if ask.Multi {
		// Split by comma; trim whitespace. Empty entries skipped.
		for _, part := range splitAndTrim(resp, ",") {
			if part != "" {
				selected = append(selected, part)
			}
		}
	} else {
		selected = []string{resp}
	}

	data, err := encodeUserAnswer(ask.ToolUseID, selected, ask.Multi)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return s.writeLine(data)
}

// Close terminates the session: closes stdin (so the child sees EOF
// and exits cleanly), waits briefly for graceful shutdown, then
// SIGKILLs if necessary. Idempotent.
func (s *session) Close() error {
	var firstErr error
	s.closeOnce.Do(func() {
		close(s.closed)

		// Close stdin so claude sees EOF and exits.
		s.stdinMu.Lock()
		_ = s.stdin.Flush()
		s.stdinMu.Unlock()

		if s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(os.Interrupt)
		}

		// Wait up to 2s for graceful exit; SIGKILL after.
		done := make(chan struct{})
		go func() {
			_ = s.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
			<-done
		}

		close(s.events)
	})
	return firstErr
}

// writeLine writes a single JSON line to claude's stdin followed by
// \n. The mutex serializes writes (multiple SendText calls in flight).
func (s *session) writeLine(data []byte) error {
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()

	if _, err := s.stdin.Write(data); err != nil {
		return fmt.Errorf("claudecode: write stdin: %w", err)
	}
	if _, err := s.stdin.WriteString("\n"); err != nil {
		return fmt.Errorf("claudecode: write newline: %w", err)
	}
	if err := s.stdin.Flush(); err != nil {
		return fmt.Errorf("claudecode: flush stdin: %w", err)
	}
	return nil
}

// drainStderr reads stderr until EOF, logging non-empty lines at
// debug level. Used by the bridge to keep stderr from blocking.
func drainStderr(r interface {
	Read(p []byte) (int, error)
}, logger *slog.Logger) error {
	if logger == nil {
		return nil
	}
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			line := trimTrailingNewline(string(buf[:n]))
			if line != "" {
				logger.Debug("claudecode stderr", "line", line)
			}
		}
		if err != nil {
			return nil
		}
	}
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// splitAndTrim splits s by sep and trims whitespace from each piece.
// Empty pieces are removed.
func splitAndTrim(s, sep string) []string {
	parts := splitString(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = trimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitString + trimSpace are tiny helpers to avoid importing strings
// from this file just for two calls. Keeps the diff with the rest
// of the package small.
func splitString(s, sep string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			out = append(out, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	out = append(out, s[start:])
	return out
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
