// Long-lived `pi --mode rpc` session.
//
// Session lifetime is two-tier:
//
//   - process  : spawn() -> cmd.Wait() -> close(events)
//   - turn     : prompt() ack -> stream events -> agent_settled -> EventDone
//
// The process can carry many turns. EventDone marks the end of one
// turn but does NOT close the events channel; only process exit
// (or Close) does that. This mirrors the contract documented in
// F-32 §3.3 and lets ChatSession.runReadPump continue reading
// across many turns on the same AgentSession.

package pi

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// handshakeTimeout bounds the initial get_state round-trip. Pi
// is expected to answer within a couple of seconds even on cold
// start; 10 s matches the acp bridge's startup deadline.
const handshakeTimeout = 10 * time.Second

// shutdownGrace is the SIGINT-to-SIGKILL window for Close(), kept
// short so /kill on a stuck prompt does not hang the runtime.
const shutdownGrace = 2 * time.Second

// maxImageBytes caps a single ContentImage after base64 encoding.
// 10 MiB of decoded binary is well within Pi's stated image support
// and keeps a single SendBlocks well under MaxFrameSize even after
// other content. Larger images are rejected with ErrImageTooLarge
// instead of silently truncating.
const maxImageBytes = 10 * 1024 * 1024

// ErrImageTooLarge is returned by SendBlocks when a single
// ContentImage exceeds maxImageBytes after base64 encoding. The
// caller should drop the offending block (or surface a clearer
// user-facing error); the bridge does not retry.
var ErrImageTooLarge = errors.New("pi: image too large")

// ErrTurnClosed is returned by SendBlocks when the previous prompt
// was rejected by Pi (success:false on the response envelope).
// The session is still usable; the caller can retry.
var ErrTurnClosed = errors.New("pi: previous prompt rejected")

// session is the runtime handle for one Pi invocation. It owns the
// child process, the JSONL pump goroutine, the per-turn ack waiter,
// and the Auto-cancelled extension-UI queue.
//
// Implements agent.AgentSession. Safe for concurrent calls to
// SendText (writes are serialized via rpcClient.writeMu). The turn
// lock (turnMu) prevents two prompts from being in flight at the
// same time; the second SendBlocks returns ErrTurnBusy.
type session struct {
	cmd       *exec.Cmd
	stdinW    io.WriteCloser
	stdoutR   io.ReadCloser
	stderrR   io.ReadCloser
	rpc       *rpcClient
	events    chan agent.AgentEvent
	pid       int
	agentName string
	workspace string
	branch    string

	// turnMu serializes SendBlocks calls so a second prompt cannot
	// start before the first one is acknowledged by Pi. It also
	// guards the "turnActive" flag.
	turnMu     sync.Mutex
	turnActive bool

	translator *translator

	// translatorMu serializes initSent reset + emitInit calls between
	// the boot handshake (newSession) and F-34 reset (s.New). Both
	// call paths can race if the user fires /new before the boot
	// handshake completes; we hold this mutex across both so the
	// initSent flag stays consistent.
	translatorMu sync.Mutex
	logger       *slog.Logger

	closeOnce sync.Once
	closed    chan struct{}

	// pumpWG tracks readPump + drainStderr so the lifecycle
	// goroutine can wait for them to drain before closing the
	// events channel. Without this, a final-line deliver() in
	// readPump races with lifecycle's close(s.events) and the
	// race detector flags runtime.closechan vs runtime.chansend.
	pumpWG sync.WaitGroup

	// exitDone is closed by the lifecycle goroutine when the
	// child has exited AND the events channel has been closed.
	// Close() waits on this so callers know the bridge is fully
	// reaped before they proceed. It replaces the in-line
	// cmd.Wait() that used to live in Close -- exec.Cmd.Wait is
	// not safe to call from multiple goroutines (the standard
	// library has a documented race), so the lifecycle goroutine
	// is the single owner of the Wait call.
	exitDone chan struct{}
}

// newSession spawns `pi` with args + env, then drives the
// get_state handshake. The returned AgentSession is ready for
// SendText / Events immediately on success.
//
// agentName + workspace + branch are stamped onto every EventInit
// emitted by the translator so the channel-layer receipt can render
// the "Agent | repo | branch | tokens" foot note.
//
// branch is captured by running `git -C workspace symbolic-ref
// --short HEAD` (or rev-parse). Failure is non-fatal: the branch
// is left empty and the receipt omits that segment. We run this
// BEFORE spawning the child so a slow `git` does not delay receipt
// init.
func newSession(ctx context.Context, agentName, command string, args, env []string, workspace string) (agent.AgentSession, error) {
	if workspace == "" {
		return nil, fmt.Errorf("pi: workspace is required")
	}

	branch := detectBranch(workspace)
	logger := slog.Default()

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pi: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("pi: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("pi: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("pi: start: %w", err)
	}

	s := &session{
		cmd:       cmd,
		stdinW:    stdin,
		stdoutR:   stdout,
		stderrR:   stderr,
		rpc:       newRPCClient(stdin),
		events:    make(chan agent.AgentEvent, 64),
		pid:       cmd.Process.Pid,
		agentName: agentName,
		workspace: workspace,
		branch:    branch,
		translator: newTranslator(agentName, workspace, branch),
		logger:    logger,
		closed:    make(chan struct{}),
		exitDone:  make(chan struct{}),
	}

	// Read pump and stderr drainer start in parallel with the
	// handshake so a slow get_state does not stall read-back
	// pressure. The lifecycle goroutine (cmd.Wait) is started
	// last so it owns both the events close and the pending
	// fail. pumpWG is incremented BEFORE the goroutines start
	// and decremented inside them so the lifecycle Wait below
	// cannot race a missed Add.
	s.pumpWG.Add(2)
	go func() {
		defer s.pumpWG.Done()
		s.readPump()
	}()
	go func() {
		defer s.pumpWG.Done()
		s.drainStderr()
	}()
	go s.lifecycle()

	// Drive the get_state handshake synchronously so a Start
	// failure surfaces immediately. The handshake uses the
	// dedicated id "boot" so it cannot collide with later
	// per-turn requests (which use req-NNN ids).
	hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	resp, err := s.rpc.request(hsCtx, "get_state", map[string]any{}, "boot")
	if err != nil {
		_ = s.Close()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("pi: handshake timeout after %s (binary present? --mode rpc supported?): %w", handshakeTimeout, context.DeadlineExceeded)
		}
		return nil, fmt.Errorf("pi: get_state: %w", err)
	}
	if !resp.Success {
		_ = s.Close()
		return nil, fmt.Errorf("pi: get_state rejected: %s", resp.Error)
	}

	var state getStateResult
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &state); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("pi: decode get_state: %w", err)
		}
	}
	// F-34 review C1: hold translatorMu across emitInit + deliver so
	// a concurrent s.New cannot observe initSent=false between our
	// reset and emit. Boot path runs before the bridge returns to
	// callers, so contention is theoretical, but the symmetry with
	// the reset path keeps the invariant obvious.
	s.translatorMu.Lock()
	s.deliverInitLocked(&state)
	s.translatorMu.Unlock()
	return s, nil
}

// deliverInitLocked emits the EventInit for `state` via deliver().
// Caller MUST hold s.translatorMu so initSent's check-and-set is
// atomic relative to translator.emitInit. Shared between the boot
// handshake (newSession) and the F-34 reset path (New).
func (s *session) deliverInitLocked(state *getStateResult) {
	for _, ev := range s.translator.emitInit(state) {
		s.deliver(ev)
	}
}

// deliver pushes one AgentEvent onto the events channel. The
// s.closed channel is the cancel signal: lifecycle never closes
// s.events while the pumps (readPump, drainStderr) are still
// running -- the pumpWG wait inside lifecycle is the barrier
// that makes this safe.
//
// Two cancellation paths break the soft-stall that would
// otherwise occur if the events buffer is full and the
// consumer (chat session read pump) is slow or has stopped:
//
//  1. s.closed is the user-initiated Close() signal. The
//     select drops the event instead of waiting for a
//     reader that is no longer coming.
//  2. The 1 s timeout covers the rare case where the
//     consumer is just slow but not gone. After 1 s the
//     event is dropped, the read pump proceeds to consume
//     the next scanner line, and the next deliver() will
//     either succeed or drop too. We deliberately do NOT
//     retry buffered events: out-of-order delivery is
//     worse than dropping.
//
// The 1 s bound is well below shutdownGrace (2 s) so the
// Close watchdog can still escalate to SIGKILL before the
// bridge stalls.
func (s *session) deliver(ev agent.AgentEvent) {
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case s.events <- ev:
	case <-s.closed:
	case <-t.C:
	}
}

// Events returns the live event stream. The channel is closed by
// the lifecycle goroutine after process exit or Close().
func (s *session) Events() <-chan agent.AgentEvent { return s.events }

// PID returns the child process pid.
func (s *session) PID() int { return s.pid }

// SendText delivers a single text user turn. It is a thin wrapper
// around SendBlocks. The interface does not accept a context
// here; callers that need a deadline should call SendBlocks
// directly with a cancellable context.
func (s *session) SendText(text string) error {
	if text == "" {
		return nil
	}
	return s.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: text},
	})
}

// SendBlocks delivers a structured user turn. The bridge joins
// multiple ContentText blocks with "\n", base64-encodes
// ContentImage blocks into prompt.images[], and degrades
// ContentFile blocks to a "[file: <path>" suffix on the message.
//
// Empty blocks slice is a no-op. Concurrent SendBlocks calls are
// rejected with ErrTurnBusy while a previous prompt is still
// awaiting its ack response.
func (s *session) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	if len(blocks) == 0 {
		return nil
	}

	s.turnMu.Lock()
	if s.turnActive {
		s.turnMu.Unlock()
		return ErrTurnBusy
	}
	s.turnActive = true
	s.turnMu.Unlock()
	defer func() {
		s.turnMu.Lock()
		s.turnActive = false
		s.turnMu.Unlock()
	}()

	var messageText string
	var images []imageAttachment
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			if b.Text == "" {
				continue
			}
			if messageText != "" {
				messageText += "\n"
			}
			messageText += b.Text
		case agent.ContentImage:
			dataURL, err := encodeImage(b.Path, b.MediaType)
			if err != nil {
				return err
			}
			images = append(images, imageAttachment{
				Data:     stripDataURLPrefix(dataURL),
				MimeType: b.MediaType,
			})
		case agent.ContentFile:
			// F-32 MVP: degrade to a text reference so the
			// model can read it via the workspace's file
			// tools. We deliberately do not return an error
			// here -- the same fallback exists in claudecode.
			if b.Path == "" {
				continue
			}
			if messageText != "" {
				messageText += "\n"
			}
			messageText += "[file: " + b.Path + "]"
		default:
			return fmt.Errorf("pi: unknown content block type %q", b.Type)
		}
	}

	if messageText == "" && len(images) == 0 {
		return nil
	}

	params := promptParams{Message: messageText, Images: images}
	resp, err := s.rpc.request(ctx, "prompt", params, "")
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%w: %s", ErrTurnClosed, resp.Error)
	}
	return nil
}

// SendPermission is not used in the F-32 MVP. Extension UI is
// auto-cancelled by the read pump. The interface still requires
// the method, so we return a clear error when it is called.
func (s *session) SendPermission(_ string) error {
	return errors.New("pi: SendPermission not supported in F-32 MVP (extension UI auto-cancelled)")
}

// New resets the conversation context on the running session without
// terminating the underlying process. F-34 §3.2.2 + Phase 3 protocol
// verification 2026-08-04:
//
// Per the official pi-coding-agent RPC spec (docs/rpc.md):
//   - `new_session` RPC exists, request envelope: {"type":"new_session"}
//   - Response carries only {"cancelled":bool}; NO new sessionId in data
//   - NO `state_update` event is emitted afterwards
//   - The ONLY way to learn the new sessionId is to call `get_state`
//     again after `new_session` completes
//
// Implementation: send new_session, wait for response, then issue
// get_state to retrieve the new sessionId, then push it into the
// events channel as an EventInit so the runtime's eventHandler
// captures it via SetResumeID (cmd/nightme/run.go newEventHandler).
//
// The process stays alive; the transport stays open; Events() stays
// open; PID stays the same. Subsequent prompt submissions operate on
// the fresh conversation.
//
// Timeout: each of the two RPC round-trips (new_session + get_state)
// gets its own 10s deadline (F-34 Phase 3 review). The slash
// command handler passes a Background ctx that would otherwise let
// a hung pi server block /new forever; we previously shared a
// single deadline across both calls, which left get_state starved
// if new_session was slow.
func (s *session) New(ctx context.Context) error {
	select {
	case <-s.closed:
		return errors.New("pi: session closed")
	default:
	}

	// 1. Send new_session and wait for response.
	newCtx, newCancel := context.WithTimeout(ctx, 10*time.Second)
	defer newCancel()
	respEnv, err := s.rpc.request(newCtx, "new_session", nil, "")
	if err != nil {
		return fmt.Errorf("pi: new_session: %w", err)
	}
	if !respEnv.Success {
		return fmt.Errorf("pi: new_session rejected: %s", respEnv.Error)
	}

	// 2. Inspect data.cancelled (extension may veto the reset).
	var cancelled struct {
		Cancelled bool `json:"cancelled"`
	}
	if len(respEnv.Data) > 0 {
		if err := json.Unmarshal(respEnv.Data, &cancelled); err != nil {
			// Be lenient: a missing/odd payload should not fail the
			// whole reset; the get_state call below is the source of
			// truth for the new sessionId.
			s.logger.Warn("pi: decode new_session data (continuing)",
				slog.String("err", err.Error()))
		}
	}
	if cancelled.Cancelled {
		return errors.New("pi: new_session cancelled by extension")
	}

	// 3. Re-arm emitInit for the new session and fetch the new id
	// via get_state. F-34 review C1: translatorMu must cover BOTH
	// the initSent reset AND the subsequent emitInit call; otherwise
	// the runtime sees a zero-sessionId init (or a stale init from
	// the boot handshake) leak out between the reset and the
	// emit. We hold the lock across reset+emit+deliver.
	s.translatorMu.Lock()
	s.translator.initSent = false
	stateCtx, stateCancel := context.WithTimeout(ctx, 10*time.Second)
	defer stateCancel()
	stateEnv, err := s.rpc.request(stateCtx, "get_state", map[string]any{}, "")
	if err != nil {
		s.translatorMu.Unlock()
		return fmt.Errorf("pi: get_state after new_session: %w", err)
	}
	if !stateEnv.Success {
		s.translatorMu.Unlock()
		return fmt.Errorf("pi: get_state rejected: %s", stateEnv.Error)
	}

	var state getStateResult
	if len(stateEnv.Data) > 0 {
		if err := json.Unmarshal(stateEnv.Data, &state); err != nil {
			s.translatorMu.Unlock()
			return fmt.Errorf("pi: decode get_state: %w", err)
		}
	}

	// F-34 review C2: refuse to commit the reset if pi has no
	// sessionId in its state. emitInit would emit a zero-SessionID
	// EventInit which runtime's eventHandler ignores (cmd/nightme/run.go
	// guards on SessionID != ""), leaving the OLD ResumeID persisted
	// in agent_sessions.json. Surface as a hard error so the caller
	// knows the reset did not take effect.
	if state.SessionID == "" {
		s.translatorMu.Unlock()
		return errors.New("pi: get_state returned empty sessionId after new_session")
	}

	// 4. Push the new EventInit into the events channel. deliver()
	// is non-blocking; if the channel is full we drop with a warn
	// (the next prompt from the user will fail visibly, but the
	// process / transport is fine).
	//
	// Note (F-34 review C3): we hold translatorMu across deliver().
	// deliver() may block up to 1s on a full channel, so a busy
	// readPump holding the chan could delay /new by up to 1s. The
	// alternative is to release the lock and accept a brief window
	// where another caller sees initSent=false; we err on safety.
	s.deliverInitLocked(&state)
	s.translatorMu.Unlock()
	return nil
}

// Close terminates the session: signals the child, waits for the
// lifecycle goroutine to reap it, then returns. Idempotent.
//
// The lifecycle goroutine is the SOLE owner of cmd.Wait -- this
// matches the contract documented in claudecode/session.go (where
// an earlier watchdog + Close double-Wait race was fixed by
// removing the watchdog) and avoids the os/exec.Cmd data race
// described in the standard library docs.
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)

		// Stop accepting new commands immediately. Any blocked
		// request waiters wake with ErrSessionClosed.
		s.rpc.failPending(ErrSessionClosed)

		// Best-effort: close the write pipe so the child sees
		// EOF on stdin. We do not return the error -- the
		// lifecycle goroutine reports the real exit status.
		_ = s.stdinW.Close()

		if s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(os.Interrupt)
		}

		// Spawn a watchdog that escalates to SIGKILL after the
		// grace period. The lifecycle goroutine owns Wait; this
		// watchdog only sends signals. There is exactly one
		// Wait caller regardless of the escalation path.
		go func() {
			select {
			case <-s.exitDone:
				// Lifecycle goroutine has already
				// reaped the child. No escalation
				// needed.
				return
			case <-time.After(shutdownGrace):
				if s.cmd.Process != nil {
					_ = s.cmd.Process.Kill()
				}
			}
		}()
	})
	// Wait for the lifecycle goroutine to finish, which means
	// the child has been reaped AND the events channel has been
	// closed. Callers (especially the test harness) can rely on
	// "<-sess.Events() == (zero, false)" returning immediately
	// after Close() returns.
	<-s.exitDone
	return nil
}

// readPump reads JSONL frames from stdout, dispatches responses to
// pending waiters, and feeds events through the translator. It
// owns the read side of the stdout pipe; lifecycle owns everything
// else.
func (s *session) readPump() {
	scanner := bufio.NewScanner(s.stdoutR)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxFrameSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Probe for "type":"response" without committing to the
		// heavier struct; events also have a "type" but the
		// discriminator for a response is exactly that literal.
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			s.deliver(agent.AgentEvent{
				Kind:  agent.EventError,
				Error: &agent.ErrorEvent{Err: fmt.Errorf("%w: %s", ErrMalformedJSON, string(line))},
			})
			s.lifecycleHalt()
			return
		}
		if probe.Type == "response" {
			var resp responseEnvelope
			if err := json.Unmarshal(line, &resp); err != nil {
				s.deliver(agent.AgentEvent{
					Kind:  agent.EventError,
					Error: &agent.ErrorEvent{Err: fmt.Errorf("%w: %s", ErrMalformedJSON, string(line))},
				})
				s.lifecycleHalt()
				return
			}
			s.rpc.dispatchResponse(resp)
			continue
		}
		// Per the wire spec, extension_ui_request carries a
		// "type":"extension_ui_request" envelope; we special-case
		// it to (a) auto-reply cancelled so Pi does not block
		// and (b) log a warning. No AgentEvent is emitted.
		if probe.Type == "extension_ui_request" {
			s.handleExtensionUIRequest(line)
			continue
		}
		// Otherwise it is a regular async event.
		events, err := s.translator.translate(line, s.logger)
		if err != nil {
			s.deliver(agent.AgentEvent{
				Kind:  agent.EventError,
				Error: &agent.ErrorEvent{Err: fmt.Errorf("%w: %s", ErrMalformedJSON, string(line))},
			})
			s.lifecycleHalt()
			return
		}
		for _, ev := range events {
			s.deliver(ev)
		}
	}

	if err := scanner.Err(); err != nil {
		// ErrFrameTooLarge is a sentinel from the bufio.Scanner
		// internal; surface it as a structured error rather than
		// the raw "bufio.Scanner: token too long" string.
		s.deliver(agent.AgentEvent{
			Kind:  agent.EventError,
			Error: &agent.ErrorEvent{Err: fmt.Errorf("%w: %v", ErrFrameTooLarge, err)},
		})
		s.lifecycleHalt()
	}
}

// handleExtensionUIRequest auto-cancels the request so Pi's
// extension does not block on a missing UI channel. The MVP does
// not surface extension UI to the channel; the queue is just a
// safety net so a future translator extension can drain it.
func (s *session) handleExtensionUIRequest(line []byte) {
	var req extensionUIRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.logger.Warn("pi extension_ui_request parse failed",
			slog.String("raw", string(line)),
		)
		return
	}
	s.logger.Warn("pi extension_ui_request auto-cancelled (F-32 MVP)",
		slog.String("id", req.ID),
		slog.String("method", req.Method),
	)
	body := map[string]any{
		"id":        req.ID,
		"type":      "extension_ui_response",
		"cancelled": true,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return
	}
	if err := s.rpc.writeRawLine(payload); err != nil {
		s.logger.Warn("pi extension_ui_response write failed",
			slog.String("error", err.Error()),
		)
	}
}

// drainStderr consumes Pi's stderr, logging each chunk at debug.
// We do not currently surface stderr to the channel; the log is
// the postmortem trail. The function returns when the pipe
// reports EOF (typically right after cmd.Wait runs). Draining is
// required regardless of whether logging is enabled: an
// undrained stderr pipe blocks the child on stderr writes and
// the bridge would deadlock on cmd.Wait.
func (s *session) drainStderr() {
	buf := make([]byte, 4096)
	for {
		n, err := s.stderrR.Read(buf)
		if n > 0 {
			s.logger.Debug("pi stderr",
				slog.String("chunk", string(buf[:n])),
			)
		}
		if err != nil {
			if err != io.EOF {
				s.logger.Debug("pi stderr EOF",
					slog.String("error", err.Error()),
				)
			}
			return
		}
	}
}

// lifecycleHalt is invoked from readPump on a fatal scan / parse
// error. It kills the child so the cmd.Wait goroutine unblocks
// and closes the events channel. The goroutine that closes
// events is the cmd.Wait goroutine; everything else just nudges
// the process toward a clean exit.
func (s *session) lifecycleHalt() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// lifecycle is the single owner of cmd.Wait and the events
// channel close. Once-close semantics are enforced by the
// closeOnce + exitDone pair: lifecycle is the only goroutine
// that touches either.
//
// Ordering: we MUST wait for readPump and drainStderr to
// finish (via pumpWG) before closing s.events. Otherwise the
// pumps could still call s.deliver() while lifecycle is in
// runtime.closechan, which the race detector flags as a
// concurrent send/close on the same channel.
func (s *session) lifecycle() {
	err := s.cmd.Wait()
	// Whatever the cause, stop accepting new requests and wake
	// any pending callers.
	s.rpc.failPending(ErrSessionClosed)

	// Emit a final EventError when the process exited with a
	// non-zero status, so the channel can flip the receipt to an
	// error state. We do not duplicate EventDone here: Close()
	// emits nothing on the channel, and the read pump's normal
	// EOF path naturally stops emitting events. If a fatal scan
	// error already emitted EventError, this is a second one --
	// which is fine; the receipt tolerates multiple EventErrors.
	if err != nil && !s.isGracefulClose() {
		s.deliver(agent.AgentEvent{
			Kind:  agent.EventError,
			Error: &agent.ErrorEvent{Err: fmt.Errorf("pi: process exit: %w", err)},
		})
	}
	// Wait for readPump and drainStderr to finish their final
	// deliver() / log call before we close s.events. This is
	// the synchronisation barrier that prevents the
	// runtime.closechan vs runtime.chansend race the detector
	// would otherwise flag.
	s.pumpWG.Wait()
	close(s.events)
	close(s.exitDone)
}

// isGracefulClose returns true when Close() is the reason the
// process exited (i.e. we deliberately killed it). Used to avoid
// emitting a spurious EventError on user-initiated shutdown.
func (s *session) isGracefulClose() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// detectBranch runs `git -C workspace symbolic-ref --short HEAD`
// (or rev-parse) and returns the current branch. Returns empty
// string on any failure (non-git workspace, git not installed,
// etc.). A 1s timeout keeps a hung git invocation from blocking
// session start.
func detectBranch(workspace string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "symbolic-ref", "--short", "HEAD")
	out, err := cmd.Output()
	if err == nil {
		s := string(out)
		// Trim trailing newline.
		for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
			s = s[:len(s)-1]
		}
		if s != "" && s != "HEAD" {
			return s
		}
	}
	// Detached HEAD: try rev-parse --short HEAD instead.
	cmd = exec.CommandContext(ctx, "git", "-C", workspace, "rev-parse", "--short", "HEAD")
	out, err = cmd.Output()
	if err != nil {
		return ""
	}
	s := string(out)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// encodeImage reads an image file and returns a "data:<mime>;base64,
// <payload>" URL. The bridge strips the prefix and emits only the
// base64 payload + mimeType per Pi's ImageContent shape, but we
// keep the data-URL form here so the helper is also useful to
// other potential callers.
func encodeImage(path, mimeType string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("pi: image path is empty")
	}
	if mimeType == "" {
		return "", fmt.Errorf("pi: image %q has empty media type", path)
	}
	cleanPath := filepath.Clean(path)
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return "", fmt.Errorf("pi: read image %q: %w", cleanPath, err)
	}
	if len(data) > maxImageBytes {
		return "", fmt.Errorf("%w: %d bytes (max %d)", ErrImageTooLarge, len(data), maxImageBytes)
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	return "data:" + mimeType + ";base64," + encoded, nil
}

// stripDataURLPrefix returns the bare base64 payload from a
// data: URL. Pi's ImageContent shape takes only the base64 string.
func stripDataURLPrefix(dataURL string) string {
	const sep byte = ','
	i := indexByte(dataURL, sep)
	if i < 0 {
		return dataURL
	}
	return dataURL[i+1:]
}

// indexByte is a local byte-index helper to avoid pulling in the
// strings package just for this one call site.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// Compile-time guarantee that *session satisfies agent.AgentSession.
var _ agent.AgentSession = (*session)(nil)
