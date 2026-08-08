// Public Agent (template + live) for the codex app-server bridge.
//
// Two states share one type:
//
//   - Template state (after New, before Start): only the spec-half
//     fields are populated. Registered in agent.Builtins and held
//     there as a long-lived singleton per name.
//
//   - Live state (after Start, before Close): the receiver is a
//     freshly-allocated clone with runtime fields populated (session,
//     events, deliver, etc.). Calls to Events / PID / Send* / New /
//     Close are valid here.
//
// The template in Builtins is never mutated; Start returns a
// separate *Agent so concurrent Start calls from different chats do
// not interfere with each other.
//
// Session lifetime is two-tier:
//
//   - process: newSession() → cmd.Wait() → close(events)
//   - turn:    turn/start → stream events → turn/completed → EventAgentDone{Reason:"settled"}
//
// The process can carry many turns. EventAgentDone marks the end of
// one turn but does NOT close the events channel; only process exit
// (or Close) does that. This mirrors the contract documented in
// docs/feat/F-32-pi-rpc-bridge.md §3.3 and lets ChatSession's
// readpump continue reading across many turns on the same AS.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── errors ───

// ErrTurnBusy is returned by SendBlocks when a previous turn is still
// streaming. The caller may retry after the turn settles.
var ErrTurnBusy = errors.New("codex: previous turn still active")

// ErrImageTooLarge is returned by SendBlocks when a single image
// exceeds maxImageBytes. Mirrors pi's limit.
var ErrImageTooLarge = errors.New("codex: image too large")

const maxImageBytes = 10 * 1024 * 1024

// ─── Agent struct ───

// Agent is the Codex-app-server bridge descriptor. See file doc for
// the template vs live state semantics.
type Agent struct {
	// ─── template fields (set by New; immutable) ───
	name    string
	command string
	args    []string

	// ─── runtime fields (zero before Start; populated on the clone) ───
	session *session

	closeOnce sync.Once
	closed    chan struct{}

	// pendingMu guards pendingTurnActive.
	pendingMu       sync.Mutex
	pendingTurnActive bool
}

// ─── template constructor + spec-half methods ───

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

// Mode reports the structured JSON-IO mode. The runtime does not
// branch on Mode; the label is for `nightme agents` and logs.
func (a *Agent) Mode() agent.Mode { return agent.ModeJSONIO }

// Command returns the configured CLI binary (typically "codex").
// Surfaced by `nightme agents` so users can see what /run would spawn.
func (a *Agent) Command() string { return a.command }

// Args returns a defensive copy of the constructor args. Callers may
// not mutate the returned slice.
func (a *Agent) Args() []string {
	return append([]string(nil), a.args...)
}

// Env returns a defensive copy of the constructor env (always nil for
// codex; kept for symmetry with the merged agent.Agent interface).
func (a *Agent) Env() []string { return nil }

// Detect verifies the `codex` binary resolves on PATH. Call before
// Start to surface a friendly "codex not installed" error rather than
// a confusing exec failure.
func (a *Agent) Detect() error {
	_, err := exec.LookPath(a.command)
	return err
}

// ─── lifecycle ───

// Start spawns codex app-server and returns a live Agent that streams
// events on its Events channel.
//
// Start clones the receiver — the template in Builtins is untouched.
// The clone gets template fields copied (defensively), runtime fields
// zeroed, then exec.CommandContext spawns the process, the read pump
// + stderr drainer + lifecycle goroutine are kicked off, and the
// initialize + thread/start handshake runs synchronously before
// Start returns.
//
// cfg.Workspace is the child process's cwd. cfg.Args / cfg.Env are
// forwarded. cfg.SessionID, when non-empty, is forwarded as
// thread/resume. cfg.PermissionMode is ignored (the app-server uses
// approvalPolicy on a per-turn / per-thread basis; not exposed yet).
func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("codex: workspace is required")
	}

	live := &Agent{
		name:    a.name,
		command: a.command,
		args:    append([]string(nil), a.args...),
		closed:  make(chan struct{}),
	}

	cLog("Start enter",
		"agent", a.name,
		"command", a.command,
		"workspace", cfg.Workspace,
		"resume_id", cfg.SessionID,
	)

	s, err := newSession(ctx, sessionConfig{
		name:      a.name,
		command:   a.command,
		workspace: cfg.Workspace,
		args:      a.args,
		env:       cfg.Env,
		sessionID: cfg.SessionID,
		resume:    cfg.SessionID != "",
	})
	if err != nil {
		return nil, err
	}
	s.deliver = live.deliver
	live.session = s

	// Translator is wired AFTER newSession so we have access to
	// the session's current thread id / model. onTurnEnd releases
	// the busy guard at the per-turn terminal event; the prior
	// defer-in-SendBlocks pattern cleared it the instant turn/start
	// returned, opening a window where a concurrent SendBlocks would
	// race the in-flight turn on codex's side.
	live.session.translator = newTranslator(
		live.deliver, a.name, cfg.Workspace, s.branch, s.stderrTail,
		func() {
			a.pendingMu.Lock()
			a.pendingTurnActive = false
			a.pendingMu.Unlock()
		},
	)

	// Synthesize the initial EventAgentReady. The runtime subscribes
	// to Events(); it will see this immediately and capture the
	// thread id via SetSessionID.
	live.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: s.threadID,
		Model:     s.model,
		AgentName: a.name,
		Workspace: cfg.Workspace,
		Branch:    s.branch,
	})
	cLog("Start: EventAgentReady emitted",
		"pid", s.pid,
		"thread_id", s.threadID,
		"model", s.model,
	)

	go func() {
		<-ctx.Done()
		_ = live.Close()
	}()

	return live, nil
}

// Events streams AgentEvent values until the session ends. The channel
// is closed by the session's lifecycle goroutine only when the
// underlying process (or transport) terminates — NOT after every
// EventAgentDone. Long-lived bridges multiplex many turns over a
// single process; AgentDoneEvent.Reason disambiguates turn-end from
// process-end.
func (a *Agent) Events() <-chan agent.AgentEvent { return a.session.events }

// PID returns the OS process id of the underlying child.
func (a *Agent) PID() int { return a.session.pid }

// ─── user input ───

// SendText delivers plain-text user input. Convenience wrapper around
// SendBlocks.
func (a *Agent) SendText(text string) error {
	cLog("SendText enter", "len", len(text), "text", text)
	return a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: text},
	})
}

// SendBlocks delivers a structured user turn. The bridge maps:
//
//	ContentText  → {type:"text", text, text_elements:[]}
//	ContentImage → stage to disk + {type:"localImage", path}
//	ContentFile  → "@<path>" appended to a text block
//
// Empty blocks is a no-op. Concurrent calls during a streaming turn
// return ErrTurnBusy (the app-server single-turns per thread).
func (a *Agent) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	cLog("SendBlocks enter", "blocks", len(blocks), "threadID", func() string {
		if a.session == nil { return "<nil>" }
		return a.session.threadID
	}())
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
	// pendingTurnActive is released by the translator's onTurnEnd
	// callback when the turn settles (turn/completed, turn/failed,
	// or thread/status/changed.idle). NOT here — clearing on
	// SendBlocks return would re-open the busy window while the
	// turn is still active on codex's side.

	input := make([]turnInput, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			if b.Text == "" {
				continue
			}
			input = append(input, turnInput{
				Type:         "text",
				Text:         b.Text,
				TextElements: []json.RawMessage{},
			})
		case agent.ContentImage:
			path, err := a.stageImage(b)
			if err != nil {
				return err
			}
			input = append(input, turnInput{
				Type: "localImage",
				Path: path,
			})
		case agent.ContentFile:
			if b.Path == "" {
				continue
			}
			input = append(input, turnInput{
				Type: "text",
				Text: "@" + b.Path,
				// TextElements is allowed to be nil for appended lines.
			})
		default:
			cLog("SendBlocks: unknown block type, skipping",
				"type", string(b.Type))
		}
	}
	if len(input) == 0 {
		return nil
	}

	return a.session.rpc.request(ctx, "turn/start", turnStartParams{
		ThreadID: a.session.threadID,
		Input:    input,
	}, nil)
}

// stageImage copies an image from b.Path into a workspace-local
// staging dir and returns the absolute path the app-server can read.
// Matches cc-connect's pattern of creating
// `<workspace>/.nightme/codex/images/img_<ms>_<idx>.<ext>` so
// the path is stable for log debugging and survives a /new.
func (a *Agent) stageImage(b agent.ContentBlock) (string, error) {
	data, err := os.ReadFile(b.Path)
	if err != nil {
		return "", fmt.Errorf("codex: read image: %w", err)
	}
	if len(data) > maxImageBytes {
		return "", fmt.Errorf("%w: %d > %d", ErrImageTooLarge, len(data), maxImageBytes)
	}
	// Sanity-check decode; base64 length does not need to match the
	// decoded size, but the bytes must at least be a valid base64
	// payload for the file extension / mime type.
	dir := filepath.Join(a.session.workspace, ".nightme", "codex", "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("codex: mkdir images: %w", err)
	}
	ext := strings.ToLower(filepath.Ext(b.Path))
	if ext == "" {
		ext = imageExtFromMediaType(b.MediaType)
	}
	fname := fmt.Sprintf("img_%d_%d%s", time.Now().UnixNano(), os.Getpid(), ext)
	dst := filepath.Join(dir, fname)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", fmt.Errorf("codex: write image: %w", err)
	}
	return dst, nil
}

// imageExtFromMediaType maps a MIME type to a file extension. Default
// is .bin (the app-server will sniff the magic bytes anyway).
func imageExtFromMediaType(mt string) string {
	switch strings.ToLower(mt) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}

// ─── permission response ───

// SendPermission responds to the most recent EventAgentPermission.
// The argument is interpreted as:
//
//	"accept"   → JSON-RPC {decision:"accept"}
//	"decline"  → JSON-RPC {decision:"decline"}
//	"<qid>:<labels>|..." → structured requestUserInput response
//
// Empty resp is treated as "decline" so a no-op call cannot wedge
// the bridge.
//
// Routing: in a multi-approval scenario (e.g. an in-flight
// requestUserInput plus a tool approval), the user's choice targets
// the most-recently-emitted approval only. Older approvals keep
// their pending channels untouched and stay pending until their
// own timeout or ctx cancellation. The previous broadcast-all
// behaviour caused concurrent approvals to receive duplicate or
// unrelated responses, which is wrong for both the binary accept/
// decline decision and the structured requestUserInput response
// (whose `<qid>:labels` payload is per-question).
func (a *Agent) SendPermission(resp string) error {
	if resp == "" {
		resp = "decline"
	}
	a.session.pendingMu.Lock()
	defer a.session.pendingMu.Unlock()
	if a.session.lastPendingID == "" {
		return fmt.Errorf("codex: no pending approval")
	}
	id := a.session.lastPendingID
	ch, ok := a.session.pendingApprovals[id]
	if !ok {
		a.session.lastPendingID = ""
		return fmt.Errorf("codex: no pending approval")
	}
	select {
	case ch <- resp:
	default:
		// Channel already has a pending value; do not block.
	}
	delete(a.session.pendingApprovals, id)
	a.session.lastPendingID = ""
	return nil
}

// ─── /new (F-34 §3.2.1) ───

// New resets the conversation context on the running session.
// Stays on the same transport (process); only the thread changes.
// Re-emits EventAgentReady with the new threadId / model so the
// runtime's SetSessionID picks up the fresh id.
func (a *Agent) New(ctx context.Context) error {
	if err := a.session.ensureThread(ctx, ""); err != nil {
		return err
	}
	a.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: a.session.threadID,
		Model:     a.session.model,
		AgentName: a.name,
		Workspace: a.session.workspace,
		Branch:    a.session.branch,
	})
	return nil
}

// ─── close ───

// Close terminates the session: closes stdin (so the child sees EOF
// and exits), waits up to closeDrainTimeout for graceful reap, then
// SIGKILLs if necessary. Idempotent.
func (a *Agent) Close() error {
	var firstErr error
	a.closeOnce.Do(func() {
		close(a.closed)
		if a.session != nil {
			firstErr = a.session.Close()
		}
	})
	return firstErr
}

// ─── deliver ───

// deliver stamps the bridge-side session context onto every event
// before delivery, then pushes onto the events channel. The events
// channel is buffered (size 64); if the runtime is stalled we drop
// the event and emit a synthetic EventAgentError so the operator can
// see the back-pressure incident in the log.
//
// Translators / handlers call this with the event they want to emit;
// the public-facing signature has been kept tight so the call sites
// stay grep-friendly.
func (a *Agent) deliver(ev agent.AgentEvent) agent.AgentEvent {
	if a.session == nil {
		return ev
	}
	ev.SessionID = a.session.threadID
	ev.Model = a.session.model
	ev.AgentName = a.session.agentName
	ev.Workspace = a.session.workspace
	ev.Branch = a.session.branch

	// Back-pressure: try the buffered channel; on full, drop and
	// emit a synthetic EventAgentError so the operator sees it.
	select {
	case a.session.events <- ev:
	default:
		slog.Default().Error("codex: events channel full, dropping event",
			"kind", ev.Kind.String(),
			"session_id", ev.SessionID,
		)
		// Best-effort: synthesise a non-blocking error event so the
		// log captures the back-pressure incident.
		select {
		case a.session.events <- agent.AgentEvent{
			Kind:      agent.EventAgentError,
			Err:       fmt.Errorf("codex: events channel full, dropped %s", ev.Kind),
			SessionID: ev.SessionID,
			Model:     ev.Model,
			AgentName: ev.AgentName,
			Workspace: ev.Workspace,
			Branch:    ev.Branch,
		}:
		default:
			// Even the synthetic error could not be delivered;
			// the runtime is fully wedged. Nothing more we can do.
		}
	}
	return ev
}
