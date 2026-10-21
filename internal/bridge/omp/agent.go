// Package omp — driver (long-lived `omp --mode rpc` chat
// session).
//
// omp does not speak ACP. The portable way to drive it from a
// non-Node host is its official RPC mode: real stdio pipes
// carrying strictly LF-delimited JSON commands, responses,
// and events. omp REQUIRES stdout to carry protocol frames
// only and stderr to remain on its own pipe; merging them
// (the PTY transport used by internal/bridge/acp) would break
// omp's contract.
//
// Agent is BOTH the template (registered with agent.Builtins)
// and the live handle (returned by Start). The template half
// is set once by New and is immutable thereafter; Start
// clones the receiver and populates runtime fields on the
// clone.
//
// Session lifetime is two-tier:
//
//   - process  : spawn() -> cmd.Wait() -> close(events)
//   - turn     : prompt() ack -> stream events -> agent_end{isTerminal:true} -> EventAgentDone
//
// The process can carry many turns. EventAgentDone marks the
// end of one turn but does NOT close the events channel;
// only process exit (or Close) does.

package omp

import (
	"bufio"
	"context"
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
	"github.com/cnlangzi/nightme/internal/proc"
)

// DefaultArgs is the canonical argv used when spawning `omp`
// in RPC mode. NightMe is the only omp client, so the
// headless process is intentionally started with the
// approval-bypass flag — `--approval-mode yolo` is omp's
// transport-level permission flag analogous to Claude
// `--permission-mode bypassPermissions` and Codex
// `sandbox_mode=danger-full-access`. Without it, omp blocks
// the first tool call for interactive approval and the bridge
// sees no events. The user can still override the flag via
// cfg.Args.
//
// model / provider / API key / MCP config are deliberately
// NOT injected (per the agent-no-config-tampering principle
// in AGENTS.md §5). All credential resolution flows from the
// user's ~/.omp/ (omp's bundled config).
var DefaultArgs = []string{
	"--mode", "rpc",
	"--approval-mode", "yolo",
}

// handshakeTimeout bounds the initial get_state round-trip.
// omp is expected to answer within a few seconds even on
// cold start; 60 s aligns with pi's handshake budget and
// covers real-omp cold starts.
const handshakeTimeout = 60 * time.Second

// promptTimeout bounds a single SendBlocks turn.
var promptTimeout = 90 * time.Second

// shutdownGrace is the SIGINT-to-SIGKILL window for Close().
const shutdownGrace = 2 * time.Second

// closeDrainTimeout bounds the time Close() will wait for
// the lifecycle goroutine to reap the child and close the
// events channel.
const closeDrainTimeout = 5 * time.Second

// eventsBufferSize is the capacity of the AgentEvent channel.
const eventsBufferSize = 40960

// ErrResumeUnhealthySession is wrapped in agent.ErrResumeUnhealthy-
// compatible errors when the spawned process rejects a resume
// id. Exports as a typed value so drivers / tests can match on
// it via errors.As when the underlying text differs from the
// canonical "Session \"<id>\" not found." literal.
var ErrResumeUnhealthySession = errors.New("omp: resume session unhealthy")

// driver is the omp-mode bridge descriptor.
//
// Two states share one type:
//
//   - Template state (after NewStarter, before Start): only
//     the spec-half fields are populated. Registered in
//     agent.Builtins and held there as a long-lived
//     singleton per name.
//
//   - Live state (after Start, before Close): the receiver
//     is a freshly-allocated clone with runtime fields
//     populated.
type driver struct {
	// ─── runtime fields (zero before Start) ───
	cmd     *exec.Cmd
	stdinW  io.WriteCloser
	stdoutR io.ReadCloser
	stderrR io.ReadCloser

	stderrTail *agent.StderrRingBuffer
	rpc        *rpcClient
	events     chan agent.AgentEvent
	pid        int

	agentName string
	workspace string
	branch    string

	turnMu           sync.Mutex
	turnActive       bool
	inFlightPromptID string
	promptCancel     context.CancelFunc

	translator *translator

	translatorMu sync.Mutex
	logger       *slog.Logger

	closeOnce sync.Once
	closed    chan struct{}

	pumpWG sync.WaitGroup

	exitDone chan struct{}

	// resumeFail captures stderr text observed during the
	// boot handshake window for a session resume rejection
	// (`Session "<id>" not found.`). Set true when the
	// scanner observes the canonical error text. Close's
	// error path reads it to surface ErrResumeUnhealthy.
	resumeMu    sync.Mutex
	resumeFail  bool
	resumeFailE error
}

// Start spawns omp in RPC mode and returns a live Agent that
// streams events on its Events channel.
//
// cfg.Workspace is the child process's cwd. cfg.Args are
// appended after the agent's defaults. cfg.Env is appended to
// os.Environ() for the child. cfg.SessionID, when non-empty,
// is forwarded as `--resume <id>` so a restarted NightMe
// process can resume the existing omp session.
func newDriver(ctx context.Context, s *Starter, cfg agent.StartConfig) (*driver, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("omp: workspace is required")
	}

	startTime := time.Now()
	args := buildArgs(s.args, cfg)
	env := append([]string(nil), cfg.Env...)
	env = append(env, ompHeadlessEnv...)

	oLog("Start enter", "agent", s.name, "command", s.command, "workspace", cfg.Workspace, "args", args)

	branch := detectBranch(cfg.Workspace)
	logger := slog.Default()

	child := proc.New(ctx, s.command, args...)
	child.Dir = cfg.Workspace
	child.Env = append(os.Environ(), env...)

	stdin, err := child.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("omp: stdin pipe: %w", err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("omp: stdout pipe: %w", err)
	}
	stderr, err := child.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("omp: stderr pipe: %w", err)
	}

	if err := child.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		oLog("Start child.Start failed",
			"elapsed_ms", time.Since(startTime).Milliseconds(),
			"err", err.Error())
		return nil, fmt.Errorf("omp: start: %w", err)
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

	panicDeliver := func(ev agent.AgentEvent) {
		select {
		case live.events <- ev:
		case <-live.closed:
		case <-live.exitDone:
		default:
		}
	}
	live.pumpWG.Add(2)
	agent.SafeGo("omp:read-pump", func() {
		defer live.pumpWG.Done()
		defer agent.PanicEventHandler(
			"omp:read-pump", panicDeliver,
			"", live.agentName, live.workspace, live.branch)
		live.readPump()
	})
	agent.SafeGo("omp:stderr-drain", func() {
		defer live.pumpWG.Done()
		defer agent.PanicEventHandler(
			"omp:stderr-drain", panicDeliver,
			"", live.agentName, live.workspace, live.branch)
		live.drainStderr()
	})
	agent.SafeGo("omp:lifecycle", live.lifecycle)

	oLog("Start pumps+lifecycle spawned",
		"pid", live.pid,
		"elapsed_ms", time.Since(startTime).Milliseconds())

	// Drive the get_state handshake synchronously so a Start
	// failure surfaces immediately. omp's opening frame is
	// {"type":"ready","protocolVersion":1,...}, followed
	// immediately by a few housekeeping frames
	// (extension_ui_request, advisor_cost_changed,
	// available_commands_update). We let readPump drain
	// those, then send get_state and capture the response
	// data.
	hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	handshakeStart := time.Now()
	oLog("handshake start", "pid", live.pid, "timeout", handshakeTimeout.String())
	resp, err := live.rpc.request(hsCtx, "get_state", map[string]any{}, "boot")
	handshakeElapsed := time.Since(handshakeStart)
	if err != nil {
		oLog("handshake failed",
			"pid", live.pid,
			"elapsed_ms", handshakeElapsed.Milliseconds(),
			"err", err.Error())
		_ = live.Close()
		if isResumeFailure(live.stderrTail) {
			return nil, fmt.Errorf("omp: %w: %s", agent.ErrResumeUnhealthy, err.Error())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("omp: handshake timeout after %s (binary present? --mode rpc supported?): %w", handshakeTimeout, context.DeadlineExceeded)
		}
		return nil, fmt.Errorf("omp: get_state: %w", err)
	}
	if !resp.Success {
		oLog("handshake rejected",
			"pid", live.pid,
			"elapsed_ms", handshakeElapsed.Milliseconds(),
			"err", resp.Error)
		_ = live.Close()
		return nil, fmt.Errorf("omp: get_state rejected: %s", resp.Error)
	}
	oLog("handshake ok",
		"pid", live.pid,
		"elapsed_ms", handshakeElapsed.Milliseconds(),
		"success", resp.Success)

	var state getStateResult
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &state); err != nil {
			_ = live.Close()
			return nil, fmt.Errorf("omp: decode get_state: %w", err)
		}
	}
	live.translatorMu.Lock()
	for _, ev := range live.translator.emitConnected(&state) {
		live.deliver(ev)
	}
	live.translatorMu.Unlock()
	oLog("Start return ok",
		"pid", live.pid,
		"total_ms", time.Since(startTime).Milliseconds())
	return live, nil
}

// ompHeadlessEnv contains runtime-only switches that are not
// argv flags. Empty today; placeholder for future telemetry
// or refresh toggles so cfg.Env merging can keep the bridge's
// defaults opt-in without code churn.
var ompHeadlessEnv = []string{}

// Events returns the live event stream.
func (d *driver) Events() <-chan agent.AgentEvent { return d.events }

// PID returns the child process pid.
func (d *driver) PID() int { return d.pid }

// SendBlocks delivers a structured user turn. Empty blocks
// are a no-op. Concurrent calls are rejected with ErrTurnBusy
// while a previous prompt is awaiting its ack response.
func (d *driver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	if len(blocks) == 0 {
		return nil
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, promptTimeout)
		defer cancelTimeout()
	}

	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()

	d.turnMu.Lock()
	if d.turnActive {
		d.turnMu.Unlock()
		return ErrTurnBusy
	}
	promptID := d.rpc.nextRequestID()
	d.turnActive = true
	d.inFlightPromptID = promptID
	d.promptCancel = cancelCall
	d.turnMu.Unlock()
	defer func() {
		cancelCall()
		d.turnMu.Lock()
		d.turnActive = false
		d.inFlightPromptID = ""
		d.promptCancel = nil
		d.turnMu.Unlock()
	}()

	var messageText string
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
		case agent.ContentImage, agent.ContentFile:
			// omp's RPC `prompt` does not currently accept
			// image/file attachments — they would need to be
			// transferred via a separate channel the bridge
			// does not implement yet. Surface the block as
			// a [file: <path>] / [image: <path>] hint so the
			// user at least sees what was attached.
			if b.Path == "" {
				continue
			}
			if messageText != "" {
				messageText += "\n"
			}
			switch b.Type {
			case agent.ContentImage:
				messageText += "[image: " + b.Path + "]"
			default:
				messageText += "[file: " + b.Path + "]"
			}
		default:
			return fmt.Errorf("omp: unknown content block type %q", b.Type)
		}
	}

	if messageText == "" {
		return nil
	}

	params := promptParams{Message: messageText}
	resp, err := d.rpc.request(callCtx, "prompt", params, promptID)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("omp: prompt rejected: %s", resp.Error)
	}
	return nil
}

// SendPermission is a no-op for omp. omp has no structured
// AskUserQuestion today; the signature is kept so the driver
// satisfies the agent.driver interface.
func (d *driver) SendPermission(_ string) error { return nil }

// Reset / New resets the conversation context by sending a
// new_session RPC, then re-runs the get_state handshake so
// the runtime sees a fresh EventAgentReady with the new
// SessionID.
func (d *driver) Reset(ctx context.Context) error { return d.New(ctx) }

func (d *driver) New(ctx context.Context) error {
	if d.rpc == nil {
		return fmt.Errorf("omp: session is not initialized")
	}

	newCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	resp, err := d.rpc.request(newCtx, "new_session", map[string]any{}, "reset-1")
	if err != nil {
		return fmt.Errorf("omp: new_session: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("omp: new_session rejected: %s", resp.Error)
	}

	stateCtx, stateCancel := context.WithTimeout(ctx, handshakeTimeout)
	defer stateCancel()
	stateResp, err := d.rpc.request(stateCtx, "get_state", map[string]any{}, "reset-state")
	if err != nil {
		return fmt.Errorf("omp: get_state after reset: %w", err)
	}
	if !stateResp.Success {
		return fmt.Errorf("omp: get_state after reset rejected: %s", stateResp.Error)
	}
	var state getStateResult
	if len(stateResp.Data) > 0 {
		if err := json.Unmarshal(stateResp.Data, &state); err != nil {
			return fmt.Errorf("omp: decode get_state: %w", err)
		}
	}
	d.translatorMu.Lock()
	d.translator.connectedSent = false
	for _, ev := range d.translator.emitConnected(&state) {
		d.deliver(ev)
	}
	d.translatorMu.Unlock()
	return nil
}

// Stop sends an abort RPC to omp. omp's wire contract is to
// emit agent_end{isTerminal:true} (and possibly prompt_result
// {status:"aborted"} + session_settled) after a successful
// abort, so the runtime's existing IsReady flip via the
// terminal EventAgentDone handles the user-visible turn end.
//
// Two parallel cancellation paths:
//
//   - failResponse(id) closes the prompt's pending channel so
//     SendBlocks returns ErrSessionClosed immediately.
//   - cancelFn() cancels the prompt's callCtx directly.
//
// Without failResponse, SendBlocks would block until
// promptTimeout and the next Submit would bounce off
// ErrTurnBusy.
func (d *driver) Stop(ctx context.Context) error {
	if d.rpc == nil {
		return agent.ErrNotSupported
	}

	snap := func() (pendingID string, cancelFn context.CancelFunc) {
		d.turnMu.Lock()
		pendingID = d.inFlightPromptID
		cancelFn = d.promptCancel
		d.turnMu.Unlock()
		return
	}
	pendingID, cancelFn := snap()
	if pendingID == "" && cancelFn == nil {
		deadline := time.Now().Add(5 * time.Millisecond)
		for time.Now().Before(deadline) {
			select {
			case <-time.After(500 * time.Microsecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			pendingID, cancelFn = snap()
			if pendingID != "" || cancelFn != nil {
				break
			}
		}
	}
	if pendingID != "" {
		d.rpc.failResponse(pendingID, ErrTurnAborted)
	}
	if cancelFn != nil {
		cancelFn()
	}

	stopCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	resp, err := d.rpc.request(stopCtx, "abort", map[string]any{}, "")
	if err != nil {
		return fmt.Errorf("omp: stop: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("omp: stop rejected: %s", resp.Error)
	}
	return nil
}

// Close terminates the session.
func (d *driver) Close() error {
	var firstErr error
	d.closeOnce.Do(func() {
		close(d.closed)
		if d.cmd != nil && d.cmd.Process != nil {
			_ = agent.SignalProcessGroup(d.cmd.Process, os.Interrupt)
		}
		time.AfterFunc(shutdownGrace, func() {
			if d.cmd != nil && d.cmd.Process != nil {
				_ = d.cmd.Process.Kill()
			}
		})
		// If the boot handshake observed a resume rejection
		// (Session "<id>" not found.) — and the process
		// will then exit 0 per omp's documented contract —
		// surface ErrResumeUnhealthy so the chat layer can
		// clear the stale session id and retry without
		// resume.
		if d.resumeFailed() {
			firstErr = fmt.Errorf("omp: %w: session id no longer exists", agent.ErrResumeUnhealthy)
		}
		select {
		case <-d.exitDone:
		case <-time.After(closeDrainTimeout):
			if firstErr == nil {
				firstErr = fmt.Errorf("omp: close drain timed out after %s", closeDrainTimeout)
			}
		}
	})
	return firstErr
}

// resumeFailed reports whether the boot handshake observed a
// resume rejection text on stderr. Recorded by readPump when
// it sees the canonical "Session \"<id>\" not found." line.
func (d *driver) resumeFailed() bool {
	d.resumeMu.Lock()
	defer d.resumeMu.Unlock()
	return d.resumeFail
}

// recordResumeFailure marks the session as rejected by omp
// during the resume flow. The flag sticks because retrying
// with the same id would just hit the same error.
func (d *driver) recordResumeFailure() {
	d.resumeMu.Lock()
	defer d.resumeMu.Unlock()
	d.resumeFail = true
	d.resumeFailE = ErrResumeUnhealthySession
}

// isResumeFailure inspects stderr tail text for the canonical
// "Session \"<id>\" not found." literal that omp emits when a
// --resume targets an unknown session id. Matches per
// docs: omp exits 0 in this case, so the bridge can only
// catch it via stderr text.
func isResumeFailure(tail *agent.StderrRingBuffer) bool {
	if tail == nil {
		return false
	}
	s := tail.String()
	// omp's canonical resume rejection text:
	//   Error: Session "<id>" not found.
	return strings.Contains(s, `Session "`) && strings.Contains(s, `" not found`)
}

// deliver pushes one AgentEvent onto the events channel.
func (d *driver) deliver(ev agent.AgentEvent) {
	exitDone := d.exitDone
	select {
	case d.events <- ev:
	case <-d.closed:
	case <-exitDone:
	}
}

// readPump reads JSONL frames from stdout, dispatches
// responses to pending waiters, and feeds events through the
// translator.
func (d *driver) readPump() {
	defer func() {
		d.rpc.failPending(io.EOF)
		oLog("readPump exit", "pid", d.pid)
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

// drainStderr consumes the child process's stderr to keep
// the pipe from blocking the child.
func (d *driver) drainStderr() {
	buf := make([]byte, 4096)
	for {
		n, err := d.stderrR.Read(buf)
		if n > 0 {
			if d.stderrTail != nil {
				_, _ = d.stderrTail.Write(buf[:n])
			}
			d.logger.Debug("omp stderr",
				slog.String("chunk", string(buf[:n])),
			)
			// omp's canonical resume rejection surfaces on
			// stderr. Re-check the ring buffer so the strings-
			// contains path sees the bytes we just wrote. Cheap
			// (single mutex acquire) and bounded to one read
			// chunk in practice.
			if d.stderrTail != nil && isResumeFailure(d.stderrTail) {
				d.recordResumeFailure()
			}
		}
		if err != nil {
			if err != io.EOF {
				d.logger.Debug("omp stderr EOF",
					slog.String("error", err.Error()),
				)
			}
			return
		}
	}
}

// lifecycleHalt kills the child so cmd.Wait unblocks and the
// events channel can close.
func (d *driver) lifecycleHalt() {
	if d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}
}

// lifecycle is the single owner of cmd.Wait and the events
// channel close.
func (d *driver) lifecycle() {
	defer close(d.exitDone)
	defer close(d.events)

	err := d.cmd.Wait()
	d.rpc.failPending(ErrSessionClosed)

	graceful := d.isGracefulClose()
	exitKind := agent.ClassifyExit(err, graceful)
	if !graceful {
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
		errMsg := fmt.Sprintf("omp: lifecycle exit %s: %v", exitKind, errStr(err))
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

// isGracefulClose returns true when Close() is the reason
// the process exited.
func (d *driver) isGracefulClose() bool {
	select {
	case <-d.closed:
		return true
	default:
		return false
	}
}

// ─── misc helpers (package-level) ───

// buildArgs concatenates DefaultArgs + extraArgs + cfg.Args,
// then appends omp's `--resume <id>` when cfg.SessionID is
// non-empty.
//
// Order rationale: resume flag goes LAST so user-supplied
// cfg.Args (typically model/provider overrides) remain
// grep-visible before the session identifier.
//
// Conflict resolution: cfg.Args may legitimately carry
// session-selection flags of its own (--resume,
// --continue, --no-session). When cfg.SessionID is non-empty
// the runtime-persisted identity must win — see
// filterSessionFlags.
func buildArgs(extraArgs []string, cfg agent.StartConfig) []string {
	args := filterSessionFlags(cfg.Args, cfg.SessionID, slog.Default())
	out := make([]string, 0, len(DefaultArgs)+len(extraArgs)+len(args)+2)
	out = append(out, DefaultArgs...)
	out = append(out, extraArgs...)
	out = append(out, args...)
	if cfg.SessionID != "" {
		out = append(out, "--resume", cfg.SessionID)
	}
	return out
}

// filterSessionFlags strips any session-selection flags the
// caller placed in args when SessionID is set.
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
		case "--resume", "--continue", "-r":
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
		logger.Debug("omp buildArgs: cfg.Args carried session-selection flags; runtime SessionID wins",
			slog.String("resume_id", sessionID))
	}
	return out
}

// detectBranch returns the current git branch for workspace,
// or "" on any failure. Mirrors pi/detectBranch so StatusBar
// receives consistent values.
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

// Compile-time guarantee that *driver satisfies the
// package-private agent.driver interface.
var _ agentDriver = (*driver)(nil)

// agentDriver is the local alias for the agent.driver
// interface (used only for the compile-time check above).
type agentDriver interface {
	SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error
	SendPermission(resp string) error
	Reset(ctx context.Context) error
	Close() error
	Stop(ctx context.Context) error
}

// strings is referenced via isResumeFailure above; this no-op
// alias documents the dependency for grep.
var _ = strings.Contains
