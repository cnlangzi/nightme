// Public Agent (template + live) for the opencode HTTP bridge.
//
// Two states share one type, matching the convention used by the
// codex / pi / claudecode / acp bridges:
//
//   - Template state (after New, before Start): only the spec-half
//     fields are populated. Registered in agent.Builtins and held
//     there as a long-lived singleton per name.
//
//   - Live state (after Start, before Close): the receiver is a
//     freshly-allocated clone with runtime fields populated (server
//     process, client, events channel, deliver, lifecycle state).
//     Calls to Events / PID / Send* / New / Close are valid here.
//
// The template in Builtins is never mutated; Start returns a separate
// *Agent so concurrent Start calls from different chats do not
// interfere with each other.
//
// Session lifetime is two-tier:
//
//   - process: `opencode serve` → HTTP baseURL captured → ... → Close
//   - turn:    POST /prompt → SSE events … → session.idle → EventAgentDone{Reason:"settled"}
//
// The process carries many turns. EventAgentDone marks the end of one
// turn but does NOT close the events channel; only process exit or
// Close() does.
package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── Agent struct ────────────────────────────────────────────────

// Agent is the opencode HTTP bridge descriptor.
//
// Two states share one type (see file doc).
type Agent struct {
	// ─── template fields (set by New; immutable) ───
	name    string
	command string
	args    []string

	// ─── runtime fields (zero before Start; populated on the clone) ───
	server    *serverProc
	client    *Client
	events    chan agent.AgentEvent
	workspace string
	branch    string
	sessionID string
	model     string
	trans     *translator

	// pendingMu guards both pendingTurnActive and pendingApproval.
	// Lifted from the codex bridge's pattern, but simpler: opencode
	// has only one pending approval at a time (no broadcast routes).
	pendingMu         sync.Mutex
	pendingTurnActive bool
	pendingApprovalID string // request id of the most recent permission.asked

	// lastEventAtUnixNano is set by the SSE reader on every frame
	// received. The watchdog goroutine compares it against the
	// current wall clock to decide whether the server has gone
	// silent. atomic.Int64 — written from readSSE, read from the
	// watchdog, no shared mutex needed.
	lastEventAtUnixNano atomic.Int64

	closeOnce sync.Once
	closed    chan struct{}
	stopDeliver chan struct{} // closed before events to signal deliver to back off
	exitDone  chan struct{}

	// sseCancel cancels the SSE stream context, which closes the
	// HTTP response body and lets readSSE exit. Without this,
	// readSSE blocks on a body that never closes and lifecycle
	// deadlocks on pumpWG.Wait.
	sseCancel context.CancelFunc

	// pumpWG tracks readSSE so the lifecycle goroutine can wait
	// for the SSE reader to drain before closing events. Without
	// this, a slow SSE reader can race with close(events) and
	// panic on a send to the closed channel.
	pumpWG sync.WaitGroup
}

// ─── template constructor + spec-half methods ────────────────────

// New constructs the template Agent. cmd/nightme/agents.go calls this
// from init(); the returned *Agent is held by agent.Builtins as the
// singleton for `name`.
func New(name, command string, args []string) *Agent {
	return &Agent{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
	}
}

// Name returns the registry key.
func (a *Agent) Name() string { return a.name }

// Mode reports the HTTP backend. The runtime does not branch on Mode;
// the label is for `nightme agents` and logs. We reuse ModeJSONIO
// because the bridge is wire-format-agnostic at the runtime layer;
// F-OPENCODE §6 notes that a dedicated ModeHTTP would be cleaner but
// is a separate PR.
func (a *Agent) Mode() agent.Mode { return agent.ModeJSONIO }

// Command returns the configured CLI binary (typically "opencode").
func (a *Agent) Command() string { return a.command }

// Args returns a defensive copy of the constructor args. The args
// here are appended after the canonical serve flags (see Start). We
// keep `args` empty by default — the bridge sets its own flags.
func (a *Agent) Args() []string {
	return append([]string(nil), a.args...)
}

// Env returns a defensive copy of the constructor env (always nil for
// opencode; kept for symmetry with the merged agent.Agent interface).
func (a *Agent) Env() []string { return nil }

// Detect verifies the `opencode` binary resolves on PATH. Call before
// Start to surface a friendly "opencode not installed" error rather
// than a confusing exec failure.
func (a *Agent) Detect() error {
	_, err := exec.LookPath(a.command)
	return err
}

// ─── lifecycle ───────────────────────────────────────────────────

// Start spawns `opencode serve` and resolves its HTTP base URL, then
// either creates a new session or resumes the given SessionID. The
// caller is expected to call Close() on the returned Agent when done.
//
// Start clones the receiver — the template in Builtins is untouched.
// The clone gets template fields copied (defensively), runtime fields
// zeroed, then the server process is spawned (which gives us the
// baseURL), then the HTTP session handshake (Create or Get) runs
// synchronously before Start returns.
func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("opencode: workspace is required")
	}

	oLog("Start enter",
		"agent", a.name,
		"command", a.command,
		"workspace", cfg.Workspace,
		"resume_id", cfg.SessionID,
	)

	// Retry wrapper. Long-lived bridge = a single failed server
	// start would otherwise kill the chat session; a single retry
	// covers the common case of a stale HOME/.opencode state from
	// a previous interrupted run (the root cause of TestE2E_Interrupt
	// hanging on the third server spawn). Auth / config errors are
	// surfaced immediately via isUnrecoverableStartErr — we don't
	// retry those.
	var live *Agent
	var err error
	for attempt := 1; attempt <= startupMaxAttempts; attempt++ {
		live, err = a.startOnce(ctx, cfg)
		if err == nil {
			return live, nil
		}
		if isUnrecoverableStartErr(err) {
			return nil, err
		}
		oLog("Start: attempt failed, retrying",
			"attempt", attempt,
			"max", startupMaxAttempts,
			"err", err.Error(),
		)
		if attempt < startupMaxAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(startupRetryDelay):
			}
		}
	}
	return nil, err
}

// startOnce does the actual work of Start without retry. Returns a
// live *Agent on success; on failure the partial state has already
// been torn down (live.Close called). Caller decides whether to retry.
func (a *Agent) startOnce(ctx context.Context, cfg agent.StartConfig) (*Agent, error) {
	live := &Agent{
		name:        a.name,
		command:     a.command,
		args:        append([]string(nil), a.args...),
		events:      make(chan agent.AgentEvent, eventBufferSize),
		workspace:   cfg.Workspace,
		branch:      detectBranch(cfg.Workspace),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
	}

	proc, err := startServer(ctx, serverConfig{
		workspace: cfg.Workspace,
		env:       cfg.Env,
		args:      a.args,
	})
	if err != nil {
		_ = live.Close()
		return nil, err
	}
	live.server = proc

	live.client = newClient(proc, cfg.Workspace)
	// Re-add auth header from environment if the operator set it.
	if pw := os.Getenv("OPENCODE_SERVER_PASSWORD"); pw != "" {
		live.client.setPassword(pw)
	}

	// Session handshake: create or resume.
	hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	var s *Session
	resumed := false
	if cfg.SessionID != "" {
		// Resume path. We try GET /api/session/{id} first; if the
		// server has forgotten the session (e.g. fresh server with
		// cleared cache, or different data dir) we fall through
		// to create. We DO NOT silently drop the SessionID — that
		// would surprise the operator; we log loudly and proceed
		// with a fresh session so the operator at least sees the
		// context_loss signal in the log.
		var hsErr error
		s, hsErr = live.client.GetSession(hsCtx, cfg.SessionID)
		if hsErr != nil {
			oLog("Start: resume failed, falling back to fresh session",
				"session_id", cfg.SessionID, "err", hsErr.Error())
			s, hsErr = live.client.CreateSession(hsCtx, CreateSessionOpts{})
			if hsErr != nil {
				_ = live.Close()
				return nil, fmt.Errorf("opencode: create session (fallback): %w", hsErr)
			}
		} else {
			resumed = true
		}
	} else {
		var hsErr error
		s, hsErr = live.client.CreateSession(hsCtx, CreateSessionOpts{})
		if hsErr != nil {
			_ = live.Close()
			return nil, fmt.Errorf("opencode: create session: %w", hsErr)
		}
	}
	live.sessionID = s.ID
	oLog("session handshake complete",
		"session_id", live.sessionID,
		"resumed", resumed,
		"requested_id", cfg.SessionID,
	)

	// live.model stays empty: model selection is opencode's job,
	// not the bridge's. opencode reads ~/.local/state/opencode/
	// model.json at session creation time and picks the user's
	// most-recent selection. runtime doesn't surface the model
	// in EventAgentReady.Model (the only model-aware runtime
	// hook would be SetModel, which is exposed for completeness
	// but is intentionally not part of the runtime's normal
	// surface — there's no /model slash command in nightme).

	live.trans = newTranslator(
		live.deliver,
		a.name,
		cfg.Workspace,
		live.branch,
		live.sessionID,
		live.model,
	)

	// Synthesize the initial EventAgentReady.
	live.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: live.sessionID,
		Model:     live.model,
		AgentName: a.name,
		Workspace: cfg.Workspace,
		Branch:    live.branch,
	})

	// Start the SSE reader + lifecycle goroutine. Once these are
	// running the events channel is the source of truth for the
	// runtime.
	sseCtx, sseCancel := context.WithCancel(ctx)
	live.sseCancel = sseCancel
	body, err := live.client.Subscribe(sseCtx, live.sessionID)
	if err != nil {
		_ = live.Close()
		return nil, fmt.Errorf("opencode: subscribe: %w", err)
	}
	go live.readSSE(body)
	go live.lifecycle()

	// Tear down on parent context cancellation.
	go func() {
		<-ctx.Done()
		_ = live.Close()
	}()

	oLog("Start return ok",
		"pid", live.server.pid,
		"session_id", live.sessionID,
		"base_url", live.server.baseURL,
	)
	return live, nil
}

// ─── live-half methods ───────────────────────────────────────────

// Events returns the read-only event channel. Closed by the lifecycle
// goroutine when the server process exits (or by Close()).
func (a *Agent) Events() <-chan agent.AgentEvent { return a.events }

// PID returns the OS process id of the `opencode serve` child, or 0
// when the session has no process (e.g. before Start).
func (a *Agent) PID() int {
	if a.server == nil {
		return 0
	}
	return a.server.pid
}

// isUnrecoverableStartErr returns true for start errors that
// retrying cannot fix. The retry wrapper in Start skips the next
// attempt when this returns true. Currently:
//
//   - binary not found ("executable file not found") — the same
//     missing binary will not magically appear.
//   - "command not found" — same root cause.
//   - context.Canceled / context.DeadlineExceeded — the caller
//     already gave up.
//
// Network / timeout / "stale HOME state" errors are considered
// recoverable.
func isUnrecoverableStartErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "executable file not found") ||
		strings.Contains(msg, "command not found") ||
		strings.Contains(msg, "no such file")
}

// ─── user input ──────────────────────────────────────────────────

// SendText delivers a plain-text user turn. Convenience wrapper around
// SendBlocks.
func (a *Agent) SendText(text string) error {
	if text == "" {
		return nil
	}
	return a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: text},
	})
}

// SendBlocks delivers a structured user turn. The bridge maps:
//
//	ContentText  → {type:"text", text:t}
//	ContentImage → {type:"file", mime, url:"file://..."}  (opencode reads bytes)
//	ContentFile  → {type:"file", mime, url:"file://..."}
//
// Empty blocks is a no-op. Concurrent calls during a streaming turn
// are rejected with ErrTurnBusy (the bridge single-turns per session
// to keep the SSE event ordering sane).
func (a *Agent) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	if a.server == nil {
		return fmt.Errorf("opencode: not started")
	}
	if len(blocks) == 0 {
		return nil
	}
	a.pendingMu.Lock()
	if a.pendingTurnActive {
		a.pendingMu.Unlock()
		return ErrTurnBusy
	}
	a.pendingTurnActive = true
	a.pendingMu.Unlock()
	// pendingTurnActive is released by the SSE handler when it sees
	// session.idle. We do NOT defer-clear here — the turn is not
	// finished until the server says so.

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, promptTimeout)
		defer cancelTimeout()
	}

	parts := make([]PartInput, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			if b.Text == "" {
				continue
			}
			parts = append(parts, TextPart(b.Text))
		case agent.ContentImage:
			// Inline base64. opencode's HTTP API accepts
			// `data:<mime>;base64,<payload>` URLs in the
			// FilePartInput.url field. We read the file from
			// disk, base64-encode, and emit a data URL so the
			// model sees the bytes immediately without a
			// separate file fetch.
			//
			// Size guard: cap to maxImageBytes (10 MiB raw).
			// A base64-encoded 10 MiB image is ~13 MiB which
			// still fits well within SSE JSON line limits.
			if b.Path == "" {
				continue
			}
			data, err := os.ReadFile(b.Path)
			if err != nil {
				a.pendingMu.Lock()
				a.pendingTurnActive = false
				a.pendingMu.Unlock()
				return fmt.Errorf("opencode: read image %s: %w", b.Path, err)
			}
			if int64(len(data)) > maxImageBytes {
				a.pendingMu.Lock()
				a.pendingTurnActive = false
				a.pendingMu.Unlock()
				return fmt.Errorf("%w: %s = %d bytes", ErrImageTooLarge, b.Path, len(data))
			}
			mime := b.MediaType
			if mime == "" {
				mime = "image/png"
			}
			encoded := base64.StdEncoding.EncodeToString(data)
			parts = append(parts, FilePart(mime, "data:"+mime+";base64,"+encoded))
		case agent.ContentFile:
			if b.Path == "" {
				continue
			}
			// Non-image files keep file:// URLs: opencode reads
			// them itself, no need to round-trip the bytes.
			info, err := os.Stat(b.Path)
			if err != nil {
				a.pendingMu.Lock()
				a.pendingTurnActive = false
				a.pendingMu.Unlock()
				return fmt.Errorf("opencode: stat %s: %w", b.Path, err)
			}
			if info.Size() > maxImageBytes {
				a.pendingMu.Lock()
				a.pendingTurnActive = false
				a.pendingMu.Unlock()
				return fmt.Errorf("%w: %s = %d bytes", ErrImageTooLarge, b.Path, info.Size())
			}
			parts = append(parts, FilePart(b.MediaType, "file://"+b.Path))
		default:
			oLog("SendBlocks: unknown block type, skipping", "type", string(b.Type))
		}
	}
	if len(parts) == 0 {
		// Nothing to send — release the busy guard so the next
		// SendBlocks with non-empty input can proceed.
		a.pendingMu.Lock()
		a.pendingTurnActive = false
		a.pendingMu.Unlock()
		return nil
	}

	if _, err := a.client.Prompt(ctx, a.sessionID, parts); err != nil {
		// Prompt failed before the turn even started — release the
		// guard so the next SendBlocks can proceed. The normal
		// release path is the session.idle SSE event.
		a.pendingMu.Lock()
		a.pendingTurnActive = false
		a.pendingMu.Unlock()
		return err
	}
	// Prompt was admitted — reset the per-turn content flag on the
	// translator so the next EventAgentDone can distinguish "agent
	// produced text" from "(empty response)", and restart the
	// watchdog clock. The watchdog only fires while pendingTurnActive
	// is true; on a session.idle / step.ended the busy-guard release
	// cancels it.
	a.trans.ResetTurn()
	a.lastEventAtUnixNano.Store(time.Now().UnixNano())
	go a.watchdog()
	return nil
}

// Abort sends /interrupt to cancel the in-flight turn. Implements
// the agent.Agent.Abort contract (stage 6, commit e9886ec) — the
// runtime calls this directly through the interface.
//
// Abort is best-effort: if the server is unreachable, the error
// is returned to the caller but the bridge is not torn down. The
// turn will eventually settle on its own via session.idle and the
// pendingTurnActive guard will release.
func (a *Agent) Abort(ctx context.Context) error {
	if a.server == nil {
		return fmt.Errorf("opencode: not started")
	}
	if a.sessionID == "" {
		return fmt.Errorf("opencode: no session")
	}
	return a.client.Interrupt(ctx, a.sessionID)
}

// SetModel switches the active model on the running session. The
// next turn will use the new model; in-flight turns are not affected.
// Mirrors what /use would do at the runtime layer.
func (a *Agent) SetModel(ctx context.Context, providerID, modelID string) error {
	if a.server == nil {
		return fmt.Errorf("opencode: not started")
	}
	if a.sessionID == "" {
		return fmt.Errorf("opencode: no session")
	}
	return a.client.SetModel(ctx, a.sessionID, providerID, modelID)
}

// Compact asks the server to compact the conversation history. The
// session id is unchanged; the next /prompt response carries the
// fresh token totals so the channel footer can refresh without
// waiting for the model to error out on context overflow.
//
// The session.compacted SSE event fires after success; the
// runtime's translator already emits a fresh EventAgentReady
// so the channel can refresh the header.
func (a *Agent) Compact(ctx context.Context) error {
	if a.server == nil {
		return fmt.Errorf("opencode: not started")
	}
	if a.sessionID == "" {
		return fmt.Errorf("opencode: no session")
	}
	return a.client.Compact(ctx, a.sessionID)
}

// ListSessions returns the sessions known to the server, scoped
// to the bridge's workspace. The runtime uses this for a resume
// picker; the bridge itself does not auto-resume from the list.
//
// Bridge-independent: this call does not require an active
// session. We use the workspace-scoped header so the server
// returns only sessions for this project.
func (a *Agent) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	if a.client == nil {
		return nil, fmt.Errorf("opencode: not started")
	}
	return a.client.ListSessions(ctx, limit)
}

// ListProviders returns the configured providers. Bridge-
// independent; the runtime uses it for a /model picker.
func (a *Agent) ListProviders(ctx context.Context) ([]Provider, error) {
	if a.client == nil {
		return nil, fmt.Errorf("opencode: not started")
	}
	return a.client.ListProviders(ctx)
}

// ListModels returns the active model catalog. Bridge-
// independent.
func (a *Agent) ListModels(ctx context.Context) (map[string]any, error) {
	if a.client == nil {
		return nil, fmt.Errorf("opencode: not started")
	}
	return a.client.ListModels(ctx)
}

// AvailableBuiltinCommands returns the slash command names opencode
// has advertised via the latest available_commands_update SSE event.
// This is informational — opencode's HTTP API does not (yet)
// expose a /command endpoint that would let us execute them, so
// the runtime shim currently forwards commands as plain text
// prompts to the model. Exposing the list lets the shim:
//
//   - render a help menu that includes opencode builtins
//   - mark a passthrough input as "opencode builtin" for log
//     clarity without changing the wire payload
//
// Returns nil before the first available_commands_update event
// arrives (typically within ~1s of /api/event subscription) and
// before Start.
func (a *Agent) AvailableBuiltinCommands() []string {
	if a.trans == nil {
		return nil
	}
	return a.trans.AvailableBuiltinCommands()
}

// IsBuiltinCommand returns true if name (with or without leading
// "/") is in opencode's advertised command list. Returns false
// when no commands have been advertised yet, when the bridge is
// not running, or when the name is unknown.
func (a *Agent) IsBuiltinCommand(name string) bool {
	if a.trans == nil {
		return false
	}
	return a.trans.IsBuiltinCommand(name)
}


// ─── permission response ─────────────────────────────────────────

// SendPermission responds to the most recent permission.asked event.
// The argument is interpreted as:
//
//	"once" / "always" / "reject" → passed verbatim to the server
//	"accept"                      → mapped to "once" (Claude-style alias)
//
// Empty resp is treated as "reject" so a no-op call cannot wedge the
// bridge.
func (a *Agent) SendPermission(resp string) error {
	if resp == "" {
		resp = "reject"
	}
	switch strings.ToLower(resp) {
	case "accept", "allow", "allow-once", "allow_once":
		resp = "once"
	case "deny", "decline", "reject-once", "reject_once":
		resp = "reject"
	}
	a.pendingMu.Lock()
	id := a.pendingApprovalID
	a.pendingApprovalID = ""
	a.pendingMu.Unlock()

	if id == "" {
		return ErrNoPendingPermission
	}
	if err := a.client.ReplyPermission(context.Background(), a.sessionID, id, resp); err != nil {
		// Restore the id so the operator can retry.
		a.pendingMu.Lock()
		if a.pendingApprovalID == "" {
			a.pendingApprovalID = id
		}
		a.pendingMu.Unlock()
		return err
	}
	return nil
}

// ─── /new (F-34) ─────────────────────────────────────────────────

// New resets the conversation context on the running session. The
// opencode server keeps the same session id; we emit a fresh
// EventAgentReady so the runtime can refresh its session id cache.
//
// In practice opencode does not have a dedicated "reset" endpoint.
// The runtime treats this as a no-op-with-fresh-EventAgentReady;
// operators who want a clean slate should kill the session and Spawn
// a new one.
func (a *Agent) New(ctx context.Context) error {
	if a.server == nil {
		return fmt.Errorf("opencode: not started")
	}
	hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	s, err := a.client.GetSession(hsCtx, a.sessionID)
	if err != nil {
		return fmt.Errorf("opencode: GET session on /new: %w", err)
	}
	a.sessionID = s.ID
	a.trans.sessionID = s.ID
	a.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: a.sessionID,
		Model:     a.model,
		AgentName: a.name,
		Workspace: a.workspace,
		Branch:    a.branch,
	})
	return nil
}

// ─── close ───────────────────────────────────────────────────────

// Close terminates the session: tells the server process to exit,
// waits for the lifecycle goroutine to drain, then returns. Idempotent.
// Waits up to closeDrainTimeout for the underlying cmd.Wait goroutine
// so that a wedged reap cannot block the runtime's spawn path
// indefinitely.
func (a *Agent) Close() error {
	var firstErr error
	a.closeOnce.Do(func() {
		close(a.closed)
		// Cancel the SSE stream context so readSSE exits. This
		// must run before server.Close so the in-flight HTTP
		// request is cancelled (otherwise readSSE blocks on the
		// body until the server tears down).
		if a.sseCancel != nil {
			a.sseCancel()
		}
		if a.server != nil {
			firstErr = a.server.Close()
		}
	})
	// Wait outside closeOnce so the lifecycle goroutine can always
	// close exitDone even if our closeOnce is held.
	select {
	case <-a.exitDone:
	case <-time.After(closeDrainTimeout):
		if firstErr == nil {
			firstErr = fmt.Errorf("opencode: close drain timed out after %s", closeDrainTimeout)
		}
	}
	return firstErr
}

// ─── deliver ─────────────────────────────────────────────────────

// deliver stamps the bridge-side session context onto every event
// before delivery, then blocks on pushing onto the events channel
// until either the runtime drains it OR the session is closed / the
// lifecycle has reaped the server.
//
// Producer-side contract (matches codex / pi / claudecode / pty / acp,
// promoted in commit 67b295ec):
//   - No `default:` instant-drop. The producer is allowed to block
//     until the consumer (runtime readpump) drains. The channel's
//     40960 buffer absorbs bursts.
//   - No timeout drop. A `case <-time.After(1s)` branch was the
//     root cause of the F-54 "bridge reset: pi: new_session:
//     context deadline exceeded" incident.
//   - Close signals release a parked deliver(). This prevents leaked
//     goroutines after the session is torn down.
func (a *Agent) deliver(ev agent.AgentEvent) agent.AgentEvent {
	if a.server == nil {
		return ev
	}
	if ev.SessionID == "" {
		ev.SessionID = a.sessionID
	}
	if ev.Model == "" {
		ev.Model = a.model
	}
	ev.AgentName = a.name
	ev.Workspace = a.workspace
	ev.Branch = a.branch

	// Note: the per-turn "did the agent produce any content?" flag
	// lives on the translator (trans.turnHadContent) — it is set
	// by translator.markContent() before deliver() is called for
	// text / tool events, and consumed at the terminal-event branch
	// to pick Done.Reason = "settled" vs "empty". We intentionally
	// do NOT mirror the flag here on Agent; the previous
	// agent.turnHadContent shadow was set but never read (stage 8
	// review).

	select {
	case a.events <- ev:
	case <-a.stopDeliver:
		// Lifecycle has begun teardown. Drop silently.
	case <-a.closed:
		oLog("deliver dropped (session closed)", "kind", ev.Kind.String())
	case <-a.exitDone:
		// lifecycle closed exitDone after wait returned; the bridge
		// is being torn down. Drop silently.
	}
	return ev
}

// ─── SSE reader + lifecycle ──────────────────────────────────────

// readSSE owns the SSE body. It blocks on decodeSSE until the server
// closes the stream (graceful EOF → nil) or the wire errors. We
// delegate event handling to the translator; this function never
// touches the events channel directly.
func (a *Agent) readSSE(body io.ReadCloser) {
	a.pumpWG.Add(1)
	defer a.pumpWG.Done()
	defer body.Close()
	defer a.finishTurn()

	// Stamp the watchdog's "last event" timer immediately so a
	// quiet SSE stream from the start does not look like a hang
	// (the watchdog only fires when pendingTurnActive is true, but
	// the clock must be moving from the moment we subscribe).
	a.lastEventAtUnixNano.Store(time.Now().UnixNano())

	err := decodeSSE(body, func(ev SessionEvent) error {
		// Every SSE frame — including unknown / plugin / catalog
		// chatter — proves the server is alive. Reset the watchdog
		// clock here so plugin load storms don't trigger false
		// positives on a quiet model turn.
		a.lastEventAtUnixNano.Store(time.Now().UnixNano())
		// Stamp the request id onto the permission event so the
		// reply goroutine can route SendPermission back.
		if ev.Type == "permission.asked" {
			var p PermissionAsked
			if err := json.Unmarshal(ev.properties(), &p); err == nil {
				a.pendingMu.Lock()
				a.pendingApprovalID = p.ID
				a.pendingMu.Unlock()
			}
		}
		// Release pendingTurnActive at the per-turn terminal event.
		// The earliest reliable signal is session.idle; we also
		// release on session.error so a failed turn does not lock
		// the bridge forever (the F-54 / F-32 lesson).
		switch ev.Type {
		case "session.idle", "session.error":
			a.pendingMu.Lock()
			a.pendingTurnActive = false
			a.pendingMu.Unlock()
		}
		return a.trans.handleEvent(ev)
	})
	if err != nil {
		oLog("sse read error", "err", err.Error())
	}
}

// finishTurn is called when the SSE stream closes. It releases the
// pendingTurnActive guard so the next SendBlocks can proceed. The
// retry trap (turnActive not released) was the F-54 / F-32 root
// cause for the other bridges; we mirror the same fix.
func (a *Agent) finishTurn() {
	a.pendingMu.Lock()
	a.pendingTurnActive = false
	a.pendingMu.Unlock()
}

// lifecycle is the single owner of the events-channel close. Once-close
// semantics are enforced by the closeOnce in Close(); everything else
// just nudges the process toward a clean exit.
//
// Order is critical:
//   1. Wait for the SSE reader to drain (pumpWG.Wait). This prevents
//      a concurrent readSSE from racing with close(events).
//   2. Close stopDeliver so any in-flight deliver backs off.
//   3. Close events.
//   4. Close exitDone so Close() can return.
//
// We block on either the server cmd exiting (real production path) or
// the close signal (test path — synthetic agents without a real cmd).
func (a *Agent) lifecycle() {
	defer close(a.exitDone)
	if a.server != nil && a.server.cmd != nil {
		// Production path: wait for the server to exit. Real
		// cmd.Wait blocks until the child exits.
		_, _ = a.server.cmd.Process.Wait()
	} else {
		// Test path: block until Close() is called.
		<-a.closed
	}
	// Wait for the SSE reader to drain before we close events.
	// Without this, a slow readSSE can race with close(events)
	// and panic when deliver tries to send.
	a.pumpWG.Wait()
	// Close stopDeliver FIRST so any in-flight deliver backs off
	// before we close events. Without this, a concurrent
	// `case a.events <- ev:` could panic on a closed channel.
	close(a.stopDeliver)
	close(a.events)
}

// ─── turn watchdog ───────────────────────────────────────────────

// watchdogTimeout returns the configured turn watchdog timeout,
// with NIGHTME_OPENCODE_TURN_WATCHDOG as the per-deployment
// override (e.g. operators with a slow enterprise model bump
// this to 30m). Returns 0 to disable the watchdog entirely.
func watchdogTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("NIGHTME_OPENCODE_TURN_WATCHDOG"))
	if v == "" {
		return turnWatchdogTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return turnWatchdogTimeout
	}
	return d
}

// watchdog is a per-turn self-healing timer. Patterned after
// cc-connect (defaultEventIdleTimeout in their engine.go:953):
// the timer is reset on every SSE event (readSSE writes
// lastEventAtUnixNano on each frame). If the gap exceeds the
// threshold while a turn is pending, we kill the server,
// synthesise an EventAgentError, and let the runtime readpump
// surface a clear "agent session timed out (no response)" message
// instead of leaving the chat stuck on the busy spinner.
//
// The watchdog exits as soon as the busy-guard drops (terminal
// event arrived) or the bridge closes (Close was called).
func (a *Agent) watchdog() {
	timeout := watchdogTimeout()
	if timeout <= 0 {
		return
	}
	// Tick at timeout/10 so a 10-minute production timeout wakes
	// us every minute (cheap) and a 200ms test timeout wakes us
	// every 20ms (fast enough that tests don't sleep for 10s).
	// Capped at 5s so a 1-hour test override still wakes the
	// watchdog within a reasonable wall time.
	tickInterval := timeout / 10
	if tickInterval < 20*time.Millisecond {
		tickInterval = 20 * time.Millisecond
	} else if tickInterval > 5*time.Second {
		tickInterval = 5 * time.Second
	}
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.closed:
			return
		case <-ticker.C:
			a.pendingMu.Lock()
			busy := a.pendingTurnActive
			a.pendingMu.Unlock()
			if !busy {
				// Turn settled cleanly before the deadline.
				return
			}
			lastEvent := time.Unix(0, a.lastEventAtUnixNano.Load())
			if time.Since(lastEvent) < timeout {
				// Activity within the window — keep waiting.
				continue
			}
			// Timed out. Kill the server, deliver the error,
			// and close the bridge. The runtime's readpump
			// will surface the error to the user.
			oLog("watchdog: turn timeout, killing session",
				"timeout", timeout,
				"since_last_event", time.Since(lastEvent),
			)
			a.deliver(agent.AgentEvent{
				Kind:      agent.EventAgentError,
				SessionID: a.sessionID,
				Model:     a.model,
				AgentName: a.name,
				Workspace: a.workspace,
				Branch:    a.branch,
				Err:       fmt.Errorf("opencode: turn watchdog timeout (no events for %s)", timeout),
			})
			_ = a.Close()
			return
		}
	}
}

// detectBranch returns the current git branch for workspace, or "" on
// any failure (non-git workspace, git not installed, detached HEAD).
// Mirrors the helper used by the codex / pi bridges.
func detectBranch(workspace string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Compile-time guarantee that *Agent satisfies agent.Agent.
var _ agent.Agent = (*Agent)(nil)
