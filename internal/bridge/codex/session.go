
// Process / I/O lifecycle for the codex app-server bridge.
//
// session owns the spawned child process, its stdio pipes, the
// rpcClient wired over those pipes, and the pumps that turn wire
// frames into agent.AgentEvent values.
//
// Lifetime:
//
//   newSession()
//     ├─ spawn codex app-server --listen stdio://
//     ├─ wire rpcClient (merged stdio pipe)
//     ├─ start readPump + stderrLoop goroutines
//     ├─ start lifecycle goroutine (owns cmd.Wait + events close)
//     ├─ handshake: initialize → initialized → ensureThread
//     └─ return; caller emits EventAgentReady separately
//
// Failure semantics:
//   - Any spawn / pipe / Start failure returns an error WITHOUT
//     closing the events channel (Start returns nil, error).
//   - Any handshake failure calls Close() to release resources
//     before returning the error.
//   - cmd.Wait owns the events-channel close (lifecycle goroutine).
//     Close() waits up to closeDrainTimeout before forcing a kill.
package codex

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── constants & exported vars ───

// handshakeTimeout bounds the initialize round-trip. Codex is expected
// to answer within a couple of seconds even on cold start; 10s matches
// the acp and pi bridges and gives plenty of room for cold model load.
const handshakeTimeout = 10 * time.Second

// closeDrainTimeout bounds the time Close() will wait for the lifecycle
// goroutine to reap the child and close the events channel. Beyond this
// window Close returns even if the underlying cmd.Wait is wedged. The
// bridge is already unusable at that point (closeOnce has fired the
// SIGKILL), and a wedged Wait must NOT block the runtime's spawn path.
const closeDrainTimeout = 5 * time.Second

// eventBufferSize is the events channel capacity.
//
// Sized to match the producer-side contract promoted in commit
// 67b295ec ("unify producer-side buffer contract across all bridges"):
// 40960 across pi / claudecode / pty / acp. Agent.deliver uses
// `events <- ev` under a select against session.closed and
// session.exitDone — NO `default:` drop, NO `time.After` drop —
// so the producer is allowed to block until the consumer drains.
//
// Allocated in newSession; buffer_contract_test.go pins the value
// at the package level so a regression that lowers the cap or
// reintroduces a default-drop is caught in `go test`.
const eventBufferSize = 40960

// stderrTailBytes is how much of the child's stderr we keep for
// error-enrichment on EventAgentError. Match cc-connect's choice.
const stderrTailBytes = 2048

// permissionTimeout is how long a server-initiated approval request
// waits for a user decision before defaulting to decline. Package
// var (not const) so tests can compress it.
var permissionTimeout = 5 * time.Minute

// codexDebug toggles the bridge's detailed debug logging.
// Default ON; silence with NIGHTME_CODEX_DEBUG=0 (also accepts
// "false", "no", "off", case-folded).
var codexDebug = codexDebugEnabled()

func codexDebugEnabled() bool {
	v := os.Getenv("NIGHTME_CODEX_DEBUG")
	switch v {
	case "", "1", "true", "yes", "on":
		return true
	}
	return false
}

// cLog emits an info-level message tagged [codex] when debug is
// enabled. Mirrors piLog so log scrapers see a consistent component.
// Tests in this package may swap slog.Default via slog.SetDefault().
func cLog(msg string, args ...any) {
	if !codexDebug {
		return
	}
	all := make([]any, 0, len(args)+2)
	all = append(all, "component", "codex")
	all = append(all, args...)
	slog.Default().Info("[codex] "+msg, all...)
}

// ─── ringBuffer ───

// ringBuffer keeps the last N bytes of a stream for diagnostic
// inclusion on EventAgentError. Not thread-safe on its own; callers
// must serialize (we call it from stderrLoop only).
type ringBuffer struct {
	buf []byte
	max int
}

func newRingBuffer(max int) *ringBuffer {
	return &ringBuffer{buf: make([]byte, 0, max), max: max}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	return string(r.buf)
}

// ─── stdioPipe ───

// stdioPipe combines a write-only stdin and a read-only stdout into a
// single io.ReadWriteCloser that the rpcClient can drive. Writes go to
// stdin, reads come from stdout. Close is a no-op on the child pipes
// (the session owns and closes them separately via Close()).
type stdioPipe struct {
	w io.Writer
	r io.Reader
}

func newStdioPipe(w io.Writer, r io.Reader) io.ReadWriteCloser {
	return &stdioPipe{w: w, r: r}
}

func (p *stdioPipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *stdioPipe) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *stdioPipe) Close() error                { return nil }

// ─── session ───

// session is the runtime half of the bridge. The Agent (template +
// live) wraps a *session; this struct carries everything tied to the
// spawned child. Concurrent access is governed by:
//
//   - rpc:       thread-safe (writeMu / pendingMu)
//   - events:    single producer (this session), single consumer (caller)
//   - pendingApprovals / pendingMu: SendPermission vs handler goroutines
//   - stderrTail: only stderrLoop writes; emitError reads under no lock
//     (the tail is advisory and tolerates one torn read).
type session struct {
	cmd         *exec.Cmd
	stdinW      io.WriteCloser // child stdin
	stdoutR     io.ReadCloser  // child stdout
	stderrR     io.ReadCloser  // child stderr
	rpc         *rpcClient
	events      chan agent.AgentEvent
	pid         int
	agentName   string
	workspace   string
	branch      string
	model       string
	threadID    string
	stderrTail  *ringBuffer
	pendingMu   sync.Mutex
	pendingApprovals map[string]chan string
	// lastPendingID is the request id of the most-recently spawned
	// approval. SendPermission routes to that specific channel
	// rather than broadcasting across the map (which is both
	// non-deterministic due to Go's randomised map iteration and
	// semantically wrong for multi-approval scenarios).
	lastPendingID string

	closeOnce   sync.Once
	closed      chan struct{}
	pumpWG      sync.WaitGroup
	exitDone    chan struct{}
	exitErr     error

	// ctx is the session's lifetime context. Cancelled by Close() so
	// any in-flight goroutines (readPump, approval goroutines) unblock.
	ctx    context.Context
	cancel context.CancelFunc

	// hooks — wired by Agent. onNotification / onServerRequest are
	// injected into rpcClient at construction time and called from
	// the rpcClient's readPump. deliver() is called by the translator
	// to enqueue an event onto events (after stamping session context).
	deliver func(agent.AgentEvent) agent.AgentEvent

	// translator turns wire envelopes into AgentEvents. Wired by
	// Agent.Start AFTER newSession returns (so we don't expose
	// the agent-name / workspace / branch through sessionConfig).
	translator *translator
}

// sessionConfig is what newSession needs from the Agent template half.
type sessionConfig struct {
	name      string
	command   string
	workspace string
	args      []string // agent defaults (currently unused; codex ignores)
	env       []string // cfg.Env extras
	model     string   // optional; passed via -c model=
	effort    string   // optional; passed via -c model_reasoning_effort=
	sessionID string   // optional; resume if non-empty
	resume    bool     // semantic: cfg.SessionID was non-empty
}

// newSession spawns codex app-server, runs the handshake, and returns
// the live session. The caller (Agent.Start) is responsible for wiring
// session.deliver to its own deliver helper BEFORE emitting any events.
func newSession(ctx context.Context, cfg sessionConfig) (*session, error) {
	if cfg.workspace == "" {
		return nil, fmt.Errorf("codex: workspace required")
	}

	// Build argv.
	argv := []string{"app-server", "--listen", "stdio://"}
	if cfg.model != "" {
		argv = append(argv, "-c", fmt.Sprintf("model=%q", cfg.model))
	}
	if cfg.effort != "" {
		argv = append(argv, "-c", fmt.Sprintf("model_reasoning_effort=%q", cfg.effort))
	}

	cmd := agent.NewCmd(ctx, cfg.command, argv...)
	cmd.Dir = cfg.workspace
	cmd.Env = append(os.Environ(), cfg.env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("codex: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("codex: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("codex: start: %w", err)
	}

	parentCtx, cancel := context.WithCancel(ctx)
	s := &session{
		cmd:               cmd,
		stdinW:            stdin,
		stdoutR:           stdout,
		stderrR:           stderr,
		events:            make(chan agent.AgentEvent, eventBufferSize),
		pid:               cmd.Process.Pid,
		agentName:         cfg.name,
		workspace:         cfg.workspace,
		branch:            detectBranch(cfg.workspace),
		stderrTail:        newRingBuffer(stderrTailBytes),
		pendingApprovals:  make(map[string]chan string),
		lastPendingID:     "",
		closed:            make(chan struct{}),
		exitDone:          make(chan struct{}),
		ctx:               parentCtx,
		cancel:            cancel,
	}
	s.rpc = newRPCClient(
		newStdioPipe(stdin, stdout),
		s.onServerRequest,
		s.onNotification,
	)

	// Pumps: readPump + stderrLoop. Both incremented BEFORE the
	// goroutine starts so lifecycle's Wait cannot race a missed Add.
	s.pumpWG.Add(2)
	go func() {
		defer s.pumpWG.Done()
		s.rpc.readPump(parentCtx, s.emitWireError)
	}()
	go func() {
		defer s.pumpWG.Done()
		s.stderrLoop(parentCtx)
	}()
	go s.lifecycle()

	cLog("session started",
		"pid", s.pid,
		"workspace", s.workspace,
		"resume", cfg.resume,
		"model", cfg.model,
		"effort", cfg.effort,
	)

	// Handshake: initialize + initialized + ensureThread.
	hsCtx, hsCancel := context.WithTimeout(parentCtx, handshakeTimeout)
	defer hsCancel()

	if err := s.initialize(hsCtx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("codex: initialize: %w", err)
	}
	if err := s.rpc.notify("initialized", nil); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("codex: initialized notify: %w", err)
	}
	if err := s.ensureThread(hsCtx, cfg.sessionID); err != nil {
		_ = s.Close()
		return nil, err
	}

	cLog("session handshake complete",
		"pid", s.pid,
		"thread_id", s.threadID,
		"model", s.model,
	)
	return s, nil
}

// initialize sends the initialize request with our clientInfo and
// the optOutNotificationMethods list. This MUST happen before any
// other request, including "initialized".
func (s *session) initialize(ctx context.Context) error {
	params := initializeParams{
		ClientInfo: clientInfo{
			Name:    "nightme-codex-bridge",
			Title:   "Nightme Codex Bridge",
			Version: version,
		},
		Capabilities: &initializeCapabilities{
			ExperimentalAPI: true,
			OptOutNotificationMethods: []string{
				"command/exec/outputDelta",
				"item/agentMessage/delta",
				"item/plan/delta",
				"item/fileChange/outputDelta",
				"item/reasoning/summaryTextDelta",
				"item/reasoning/textDelta",
			},
		},
	}
	var resp initializeResponse
	if err := s.rpc.request(ctx, "initialize", params, &resp); err != nil {
		return err
	}
	cLog("initialize ok",
		"user_agent", resp.UserAgent,
	)
	return nil
}

// ensureThread either resumes an existing thread (if sessionID is set)
// or starts a fresh one. The returned threadId becomes the source of
// EventAgentReady.SessionID; the model becomes the source of
// EventAgentReady.Model.
func (s *session) ensureThread(ctx context.Context, resumeID string) error {
	if resumeID != "" {
		params := threadResumeParams{
			ThreadID:              resumeID,
			PersistExtendedHistory: true,
			CWD:                   s.workspace,
		}
		var resp threadStartResponse
		if err := s.rpc.request(ctx, "thread/resume", params, &resp); err != nil {
			return fmt.Errorf("codex: thread/resume: %w", err)
		}
		if resp.Thread.ID == "" {
			return fmt.Errorf("codex: thread/resume returned empty thread id")
		}
		s.threadID = resp.Thread.ID
		s.model = resp.Model
		return nil
	}
	params := threadStartParams{
		CWD:                    s.workspace,
		ExperimentalRawEvents:  false,
		PersistExtendedHistory: false,
	}
	var resp threadStartResponse
	if err := s.rpc.request(ctx, "thread/start", params, &resp); err != nil {
		return fmt.Errorf("codex: thread/start: %w", err)
	}
	if resp.Thread.ID == "" {
		return fmt.Errorf("codex: thread/start returned empty thread id")
	}
	s.threadID = resp.Thread.ID
	s.model = resp.Model
	return nil
}

// stderrLoop drains the child's stderr into stderrTail. We don't log
// each chunk (the Codex app-server writes diagnostics liberally) —
// the tail is sampled into EventAgentError when something goes wrong.
func (s *session) stderrLoop(ctx context.Context) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := s.stderrR.Read(buf)
		if n > 0 {
			_, _ = s.stderrTail.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// emitWireError is called by the rpcClient read pump when a malformed
// or oversized frame arrives. We translate it into EventAgentError and
// tear down the session — a corrupted wire cannot recover.
//
// We also fail any in-flight pending approvals so the channel /
// runtime does not wait forever for a reply that will never come,
// and we cancel the session ctx so any SendBlocks / pending
// requests (e.g. a turn/start that fired immediately before the
// wire broke) unblock promptly with ErrSessionClosed.
func (s *session) emitWireError(err error) {
	cLog("wire error", "err", err.Error())
	s.deliver(agent.AgentEvent{
		Kind: agent.EventAgentError,
		Err:  err,
	})
	// Cancel pending approvals first so the decision goroutines
	// exit cleanly (they select on s.ctx.Done as a fallback).
	s.pendingMu.Lock()
	for id, ch := range s.pendingApprovals {
		select {
		case ch <- "decline":
		default:
		}
		delete(s.pendingApprovals, id)
	}
	s.lastPendingID = ""
	s.pendingMu.Unlock()
	s.rpc.failPending(ErrSessionClosed)
	// Tear down by closing stdin; the lifecycle goroutine will reap
	// the child and close events.
	select {
	case <-s.closed:
	default:
		_ = s.stdinW.Close()
	}
}

// lifecycle is the single owner of cmd.Wait and the events-channel
// close. Pump completion is gated via pumpWG so the close happens
// AFTER readPump + stderrLoop have drained.
func (s *session) lifecycle() {
	defer close(s.exitDone)
	s.exitErr = s.cmd.Wait()
	// Stop accepting new requests and wake any pending callers.
	s.rpc.failPending(ErrSessionClosed)

	// If the wire produced a parse / oversized-frame error, that has
	// already been delivered as EventAgentError. Only emit a generic
	// exit error when the cause is NOT a graceful Close().
	if s.exitErr != nil && !s.isGracefulClose() {
		tail := s.stderrTail.String()
		msg := fmt.Sprintf("codex: process exit: %v", s.exitErr)
		if tail != "" {
			msg += "\n--- stderr tail ---\n" + tail
		}
		s.deliver(agent.AgentEvent{
			Kind: agent.EventAgentError,
			Err:  fmt.Errorf("%s", msg),
		})
	}
	s.pumpWG.Wait()
	// Closing the events channel is lifecycle's responsibility, NOT
	// Close()'s. Close() only initiates shutdown (closes stdin,
	// cancels ctx). Mixing the two under one sync.Once deadlocks
	// Close()'s <-s.exitDone wait against lifecycle's closeOnce.Do.
	close(s.events)
}

// isGracefulClose reports whether the cause of cmd.Wait returning
// was Close() (i.e. we deliberately tore down the session). Used to
// avoid emitting a duplicate EventAgentError after a clean shutdown.
func (s *session) isGracefulClose() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// Close terminates the session: closes stdin so the child sees EOF
// and exits, waits up to closeDrainTimeout for a clean reap, then
// SIGKILLs the process if it hasn't exited. Idempotent.
func (s *session) Close() error {
	var firstErr error
	s.closeOnce.Do(func() {
		close(s.closed)
		s.cancel()
		// Graceful: close stdin → app-server sees EOF → exits.
		if s.stdinW != nil {
			if err := s.stdinW.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	})
	// Wait for cmd.Wait to drain via lifecycle goroutine. Done
	// outside closeOnce so lifecycle can always reach its
	// `defer close(s.exitDone)` even when Close() holds closeOnce.
	select {
	case <-s.exitDone:
	case <-time.After(closeDrainTimeout):
		// Force-kill; we already gave up waiting.
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		<-s.exitDone
	}
	return firstErr
}

// detectBranch returns the current git branch for the given working
// directory, or "" if not a git repo / detection fails. Mirrors pi's
// helper so the runtime can stamp Branch on every event the same way.
func detectBranch(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(out))
}

// version is the bridge version reported to the codex app-server in
// the initialize clientInfo. Kept in sync with the module's semantic
// intent; bump manually when the wire contract changes materially.
const version = "0.1.0"
