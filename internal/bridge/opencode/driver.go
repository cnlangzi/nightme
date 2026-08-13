
// Package opencode — driver (runtime state) for the opencode HTTP
// bridge. Created by Starter.Start; wrapped by agent.NewAgent so
// runtime consumers (AgentSession, channel layer) see a uniform
// agent.Agent surface regardless of which bridge spawn'd it.
//
// The driver holds:
//   - HTTP client + server process references
//   - Event channel + delivery helpers
//   - translator instance (SSE → AgentEvent)
//   - Watchdog goroutine state
//   - Turn-tracking flags (pendingTurnActive, turnHadContent)
//
// It implements the unexported agent.driver interface (SendBlocks,
// SendBlocks, SendPermission, Reset, Close) and exposes bridge-
// specific extensions (Compact, ListSessions, AvailableBuiltinCommands,
// Stop, SetModel) for callers that type-assert *agent.Agent.
package opencode

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── driver struct ─────────────────────────────────────────────────

// driver is the runtime state of one opencode session. Created by
// newDriver, wrapped into *agent.Agent by Starter.Start, never
// exposed externally.
type driver struct {
	// ─── template fields (carried over from Starter) ─────────
	name    string
	command string
	args    []string

	// ─── runtime state (populated by newDriver) ─────────────
	server    *serverProc
	client    *Client
	events    chan agent.AgentEvent
	workspace string
	branch    string
	sessionID string
	model     string
	trans     *translator

	pendingMu         sync.Mutex
	pendingTurnActive bool
	pendingApprovalID string // request id of the most recent permission.asked

	lastEventAtUnixNano atomic.Int64

	closeOnce   sync.Once
	closed      chan struct{}
	stopDeliver chan struct{}
	exitDone    chan struct{}

	sseCancel context.CancelFunc

	pumpWG sync.WaitGroup
}

// newDriver is invoked from Starter.Start. It spawns the
// `opencode serve` subprocess, parses the bound URL from stdout,
// runs the session handshake (create or resume), subscribes to the
// SSE event bus, and returns a fully-wired *driver.
//
// Args are the command's protocol flags (passed in from the
// Starter). cfg.Args are appended after them (user wins);
// cfg.Env is merged with the starter's defaults (cfg wins).
func newDriver(ctx context.Context, s *Starter, cfg agent.StartConfig) (*driver, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("opencode: workspace is required")
	}

	oLog("Start enter",
		"agent", s.name,
		"command", s.command,
		"workspace", cfg.Workspace,
		"resume_id", cfg.SessionID,
	)

	d := &driver{
		name:      s.name,
		command:   s.command,
		args:      append([]string(nil), s.args...),
		events:    make(chan agent.AgentEvent, eventBufferSize),
		workspace: cfg.Workspace,
		branch:    detectBranch(cfg.Workspace),
		closed:    make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:  make(chan struct{}),
	}

	// Retry wrapper. Long-lived bridge = a single failed server
	// start would otherwise kill the chat session; a single retry
	// covers the common case of a stale HOME/.opencode state from
	// a previous interrupted run (the root cause of TestE2E_Interrupt
	// hanging on the third server spawn). Auth / config errors are
	// surfaced immediately via isUnrecoverableStartErr — we don't
	// retry those.
	var startErr error
	for attempt := 1; attempt <= startupMaxAttempts; attempt++ {
		err := d.bootServerAndHandshake(ctx, s, cfg)
		if err == nil {
			break
		}
		if isUnrecoverableStartErr(err) {
			_ = d.Close()
			return nil, err
		}
		startErr = err
		oLog("Start: attempt failed, retrying",
			"attempt", attempt,
			"max", startupMaxAttempts,
			"err", err.Error(),
		)
		if attempt < startupMaxAttempts {
			select {
			case <-ctx.Done():
				_ = d.Close()
				return nil, ctx.Err()
			case <-time.After(startupRetryDelay):
			}
		}
	}
	if startErr != nil {
		_ = d.Close()
		return nil, startErr
	}

	d.trans = newTranslator(
		d.deliver,
		d.name,
		d.workspace,
		d.branch,
		d.sessionID,
		d.model,
	)

	// Synthesize the initial EventAgentReady.
	d.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: d.sessionID,
		Model:     d.model,
		AgentName: d.name,
		Workspace: d.workspace,
		Branch:    d.branch,
	})

	// Start the SSE reader + lifecycle goroutine. Once these are
	// running the events channel is the source of truth for the
	// runtime.
	sseCtx, sseCancel := context.WithCancel(ctx)
	d.sseCancel = sseCancel
	body, err := d.client.Subscribe(sseCtx, d.sessionID)
	if err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("opencode: subscribe: %w", err)
	}
	go d.readSSE(body)
	go d.lifecycle()

	// Tear down on parent context cancellation.
	go func() {
		<-ctx.Done()
		_ = d.Close()
	}()

	oLog("Start return ok",
		"pid", d.server.pid,
		"session_id", d.sessionID,
		"base_url", d.server.baseURL,
	)
	return d, nil
}

// bootServerAndHandshake spawns the opencode subprocess, parses
// the bound URL, and runs the session handshake (create or resume).
// Factored out of newDriver so the retry loop can call it cleanly.
func (d *driver) bootServerAndHandshake(ctx context.Context, s *Starter, cfg agent.StartConfig) error {
	proc, err := startServer(ctx, serverConfig{
		workspace: cfg.Workspace,
		env:       cfg.Env,
		args:      d.args,
	})
	if err != nil {
		return err
	}
	d.server = proc

	d.client = newClient(proc, cfg.Workspace)
	if pw := os.Getenv("OPENCODE_SERVER_PASSWORD"); pw != "" {
		d.client.setPassword(pw)
	}

	// Session handshake: create or resume.
	hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	var sess *Session
	resumed := false
	if cfg.SessionID != "" {
		var hsErr error
		sess, hsErr = d.client.GetSession(hsCtx, cfg.SessionID)
		if hsErr != nil {
			oLog("Start: resume failed, falling back to fresh session",
				"session_id", cfg.SessionID, "err", hsErr.Error())
			sess, hsErr = d.client.CreateSession(hsCtx, CreateSessionOpts{})
			if hsErr != nil {
				return fmt.Errorf("opencode: create session (fallback): %w", hsErr)
			}
		} else {
			resumed = true
		}
	} else {
		var hsErr error
		sess, hsErr = d.client.CreateSession(hsCtx, CreateSessionOpts{})
		if hsErr != nil {
			return fmt.Errorf("opencode: create session: %w", hsErr)
		}
	}
	d.sessionID = sess.ID
	oLog("session handshake complete",
		"session_id", d.sessionID,
		"resumed", resumed,
		"requested_id", cfg.SessionID,
	)
	return nil
}

// ─── driver implements agent.driver ────────────────────────────────

// SendBlocks delivers a structured user turn. Encodes image/file
// blocks inline (base64 for images, file:// URL for non-images) and
// the text part as a flattened prompt body — see client.Prompt for
// the on-wire shape.
//
// The turn watchdog fires on every SendBlocks: after the prompt is
// admitted to the server, a goroutine watches the SSE stream and
// kills the session if it goes silent for too long. Empty blocks
// slice is a no-op.
func (d *driver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	if d.server == nil {
		return fmt.Errorf("opencode: not started")
	}
	if len(blocks) == 0 {
		return nil
	}
	d.pendingMu.Lock()
	if d.pendingTurnActive {
		d.pendingMu.Unlock()
		return ErrTurnBusy
	}
	d.pendingTurnActive = true
	d.pendingMu.Unlock()

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
			if b.Path == "" {
				continue
			}
			data, err := os.ReadFile(b.Path)
			if err != nil {
				d.pendingMu.Lock()
				d.pendingTurnActive = false
				d.pendingMu.Unlock()
				return fmt.Errorf("opencode: read image %s: %w", b.Path, err)
			}
			if int64(len(data)) > maxImageBytes {
				d.pendingMu.Lock()
				d.pendingTurnActive = false
				d.pendingMu.Unlock()
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
			info, err := os.Stat(b.Path)
			if err != nil {
				d.pendingMu.Lock()
				d.pendingTurnActive = false
				d.pendingMu.Unlock()
				return fmt.Errorf("opencode: stat %s: %w", b.Path, err)
			}
			if info.Size() > maxImageBytes {
				d.pendingMu.Lock()
				d.pendingTurnActive = false
				d.pendingMu.Unlock()
				return fmt.Errorf("%w: %s = %d bytes", ErrImageTooLarge, b.Path, info.Size())
			}
			parts = append(parts, FilePart(b.MediaType, "file://"+b.Path))
		default:
			oLog("SendBlocks: unknown block type, skipping", "type", string(b.Type))
		}
	}
	if len(parts) == 0 {
		d.pendingMu.Lock()
		d.pendingTurnActive = false
		d.pendingMu.Unlock()
		return nil
	}

	if _, err := d.client.Prompt(ctx, d.sessionID, parts); err != nil {
		d.pendingMu.Lock()
		d.pendingTurnActive = false
		d.pendingMu.Unlock()
		return err
	}
	d.trans.ResetTurn()
	d.lastEventAtUnixNano.Store(time.Now().UnixNano())
	go d.watchdog()
	return nil
}

// SendPermission responds to the most recent permission.asked event.
// The argument is interpreted as: "once" / "always" / "reject" passed
// verbatim to the server; "accept" maps to "once".
func (d *driver) SendPermission(resp string) error {
	if d.pendingApprovalID == "" {
		return ErrNoPendingPermission
	}
	if d.client == nil {
		return fmt.Errorf("opencode: not started")
	}
	switch strings.ToLower(resp) {
	case "accept", "allow", "allow-once", "allow_once":
		resp = "once"
	case "deny", "decline", "reject-once", "reject_once":
		resp = "reject"
	}
	d.pendingMu.Lock()
	id := d.pendingApprovalID
	d.pendingMu.Unlock()
	return d.client.ReplyPermission(context.Background(), d.sessionID, id, resp)
}

// Reset signals that the bridge cannot reset conversation context
// in-place. The agent package's wrapper layer catches this sentinel
// and falls back to kill-and-respawn via the configured Spawner.
func (d *driver) Reset(ctx context.Context) error {
	_ = ctx
	return agent.ErrRestartRequired
}

// PID returns the OS process id of the underlying opencode subprocess.
func (d *driver) PID() int {
	if d.server == nil {
		return 0
	}
	return d.server.pid
}

// Close terminates the session: tells the server process to exit,
// waits for the lifecycle goroutine to drain, then returns.
// Idempotent.
func (d *driver) Close() error {
	var firstErr error
	d.closeOnce.Do(func() {
		close(d.closed)
		if d.sseCancel != nil {
			d.sseCancel()
		}
		if d.server != nil {
			firstErr = d.server.Close()
		}
	})
	select {
	case <-d.exitDone:
	case <-time.After(closeDrainTimeout):
		if firstErr == nil {
			firstErr = fmt.Errorf("opencode: close drain timed out after %s", closeDrainTimeout)
		}
	}
	return firstErr
}

// ─── bridge-specific extensions on driver ────────────────────────

// Stop sends /interrupt to halt execution of the in-flight turn.
// The HTTP server emits session.idle (or session.error) and the
// bridge releases the busy guard. The server keeps running and the
// SessionID is preserved — the next prompt reuses the same session.
//
// Stop is fire-and-forget: it does NOT block until the server
// confirms the turn has settled. The chat layer's TryFlush watches
// IsReady() and reschedules the next queued prompt automatically.
func (d *driver) Stop(ctx context.Context) error {
	if d.server == nil {
		return fmt.Errorf("opencode: not started")
	}
	if d.sessionID == "" {
		return fmt.Errorf("opencode: no session")
	}
	return d.client.Interrupt(ctx, d.sessionID)
}

// SetModel switches the active model on the running session.
func (d *driver) SetModel(ctx context.Context, providerID, modelID string) error {
	if d.server == nil {
		return fmt.Errorf("opencode: not started")
	}
	if d.sessionID == "" {
		return fmt.Errorf("opencode: no session")
	}
	return d.client.SetModel(ctx, d.sessionID, providerID, modelID)
}

// Compact asks the server to compact the conversation history.
func (d *driver) Compact(ctx context.Context) error {
	if d.server == nil {
		return fmt.Errorf("opencode: not started")
	}
	if d.sessionID == "" {
		return fmt.Errorf("opencode: no session")
	}
	return d.client.Compact(ctx, d.sessionID)
}

// ListSessions returns the sessions known to the server.
func (d *driver) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	if d.client == nil {
		return nil, fmt.Errorf("opencode: not started")
	}
	return d.client.ListSessions(ctx, limit)
}

// ListProviders returns the configured providers.
func (d *driver) ListProviders(ctx context.Context) ([]Provider, error) {
	if d.client == nil {
		return nil, fmt.Errorf("opencode: not started")
	}
	return d.client.ListProviders(ctx)
}

// ListModels returns the active model catalog.
func (d *driver) ListModels(ctx context.Context) (map[string]any, error) {
	if d.client == nil {
		return nil, fmt.Errorf("opencode: not started")
	}
	return d.client.ListModels(ctx)
}

// AvailableBuiltinCommands returns opencode's advertised commands.
func (d *driver) AvailableBuiltinCommands() []string {
	if d.trans == nil {
		return nil
	}
	return d.trans.AvailableBuiltinCommands()
}

// IsBuiltinCommand checks whether name is an opencode builtin.
func (d *driver) IsBuiltinCommand(name string) bool {
	if d.trans == nil {
		return false
	}
	return d.trans.IsBuiltinCommand(name)
}