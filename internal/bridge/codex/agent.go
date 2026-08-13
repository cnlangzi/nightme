
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
// separate *driver so concurrent Start calls from different chats do
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
	"os"
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
type driver struct {
	// ─── runtime fields (zero before Start; populated on the clone) ───
	session *session

	closeOnce sync.Once
	closed    chan struct{}

	// pendingMu guards pendingTurnActive.
	pendingMu         sync.Mutex
	pendingTurnActive bool
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
func newDriver(ctx context.Context, s *Starter, cfg agent.StartConfig) (*driver, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("codex: workspace is required")
	}

	live := &driver{
		closed: make(chan struct{}),
	}

	cLog("Start enter",
		"agent", s.name,
		"command", s.command,
		"workspace", cfg.Workspace,
		"resume_id", cfg.SessionID,
	)

	sess, err := newSession(ctx, sessionConfig{
		name:      s.name,
		command:   s.command,
		workspace: cfg.Workspace,
		args:      s.args,
		env:       cfg.Env,
		sessionID: cfg.SessionID,
		resume:    cfg.SessionID != "",
	})
	if err != nil {
		return nil, err
	}
	sess.deliver = live.deliver
	live.session = sess

	// Translator is wired AFTER newSession so we have access to
	// the session's current thread id / model. onTurnEnd releases
	// the busy guard at the per-turn terminal event; the prior
	// defer-in-SendBlocks pattern cleared it the instant turn/start
	// returned, opening a window where a concurrent SendBlocks would
	// race the in-flight turn on codex's side.
	live.session.translator = newTranslator(
		live.deliver, s.name, cfg.Workspace, sess.branch, sess.stderrTail,
		// Captures live (not the template): SendBlocks is
		// called on the live clone, so the busy guard lives on
		// live. Clearing `d.pendingTurnActive` (the template)
		// would leave live's guard stuck at true forever and
		// every subsequent SendBlocks would return ErrTurnBusy.
		func() {
			live.pendingMu.Lock()
			live.pendingTurnActive = false
			live.pendingMu.Unlock()
		},
	)

	// Synthesize the initial EventAgentReady. The runtime subscribes
	// to Events(); it will see this immediately and capture the
	// thread id via SetSessionID.
	live.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: sess.threadID,
		Model:     sess.model,
		AgentName: s.name,
		Workspace: cfg.Workspace,
		Branch:    sess.branch,
	})
	cLog("Start: EventAgentReady emitted",
		"pid", sess.pid,
		"thread_id", sess.threadID,
		"model", sess.model,
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
func (d *driver) Events() <-chan agent.AgentEvent { return d.session.events }

// PID returns the OS process id of the underlying child.
func (d *driver) PID() int { return d.session.pid }

// ─── user input ───

// SendBlocks delivers a structured user turn. The bridge maps:
//
//	ContentText  → {type:"text", text, text_elements:[]}
//	ContentImage → stage to disk + {type:"localImage", path}
//	ContentFile  → "@<path>" appended to a text block
//
// Empty blocks is a no-op. Concurrent calls during a streaming turn
// return ErrTurnBusy (the app-server single-turns per thread).
func (d *driver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	cLog("SendBlocks enter", "blocks", len(blocks), "threadID", func() string {
		if d.session == nil {
			return "<nil>"
		}
		return d.session.threadID
	}())
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
			path, err := d.stageImage(b)
			if err != nil {
				// We never sent a turn — release the busy guard
				// so the next SendBlocks can proceed. The normal
				// release path is the translator's onTurnEnd
				// callback, but that only fires after a turn was
				// actually started.
				d.pendingMu.Lock()
				d.pendingTurnActive = false
				d.pendingMu.Unlock()
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
		// Nothing to send — release the busy guard so the next
		// SendBlocks with non-empty input can proceed. Without
		// this, every subsequent SendBlocks would return
		// ErrTurnBusy since no turn was ever started to clear
		// the guard via onTurnEnd.
		d.pendingMu.Lock()
		d.pendingTurnActive = false
		d.pendingMu.Unlock()
		return nil
	}

	return d.session.rpc.request(ctx, "turn/start", turnStartParams{
		ThreadID: d.session.threadID,
		Input:    input,
	}, nil)
}

// stageImage copies an image from b.Path into a workspace-local
// staging dir and returns the absolute path the app-server can read.
// Matches cc-connect's pattern of creating
// `<workspace>/.nightme/codex/images/img_<ms>_<idx>.<ext>` so
// the path is stable for log debugging and survives a /new.
func (d *driver) stageImage(b agent.ContentBlock) (string, error) {
	data, err := os.ReadFile(b.Path)
	if err != nil {
		return "", fmt.Errorf("codex: read image: %w", err)
	}
	if len(data) > maxImageBytes {
		return "", fmt.Errorf("%w: %d > %d", ErrImageTooLarge, len(data), maxImageBytes)
	}
	// Stage to a workspace-local dir so the app-server (which only
	// sees the workspace) can read it via localImage. We do NOT
	// decode the payload here — content sniffing is the app-server's
	// job; we just need to get the bytes to disk with the right
	// extension so it routes to the correct decoder.
	dir := filepath.Join(d.session.workspace, ".nightme", "codex", "images")
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
func (d *driver) SendPermission(resp string) error {
	if resp == "" {
		resp = "decline"
	}
	d.session.pendingMu.Lock()
	defer d.session.pendingMu.Unlock()
	if d.session.lastPendingID == "" {
		return fmt.Errorf("codex: no pending approval")
	}
	id := d.session.lastPendingID
	ch, ok := d.session.pendingApprovals[id]
	if !ok {
		d.session.lastPendingID = ""
		return fmt.Errorf("codex: no pending approval")
	}
	select {
	case ch <- resp:
	default:
		// Channel already has a pending value; do not block.
	}
	delete(d.session.pendingApprovals, id)
	d.session.lastPendingID = ""
	return nil
}

// ─── /new (F-34 §3.2.1) ───

// New resets the conversation context on the running session.
// Stays on the same transport (process); only the thread changes.
// Re-emits EventAgentReady with the new threadId / model so the
// runtime's SetSessionID picks up the fresh id.
// Reset is the agent.driver interface name for New. Implements
// the agent.driver Reset method (F-34).
func (d *driver) Reset(ctx context.Context) error { return d.New(ctx) }

func (d *driver) New(ctx context.Context) error {
	if err := d.session.ensureThread(ctx, ""); err != nil {
		return err
	}
	d.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: d.session.threadID,
		Model:     d.session.model,
		AgentName: d.session.agentName,
		Workspace: d.session.workspace,
		Branch:    d.session.branch,
	})
	return nil
}

// Stop sends SIGINT to the codex app-server. The codex app-server
// JSON-RPC API does not expose a structured `turn/cancel` method,
// so the closest portable action is the same Ctrl-C signal a user
// would press in interactive mode. The app-server catches it,
// emits `turn/failed` (or `turn/completed`) with subtype
// "interrupted", and the bridge's translator converts that to
// EventAgentDone with Reason="settled". The app-server process
// stays alive — the bridge handle remains valid for the next
// Submit on the same codex thread.
//
// Stop is fire-and-forget: it does NOT block waiting for the
// app-server to confirm the turn has settled. The chat layer's
// TryFlush watches IsReady() and reschedules the next queued
// prompt automatically once the bridge sees KindPromptEnded.
//
// Returns ErrNotSupported if the bridge is not started yet.
func (d *driver) Stop(ctx context.Context) error {
	_ = ctx
	if d.session == nil || d.session.cmd == nil || d.session.cmd.Process == nil {
		return agent.ErrNotSupported
	}
	return agent.SignalProcessGroup(d.session.cmd.Process, os.Interrupt)
}

// SetModel is not supported on the codex bridge. codex reads the
// model at startup via `-c model=...` and the app-server API does
// not expose a model swap mechanism. Operators who want a
// different model must restart the bridge.
func (d *driver) SetModel(ctx context.Context, providerID, modelID string) error {
	_ = ctx
	_ = providerID
	_ = modelID
	return agent.ErrNotSupported
}

// ─── close ───

// Close terminates the session: closes stdin (so the child sees EOF
// and exits), waits up to closeDrainTimeout for graceful reap, then
// SIGKILLs if necessary. Idempotent.
func (d *driver) Close() error {
	var firstErr error
	d.closeOnce.Do(func() {
		close(d.closed)
		if d.session != nil {
			firstErr = d.session.Close()
		}
	})
	return firstErr
}

// ─── deliver ───

// deliver stamps the bridge-side session context onto every event
// before delivery, then blocks on pushing onto the events channel
// until either the runtime drains it OR the session is closed /
// lifecycle has reaped the child.
//
// Producer-side contract (matches pi / claudecode / pty / acp,
// promoted in commit 67b295ec):
//   - No `default:` instant-drop. The producer is allowed to block
//     until the consumer (runtime readpump) drains. The runtime's
//     own per-AS eventQueue absorbs natural bursts.
//   - No timeout drop. A `case <-time.After(1s)` branch was the
//     root cause of the F-54 "bridge reset: pi: new_session:
//     context deadline exceeded" incident when the runtime was
//     busy with another AS.
//   - Close signals release a parked deliver(). This prevents
//     leaked goroutines after the session is torn down.
//
// Translators / handlers call this with the event they want to emit;
// the public-facing signature has been kept tight so the call sites
// stay grep-friendly.
func (d *driver) deliver(ev agent.AgentEvent) agent.AgentEvent {
	if d.session == nil {
		return ev
	}
	ev.SessionID = d.session.threadID
	ev.Model = d.session.model
	ev.AgentName = d.session.agentName
	ev.Workspace = d.session.workspace
	ev.Branch = d.session.branch

	select {
	case d.session.events <- ev:
	case <-d.session.closed:
		cLog("deliver dropped (session closed)", "kind", ev.Kind.String())
	case <-d.session.exitDone:
		// lifecycle closed exitDone after cmd.Wait returned;
		// the bridge is being torn down. Drop silently — nobody
		// will read this anyway.
	}
	return ev
}
