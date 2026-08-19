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
//
// Agent is BOTH the template (registered with agent.Builtins) and
// the live handle (returned by Start). The template half is set
// once by New and is immutable thereafter; Start clones the
// receiver and populates runtime fields on the clone. The two
// states share one type so the registry, the Spawner, and
// AgentSession.handle all deal with a single agent.Agent — no
// separate session struct.
//
// Session lifetime is two-tier:
//
//   - process  : spawn() -> cmd.Wait() -> close(events)
//   - turn     : prompt() ack -> stream events -> agent_settled -> EventAgentDone
//
// The process can carry many turns. EventAgentDone marks the end
// of one turn but does NOT close the events channel; only process
// exit (or Close) does that. This mirrors the contract documented
// in F-32 §3.3 and lets ChatSession.runReadPump continue reading
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
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agent/procutil"
	"github.com/cnlangzi/nightme/internal/proc"
)

// ─── constants & exported errors ───

// DefaultArgs is the canonical argv used when spawning `pi` in RPC
// mode. The flag set is intentionally minimal: --mode rpc is the
// only behavioral switch. We deliberately do not pass --model or
// --thinking; selection is the user's choice via Pi's own config
// or future /use flags. We do not pass --permission-mode either;
// Pi has no equivalent of Claude Code's bypassPermissions, and the
// F-32 MVP does not support /abort or extension UI forwarding.
var DefaultArgs = []string{
	"--mode", "rpc",
}

// handshakeTimeout bounds the initial get_state round-trip. Pi
// is expected to answer within a couple of seconds even on cold
// start; 10 s matches the acp bridge's startup deadline.
const handshakeTimeout = 10 * time.Second

// promptTimeout bounds a single SendBlocks turn.
//
// Pi is expected to ack the `prompt` RPC within a few seconds
// even on slow first-token model latency; 90 s leaves generous
// room for cold-model warm-up while still surfacing a real hang
// before the channel's reaction times out. Without this deadline
// a hung prompt leaves the user staring at an empty receipt card
// (F-32 2026-08-06 incident).
//
// Declared as a var (not const) so unit tests in this package
// can shrink it to ~hundreds of ms for fast prompt-deadline
// coverage without spinning a fake pi that hangs for 90 s.
var promptTimeout = 90 * time.Second

// shutdownGrace is the SIGINT-to-SIGKILL window for Close(), kept
// short so /close on a stuck prompt does not hang the runtime.
const shutdownGrace = 2 * time.Second

// closeDrainTimeout bounds the time Close() will wait for the
// lifecycle goroutine to reap the child and close the events
// channel. Beyond this window Close returns even if the underlying
// cmd.Wait is wedged (zombie / SIGKILL reap not landing). The
// bridge is already unusable at this point (closeOnce has fired
// SIGINT + SIGKILL), and a wedged Wait must NOT block the
// runtime's spawn path indefinitely — that is exactly the failure
// mode that froze the dispatchLoop in the F-32 incident report
// (2026-08-06).
const closeDrainTimeout = 5 * time.Second

// piDebug toggles the bridge's detailed debug logging. Default
// ON (F-32 2026-08-06 follow-up) so a "why is pi hung" incident
// produces a usable breadcrumb trail in the daemon log without
// the operator remembering to flip an env var first. Silence it
// by exporting NIGHTME_PI_DEBUG=0 (or "false", "no", "off").
//
// The flag is read once at package init; toggling it mid-session
// has no effect. Tests in this package may flip `piDebug`
// directly to keep test output clean.
var piDebug = piDebugEnabled()

// piDebugEnabled reports whether the operator wants the pi
// bridge's breadcrumb trail. Default-on: empty / unset /
// unrecognized values are treated as enabled. Explicit
// "disable" tokens are "0", "false", "no", "off" (case-folded).
func piDebugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NIGHTME_PI_DEBUG"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// piLog emits an info-level message tagged [pi] (component="pi")
// when piDebug is enabled. Otherwise the call is a no-op so the
// hot path stays cheap. Used by newSession / Close / readPump /
// rpc.request to surface the exact sequence of events when the
// bridge stalls.
//
// Log level is Info (not Debug) on purpose: the default
// production slog handler runs at LevelInfo, and breadcrumbs
// that vanish into Debug-level filtering are useless when
// chasing a hang.
func piLog(msg string, args ...any) {
	if !piDebug {
		return
	}
	all := make([]any, 0, len(args)+2)
	all = append(all, slog.String("component", "pi"))
	all = append(all, args...)
	slog.Default().Info("[pi] "+msg, all...)
}

// maxImageBytes caps a single ContentImage after base64 encoding.
// 10 MiB of decoded binary is well within Pi's stated image support
// and keeps a single SendBlocks well under MaxFrameSize even after
// other content. Larger images are rejected with ErrImageTooLarge
// instead of silently truncating.
const maxImageBytes = 10 * 1024 * 1024

// eventsBufferSize is the capacity of the AgentEvent channel.
//
// Sized large enough to absorb a sustained backlog rather than
// dropping — the deliver() contract is "never time out, never drop,
// exit only on d.closed or d.exitDone". Matches the producer-side
// contract across acp / claudecode / pty (no timeout, no default-drop).
//
// The channel is allocated in Start; deliver_contract_test.go pins
// the value at the package level so a regression that lowers the cap
// is caught in `go test`.
const eventsBufferSize = 40960

// ErrImageTooLarge is returned by SendBlocks when a single
// ContentImage exceeds maxImageBytes after base64 encoding. The
// caller should drop the offending block (or surface a clearer
// user-facing error); the bridge does not retry.
var ErrImageTooLarge = errors.New("pi: image too large")

// ErrTurnClosed is returned by SendBlocks when the previous prompt
// was rejected by Pi (success:false on the response envelope).
// The session is still usable; the caller can retry.
var ErrTurnClosed = errors.New("pi: previous prompt rejected")

// ─── Agent struct (template + runtime) ───

// Agent is the Pi-mode bridge descriptor.
//
// Two states share one type:
//
//   - Template state (after New, before Start): only the
//     spec-half fields are populated. Registered in
//     agent.Builtins and held there as a long-lived singleton
//     per name.
//
//   - Live state (after Start, before Close): the receiver is a
//     freshly-allocated clone with runtime fields populated (cmd,
//     stdinW, stdoutR, stderrR, rpc, events, pid, agentName,
//     workspace, branch, turnMu, turnActive, translator, logger,
//     pumpWG, exitDone, ...). Calls to Events / PID / Send* /
//     New / Close are valid here. Spec-half fields are still
//     readable.
//
// The template (in Builtins) is never mutated; Start returns a
// separate *driver so concurrent Start calls from different chats
// do not interfere with each other.
type driver struct {
	// ─── runtime fields (zero before Start; populated on the clone) ───
	cmd     *exec.Cmd
	stdinW  io.WriteCloser
	stdoutR io.ReadCloser
	stderrR io.ReadCloser

	// stderrTail mirrors the LAST StderrTailBytes of the child's
	// stderr for diagnostic capture on non-graceful exit. Always
	// populated (no Debug gating) — see dsh's mirror field for
	// the rationale. NOT thread-safe on its own; drainStderr
	// writes, lifecycle reads at exit.
	stderrTail *agent.StderrRingBuffer
	rpc        *rpcClient
	events     chan agent.AgentEvent
	pid        int

	// agentName / workspace / branch captured at Start and
	// stamped onto EventAgentReady events for the channel-layer
	// receipt's foot note.
	agentName string
	workspace string
	branch    string

	// turnMu serializes SendBlocks calls so a second prompt
	// cannot start before the first one is acknowledged by Pi. It
	// also guards the turnActive flag.
	turnMu     sync.Mutex
	turnActive bool

	translator *translator

	// translatorMu serializes connectedSent reset + emitConnected
	// calls between the boot handshake and F-34 reset (New).
	translatorMu sync.Mutex
	logger       *slog.Logger

	closeOnce sync.Once
	closed    chan struct{}

	// pumpWG tracks readPump + drainStderr so the lifecycle
	// goroutine can wait for them to drain before closing the
	// events channel.
	pumpWG sync.WaitGroup

	// exitDone is closed by the lifecycle goroutine when the
	// child has exited AND the events channel has been closed.
	// Close() waits on this so callers know the bridge is fully
	// reaped before they proceed.
	exitDone chan struct{}
}

// ─── template constructor + spec-half methods ───

// ─── lifecycle ───

// Start spawns Pi in RPC mode and returns a live Agent that streams
// events on its Events channel.
//
// Start clones the receiver — the template in Builtins is untouched.
// The clone gets template fields copied (defensively), runtime
// fields zeroed, then exec.CommandContext is called to spawn the
// process, the read pump + stderr drainer + lifecycle goroutine are
// kicked off, and the get_state handshake runs synchronously before
// Start returns.
//
// cfg.Workspace is the child process's cwd. cfg.Args are appended
// after the agent's defaults. cfg.Env is appended to os.Environ()
// for the child.
//
// cfg.PermissionMode is ignored (Pi has no equivalent CLI flag).
//
// cfg.SessionID, when non-empty, is forwarded as `--session-id <id>`
// at spawn time so the spawned process resumes the named session.
func newDriver(ctx context.Context, s *Starter, cfg agent.StartConfig) (*driver, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("pi: workspace is required")
	}

	startTime := time.Now()
	args := buildArgs(s.args, cfg)
	env := append([]string(nil), cfg.Env...)

	piLog("Start enter", "agent", s.name, "command", s.command, "workspace", cfg.Workspace, "args", args)

	branch := detectBranch(cfg.Workspace)
	logger := slog.Default()

	// Spawn via proc.New — see internal/proc/exec_unix.go
	// for the platform-specific SysProcAttr rationale.
	child := proc.New(ctx, s.command, args...)
	child.Dir = cfg.Workspace
	child.Env = append(os.Environ(), env...)

	stdin, err := child.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pi: stdin pipe: %w", err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("pi: stdout pipe: %w", err)
	}
	stderr, err := child.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("pi: stderr pipe: %w", err)
	}

	if err := child.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		piLog("Start child.Start failed",
			"elapsed_ms", time.Since(startTime).Milliseconds(),
			"err", err.Error())
		return nil, fmt.Errorf("pi: start: %w", err)
	}

	live := &driver{
		cmd:        child,
		stdinW:     stdin,
		stdoutR:    stdout,
		stderrR:    stderr,
		stderrTail: agent.NewStderrRingBuffer(agent.StderrTailBytes),
		rpc:        newRPCClient(stdin),
		events:     make(chan agent.AgentEvent, eventsBufferSize),
		pid:        child.Process.Pid,
		agentName:  s.name,
		workspace:  cfg.Workspace,
		branch:     branch,
		translator: newTranslator(s.name, cfg.Workspace, branch),
		logger:     logger,
		closed:     make(chan struct{}),
		exitDone:   make(chan struct{}),
	}

	// Read pump and stderr drainer start in parallel with the
	// handshake so a slow get_state does not stall read-back
	// pressure. The lifecycle goroutine (child.Wait) is started
	// last so it owns both the events close and the pending
	// fail. pumpWG is incremented BEFORE the goroutines start
	// and decremented inside them so the lifecycle Wait below
	// cannot race a missed Add.
	// pumpWG counts read-pump + stderr-drain. lifecycle is
	// wrapped in SafeGo (so a panic there is recovered and the
	// daemon stays alive) but is NOT in pumpWG — the lifecycle
	// itself calls pumpWG.Wait() inside its body, so adding
	// itself to the WaitGroup would deadlock (it would wait
	// for its own Done). SafeGo gives us daemon-level safety;
	// no WaitGroup slot is needed because lifecycle is the
	// orchestrator, not a peer of the pumps. See
	// internal/agent/safego.go for the contract.
	live.pumpWG.Add(2)
	// pi's live.deliver has NO default branch — it blocks on a
	// full events buffer waiting for the consumer to drain.
	// Calling live.deliver from inside the deferred
	// PanicEventHandler would therefore risk blocking the
	// pump goroutine forever on a wedged consumer, holding
	// pumpWG.Done() and deadlocking lifecycle's pumpWG.Wait().
	// We synthesize a non-blocking panicDeliver (drop-on-full,
	// drop-on-closed) — matches the dsh/claudecode/acp
	// panicDeliver shape, and is consistent with the new
	// opencode/codex panicDeliver pattern (those bridges'
	// deliver was also back-pressuring by design).
	panicDeliver := func(ev agent.AgentEvent) {
		select {
		case live.events <- ev:
		case <-live.closed:
			// bridge closed; drop silently
		case <-live.exitDone:
			// lifecycle done; drop silently
		default:
			// Buffer full + bridge alive. Drop — a dead pump
			// can't self-heal anyway, and blocking here would
			// wedge the bridge's pumpWG.
		}
	}
	agent.SafeGo("pi:read-pump", func() {
		defer live.pumpWG.Done()
		defer agent.PanicEventHandler(
			"pi:read-pump", panicDeliver,
			"", live.agentName, live.workspace, live.branch)
		live.readPump()
	})
	agent.SafeGo("pi:stderr-drain", func() {
		defer live.pumpWG.Done()
		defer agent.PanicEventHandler(
			"pi:stderr-drain", panicDeliver,
			"", live.agentName, live.workspace, live.branch)
		live.drainStderr()
	})
	// lifecycle is the supervisor that closes events / exitDone
	// / fails pending RPCs. pi's lifecycle uses `defer close(
	// events)` and `defer close(exitDone)` (see below) so a
	// panic in lifecycle itself still tears the bridge down
	// cleanly; SafeGo is the outer safety net for any panic
	// INSIDE those defers (e.g. a nil deref on pendingMu).
	agent.SafeGo("pi:lifecycle", live.lifecycle)

	piLog("Start pumps+lifecycle spawned",
		"pid", live.pid,
		"elapsed_ms", time.Since(startTime).Milliseconds())

	// Drive the get_state handshake synchronously so a Start
	// failure surfaces immediately.
	hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	handshakeStart := time.Now()
	piLog("handshake start", "pid", live.pid, "timeout", handshakeTimeout.String())
	resp, err := live.rpc.request(hsCtx, "get_state", map[string]any{}, "boot")
	handshakeElapsed := time.Since(handshakeStart)
	if err != nil {
		piLog("handshake failed",
			"pid", live.pid,
			"elapsed_ms", handshakeElapsed.Milliseconds(),
			"err", err.Error())
		_ = live.Close()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("pi: handshake timeout after %s (binary present? --mode rpc supported?): %w", handshakeTimeout, context.DeadlineExceeded)
		}
		return nil, fmt.Errorf("pi: get_state: %w", err)
	}
	if !resp.Success {
		piLog("handshake rejected",
			"pid", live.pid,
			"elapsed_ms", handshakeElapsed.Milliseconds(),
			"err", resp.Error)
		_ = live.Close()
		return nil, fmt.Errorf("pi: get_state rejected: %s", resp.Error)
	}
	piLog("handshake ok",
		"pid", live.pid,
		"elapsed_ms", handshakeElapsed.Milliseconds(),
		"success", resp.Success)

	var state getStateResult
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &state); err != nil {
			_ = live.Close()
			return nil, fmt.Errorf("pi: decode get_state: %w", err)
		}
	}
	live.translatorMu.Lock()
	live.deliverConnectedLocked(&state)
	live.translatorMu.Unlock()
	piLog("Start return ok",
		"pid", live.pid,
		"total_ms", time.Since(startTime).Milliseconds())
	return live, nil
}

// ─── live-half methods ───

// Events returns the live event stream. The channel is closed by
// the lifecycle goroutine after process exit or Close().
func (d *driver) Events() <-chan agent.AgentEvent { return d.events }

// PID returns the child process pid.
func (d *driver) PID() int { return d.pid }

// SendBlocks delivers a structured user turn. The bridge joins
// multiple ContentText blocks with "\n", base64-encodes
// ContentImage blocks into prompt.images[], and degrades
// ContentFile blocks to a "[file: <path>" suffix on the message.
//
// Empty blocks slice is a no-op. Concurrent SendBlocks calls are
// rejected with ErrTurnBusy while a previous prompt is still
// awaiting its ack response.
func (d *driver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	if len(blocks) == 0 {
		return nil
	}

	// Defensive deadline: callers that hand us a no-deadline ctx
	// (e.g. the runtime's FlushHook passing cs.OpContext() with
	// no external timeout wired) would otherwise wait forever for
	// pi to ack a hung prompt RPC. promptTimeout bounds the wait
	// at the bridge layer; if the caller already wired a tighter
	// or looser deadline, their ctx takes precedence.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, promptTimeout)
		defer cancelTimeout()
	}

	// Derive a per-call ctx so the bridge layer owns its own
	// cancellation surface.
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()

	d.turnMu.Lock()
	if d.turnActive {
		d.turnMu.Unlock()
		return ErrTurnBusy
	}
	d.turnActive = true
	d.turnMu.Unlock()
	defer func() {
		d.turnMu.Lock()
		d.turnActive = false
		d.turnMu.Unlock()
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

	piLog("SendBlocks: callCtx derived",
		"pid", d.pid,
		"parent_has_deadline", ctxHasDeadline(ctx),
		"call_has_deadline", ctxHasDeadline(callCtx),
	)
	params := promptParams{Message: messageText, Images: images}
	resp, err := d.rpc.request(callCtx, "prompt", params, "")
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%w: %s", ErrTurnClosed, resp.Error)
	}
	return nil
}

// ctxHasDeadline reports whether ctx carries an explicit deadline.
// Used by SendBlocks to substitute promptTimeout when the caller
// passed a bare context.Background().
func ctxHasDeadline(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Deadline()
	return ok
}

// SendPermission is currently a no-op for pi: pi does not surface a
// structured AskUserQuestion tool today (F-32 MVP). The signature
// is kept so pi satisfies the merged agent.Agent interface; the
// ChatSession layer treats a no-op return as "permission granted"
// which is the safe default for pi's current tool surface.
func (d *driver) SendPermission(_ string) error {
	return nil
}

// New resets the conversation context on the running session
// without terminating the underlying process.
//
// Per F-34 §3.2.3 + Phase 3 final verification (2026-08-04):
//   - `new_session` RPC exists, request envelope: {"type":"new_session"}
//   - Response carries only {"cancelled":bool}; NO new sessionId in data
//   - The ONLY way to learn the new sessionId is to call `get_state`
//     again after `new_session` completes
//
// Implementation: send new_session, wait for response, then issue
// get_state to retrieve the new sessionId, then push it into the
// translator so the next EventAgentReady carries it.
//
// Timeout: each of the two RPC round-trips (new_session + get_state)
// is bounded by handshakeTimeout; total wall time ≤ 2x that.
// Reset is the agent.driver interface name for New. Implements
// the agent.driver Reset method (F-34).
func (d *driver) Reset(ctx context.Context) error { return d.New(ctx) }

func (d *driver) New(ctx context.Context) error {
	if d.rpc == nil {
		return fmt.Errorf("pi: session is not initialized")
	}

	newCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	resp, err := d.rpc.request(newCtx, "new_session", map[string]any{}, "reset-1")
	if err != nil {
		return fmt.Errorf("pi: new_session: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("pi: new_session rejected: %s", resp.Error)
	}

	stateCtx, stateCancel := context.WithTimeout(ctx, handshakeTimeout)
	defer stateCancel()
	stateResp, err := d.rpc.request(stateCtx, "get_state", map[string]any{}, "reset-state")
	if err != nil {
		return fmt.Errorf("pi: get_state after reset: %w", err)
	}
	if !stateResp.Success {
		return fmt.Errorf("pi: get_state after reset rejected: %s", stateResp.Error)
	}
	var state getStateResult
	if len(stateResp.Data) > 0 {
		if err := json.Unmarshal(stateResp.Data, &state); err != nil {
			return fmt.Errorf("pi: decode get_state: %w", err)
		}
	}
	// F-34: drive the same "boot handshake" path the original Start
	// uses, so the runtime sees a fresh EventAgentReady carrying the
	// new SessionID. Without the connectedSent reset, emitConnected
	// bails out on the second call and the runtime keeps the old
	// SessionID — observable as a stale context-window footer and a
	// `--resume <dead>` on daemon restart.
	//
	// Also enter the suppression window (translate.beginReset) so any
	// events that were still in the pipe from the abandoned turn
	// (e.g. an agent_settled that races the rpc.request write of
	// new_session) cannot land on the fresh turn state. The window
	// stays open until the EventAgentReady has been pushed through
	// the events channel, deferring endReset here.
	d.translatorMu.Lock()
	d.translator.connectedSent = false
	d.translator.beginReset()
	d.deliverConnectedLocked(&state)
	d.translatorMu.Unlock()
	defer d.translator.endReset()
	return nil
}

// Stop sends an `abort` RPC to the pi session. The pi --mode rpc
// protocol exposes an abort command that cancels the in-flight turn
// and forces the agent_settled event to fire. The pi process stays
// alive; the bridge observes the boundary and the next
// SendBlocks can proceed once the chat layer's TryFlush
// loop sees IsReady() flip back to true.
//
// Stop is fire-and-forget: this method returns as soon as the
// abort RPC completes (success or failure) and does NOT wait for
// the agent_settled event. The chat layer coordinates the
// turn-end → next-submit transition via its existing
// KindPromptEnded handler.
//
// Returns ErrNotSupported if the bridge is not started yet.
func (d *driver) Stop(ctx context.Context) error {
	if d.rpc == nil {
		return agent.ErrNotSupported
	}
	stopCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	resp, err := d.rpc.request(stopCtx, "abort", map[string]any{}, "abort-1")
	if err != nil {
		return fmt.Errorf("pi: stop: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("pi: stop rejected: %s", resp.Error)
	}
	return nil
}

// Close terminates the session: signals the child, waits for the
// lifecycle goroutine to drain, then returns. Idempotent. Waits up
// to closeDrainTimeout for the underlying cmd.Wait goroutine so
// that a wedged reap cannot block the runtime's spawn path
// indefinitely (F-32 2026-08-06 incident).
func (d *driver) Close() error {
	var firstErr error
	d.closeOnce.Do(func() {
		close(d.closed)
		// Broadcast SIGINT to the cli's process group so any
		// subprocesses (e.g. a tool that's holding the rpc
		// pipe) shut down with it. Escalate to SIGKILL after.
		if d.cmd != nil && d.cmd.Process != nil {
			_ = agent.SignalProcessGroup(d.cmd.Process, os.Interrupt)
		}
		// Escalate to SIGKILL after shutdownGrace.
		time.AfterFunc(shutdownGrace, func() {
			if d.cmd != nil && d.cmd.Process != nil {
				_ = d.cmd.Process.Kill()
			}
		})
		// Wait up to closeDrainTimeout for the lifecycle goroutine
		// (which owns cmd.Wait + the events-channel close + exitDone
		// close) to finish. Beyond that, return even if the reap
		// is wedged — the bridge is already unusable.
		select {
		case <-d.exitDone:
		case <-time.After(closeDrainTimeout):
			firstErr = fmt.Errorf("pi: close drain timed out after %s", closeDrainTimeout)
		}
	})
	return firstErr
}

// Keepalive is the driver.Keepalive implementation for the
// pi bridge. pi spawns the `pi` CLI subprocess per AS — when
// it dies, the next SendBlocks silently fails. Probe the OS
// PID via procutil.AlivePID and invoke onRecover so the chat
// layer can spawn a fresh pi with the saved --resume session.
// See agent.driver.Keepalive for the full contract.
func (d *driver) Keepalive(ctx context.Context, onRecover func(context.Context) error) error {
	if err := procutil.AlivePID(d.PID()); err == nil {
		return nil
	}
	piLog("bridge process dead, invoking recovery", "pid", d.PID(), "agent", d.agentName, "workspace", d.workspace)
	if onRecover == nil {
		return fmt.Errorf("pi: bridge process dead (pid=%d) and no recovery callback", d.PID())
	}
	return onRecover(ctx)
}

// ─── internals ───

// deliverConnectedLocked emits the EventAgentReady for `state` via
// deliver(). Caller MUST hold d.translatorMu.
func (d *driver) deliverConnectedLocked(state *getStateResult) {
	for _, ev := range d.translator.emitConnected(state) {
		d.deliver(ev)
	}
}

// deliver pushes one AgentEvent onto the events channel.
//
// Producer-side back-pressure strategy: deliver NEVER times out
// and NEVER drops. The events channel is sized large enough
// (40960 — see Start) to absorb any burst the upstream runtime
// (chat session) can produce during a /use switch, a long idle
// turn, or a sustained non-active-AS backlog. The consumer (chat
// session's per-AS readpump) eventually drains the buffer.
//
// Two cancellation paths:
//
//  1. d.closed is the user-initiated Close() signal — drops so
//     we don't push to a closed channel and panic.
//  2. d.exitDone is closed by the lifecycle goroutine after
//     cmd.Wait returns — same reason. If neither has fired, the
//     send blocks until the consumer catches up. The bridge's
//     readpump blocks too, but pi's stdout pipe can absorb the
//     Gigabit of buffered JSON without back-pressuring the
//     pi process itself (kernel pipe buffer is 64 KiB; the
//     40960-event channel is in our heap, not the pipe).
//
// The previous 1 s timeout + soft-drop behavior caused the
// "bridge reset: pi: new_session: context deadline exceeded"
// failure when the runtime was busy with another AS: the per-AS
// readpump blocked on a full eventQueue, the events channel
// filled to 64, the deliver dropped the post-/new
// EventAgentReady, and the bridge's readpump stalled long enough
// that the new_session RPC's 10 s deadline fired before the
// response could be read from stdout. With a 40960-deep buffer
// and no drop, the producer can always keep up with the byte
// engine regardless of consumer lag.
func (d *driver) deliver(ev agent.AgentEvent) {
	exitDone := d.exitDone
	select {
	case d.events <- ev:
	case <-d.closed:
		piLog("deliver dropped (session closed)", "kind", ev.Kind.String())
	case <-exitDone:
		// lifecycle goroutine closed exitDone after cmd.Wait
		// returned; the bridge is being torn down. Drop
		// silently — nobody will read this anyway.
	}
}

// readPump reads JSONL frames from stdout, dispatches responses
// to pending waiters, and feeds events through the translator.
// It owns the read side of the stdout pipe; lifecycle owns
// everything else.
//
// The deferred failPending releases any parked RPC waiters
// regardless of exit cause — without it, callers parked on `<-ch`
// would block until their ctx expires.
func (d *driver) readPump() {
	defer func() {
		d.rpc.failPending(io.EOF)
		piLog("readPump exit", "pid", d.pid)
	}()

	scanner := bufio.NewScanner(d.stdoutR)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxFrameSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			d.deliver(agent.AgentEvent{
				Kind: agent.EventAgentError,
				Err:  fmt.Errorf("%w: %s", ErrMalformedJSON, string(line)),
			})
			d.lifecycleHalt()
			return
		}
		if probe.Type == "response" {
			var resp responseEnvelope
			if err := json.Unmarshal(line, &resp); err != nil {
				d.deliver(agent.AgentEvent{
					Kind: agent.EventAgentError,
					Err:  fmt.Errorf("%w: %s", ErrMalformedJSON, string(line)),
				})
				d.lifecycleHalt()
				return
			}
			d.rpc.dispatchResponse(resp)
			continue
		}
		if probe.Type == "extension_ui_request" {
			d.handleExtensionUIRequest(line)
			continue
		}
		events, err := d.translator.translate(line, d.logger)
		if err != nil {
			d.deliver(agent.AgentEvent{
				Kind: agent.EventAgentError,
				Err:  fmt.Errorf("%w: %s", ErrMalformedJSON, string(line)),
			})
			d.lifecycleHalt()
			return
		}
		for _, ev := range events {
			d.deliver(ev)
		}
	}

	if err := scanner.Err(); err != nil {
		d.deliver(agent.AgentEvent{
			Kind: agent.EventAgentError,
			Err:  fmt.Errorf("%w: %v", ErrFrameTooLarge, err),
		})
		d.lifecycleHalt()
	}
}

// drainStderr consumes the child process's stderr to keep the
// pipe from blocking the child on stderr writes (a wedged stderr
// drain deadlocks the cmd.Wait that lifecycle needs). The same
// read bytes are also written to d.stderrTail (always-on, no Debug
// gating) so lifecycle() can attach the last StderrTailBytes to
// the EventAgentError.Diagnostic on non-graceful exit. Mirrors the
// dsh / codex pattern.
func (d *driver) drainStderr() {
	buf := make([]byte, 4096)
	for {
		n, err := d.stderrR.Read(buf)
		if n > 0 {
			if d.stderrTail != nil {
				_, _ = d.stderrTail.Write(buf[:n])
			}
			d.logger.Debug("pi stderr",
				slog.String("chunk", string(buf[:n])),
			)
		}
		if err != nil {
			if err != io.EOF {
				d.logger.Debug("pi stderr EOF",
					slog.String("error", err.Error()),
				)
			}
			return
		}
	}
}

// lifecycleHalt is invoked from readPump on a fatal scan / parse
// error. It kills the child so the cmd.Wait goroutine unblocks
// and closes the events channel.
func (d *driver) lifecycleHalt() {
	if d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}
}

// lifecycle is the single owner of cmd.Wait and the events
// channel close. Once-close semantics are enforced by the
// closeOnce in Close(); everything else just nudges the
// process toward a clean exit.
func (d *driver) lifecycle() {
	// Both closes are in defer so a panic anywhere in the body
	// (or in failPending/deliver) still tears the bridge down
	// cleanly. Without these defers, a panic would orphan
	// callers waiting on events / exitDone (Close() blocks on
	// exitDone; runtime's events-reader blocks on events).
	// The order — events first, exitDone last — matches the
	// pre-existing Close() contract: Close() returns once
	// exitDone is signaled, and the events channel must already
	// be drained by then.
	defer close(d.exitDone)
	defer close(d.events)

	err := d.cmd.Wait()
	// Whatever the cause, stop accepting new requests and wake
	// any pending callers.
	d.rpc.failPending(ErrSessionClosed)

	graceful := d.isGracefulClose()
	exitKind := agent.ClassifyExit(err, graceful)
	if !graceful {
		// Bridge died without our permission — emit EventAgentError
		// with structured Diagnostic. Even when err is nil
		// (clean-exit but unrequested) we still emit so the
		// user-visible event stream reflects the unexpected exit.
		tail := ""
		if d.stderrTail != nil {
			tail = d.stderrTail.String()
		}
		diag := &agent.BridgeDiagnostic{
			ExitKind:   exitKind,
			WaitErr:    err,
			StderrTail: tail,
			SessionID:  "",
			AgentName:  d.agentName,
			KilledAt:   time.Now(),
		}
		errMsg := fmt.Sprintf("pi: lifecycle exit %s: %v", exitKind, errStr(err))
		if tail != "" {
			errMsg += "\n--- stderr tail ---\n" + agent.TruncateForLog(tail, 1024)
		}
		d.deliver(agent.AgentEvent{
			Kind:       agent.EventAgentError,
			Err:        fmt.Errorf("%s", errMsg),
			Diagnostic: diag,
		})
	}
	d.pumpWG.Wait()
}

// isGracefulClose returns true when Close() is the reason the
// process exited (i.e. we deliberately killed it). Used to avoid
// emitting a duplicate EventAgentError after a clean shutdown.
func (d *driver) isGracefulClose() bool {
	select {
	case <-d.closed:
		return true
	default:
		return false
	}
}

// handleExtensionUIRequest processes an extension_ui request frame
// from Pi. Stub — Pi's F-32 MVP does not surface the extension UI
// to the channel layer. Kept for forward compatibility with future
// /follow and /approval flows.
func (d *driver) handleExtensionUIRequest(_ []byte) {}

// ─── misc helpers (package-level) ───

// buildArgs concatenates DefaultArgs + extraArgs + cfg.Args, then
// appends Pi's `--session-id <id>` when cfg.SessionID is non-empty.
//
// Order rationale: resume flag goes LAST so user-supplied
// cfg.Args (typically model/provider overrides) remain
// grep-visible before the session identifier.
//
// Conflict resolution: cfg.Args may legitimately carry
// session-selection flags of its own (--session-id, --session,
// --no-session). When cfg.SessionID is non-empty the
// runtime-persisted identity must win — see filterSessionFlags.
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
// placed in args when SessionID is set.
func filterSessionFlags(args []string, sessionID string, logger *slog.Logger) []string {
	if sessionID == "" {
		return args
	}
	out := make([]string, 0, len(args))
	skipNext := false
	stripped := false
	for _, a := range args {
		if skipNext {
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

// detectBranch returns the current git branch for workspace, or ""
// on any failure (non-git workspace, git not installed, detached
// HEAD with no short SHA, hung invocation past 1s).
func detectBranch(workspace string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c := proc.New(ctx, "git", "-C", workspace, "symbolic-ref", "--short", "HEAD")
	out, err := c.Output()
	if err == nil {
		s := string(out)
		for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
			s = s[:len(s)-1]
		}
		if s != "" && s != "HEAD" {
			return s
		}
	}
	c = proc.New(ctx, "git", "-C", workspace, "rev-parse", "--short", "HEAD")
	out, err = c.Output()
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
// payload in the prompt.images[] array.
func encodeImage(path, mimeType string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxImageBytes {
		return "", ErrImageTooLarge
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	return "data:" + mimeType + ";base64," + encoded, nil
}

// stripDataURLPrefix removes the "data:<mime>;base64," prefix from a
// data URL, returning only the base64 payload.
func stripDataURLPrefix(dataURL string) string {
	const prefix = "base64,"
	if i := indexByte(dataURL, ','); i > 0 && strings.HasSuffix(dataURL[:i], prefix) {
		return dataURL[i+1:]
	}
	return dataURL
}

// indexByte is a tiny helper to avoid importing strings for one
// call. Returns the index of c in s, or -1 if absent.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// Compile-time guarantee that *driver satisfies the package-private
// agent.driver interface (SendBlocks/SendPermission/Reset/Close).
// External callers reach driver via *agent.Agent, which forwards
// the public methods.
var _ agentDriver = (*driver)(nil)

// agentDriver is the local alias for the agent.driver interface so
// this file can compile-time check driver satisfies it without
// importing the unexported name from the agent package.
type agentDriver interface {
	SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error
	SendPermission(resp string) error
	Reset(ctx context.Context) error
	Close() error
}
