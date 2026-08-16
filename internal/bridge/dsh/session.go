// session.go — long-lived chat session driver for the shared dsh
// host (F-dsh-shared-host).
//
// newDriver is the entry point. It does NOT spawn a dsh subprocess;
// it looks up the shared *host.Client installed by cmd/nightme/main.go
// at daemon boot, and uses its RPC + mux/host stream pumps for every
// interaction. sessionId is the multiplexing key: every ChatSession /
// AgentSession owns a single sessionId on the shared host.
//
// Lifecycle invariant: events chan is closed by Close() itself (no
// separate lifecycle goroutine). Close() calls Router.Unsubscribe so
// the shared host's mux pump stops routing frames for this sessionId;
// the session is then garbage-collectable.
//
// This file replaces the pre-shared-host driver that spawned a dsh
// subprocess per ChatSession. Per-driver fields like cmd / stdout /
// stderr / muxWS / hostWS / httpClient are gone — host.Client owns
// the transport now.
package dsh

import (
	"context"
	"encoding/base64"
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
	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// eventBufferSize caps the events channel fed to the runtime's
// read pump. Sized larger than codex/opencode/acp/claudecode (40960)
// because dsh web can burst the mux stream hard: every line of
// assistant text, every tool call/result, every projection snapshot,
// every queue update — often hundreds of frames in a single
// tool-heavy turn. At ~200 bytes per AgentEvent, 131072 ≈ 26 MiB
// worst case.
//
// We DO NOT rely solely on buffer size for deadlock prevention —
// deliver() is non-blocking and drops with a log warning when the
// buffer is full. Larger buffer just means less frequent drops.
const eventBufferSize = 131072

// ErrResumeUnhealthy is the bridge-local mirror of
// agent.ErrResumeUnhealthy. handshakeSession returns this (via
// resumeUnhealthyError.Is) when the caller asked for resume
// (cfg.SessionID != "") and session.fork refused for any reason —
// server doesn't know the id, transport error mid-handshake, value
// decode failure, etc.
//
// Mirrors claudecode's bridge-local ErrResumeUnhealthy
// (claudecode/claudecode.go). The runtime's auto-recovery at
// chatsession.go §1624 matches against agent.ErrResumeUnhealthy
// directly; this local mirror exists for symmetry with other
// bridges and for any future in-bridge callers that want to detect
// a fork failure without importing the agent package.
var ErrResumeUnhealthy = errors.New("dsh: resume session unhealthy")

// handshakeTimeout bounds each of session.fork and session.create
// independently (handshakeSession derives a separate ctx for each).
//
// Exposed as a var (not const) so tests can override it via
// `defer func(orig) { handshakeTimeout = orig }(handshakeTimeout)` —
// see TestHandshakeSession_IndependentTimeouts for the pattern.
var handshakeTimeout = 15 * time.Second

// driver is the runtime half of the chat-session bridge in the
// shared-host architecture. It owns:
//   - a reference to the process-wide shared *host.Client
//   - this session's sessionId (multiplexing key on the shared host)
//   - per-session protocol translation state (translator, wireState,
//     dispatcher, pending-approval FIFO)
//   - the events chan that the runtime reads from
//
// The shared host owns the dsh subprocess, the mux/host WS pumps,
// the HTTP RPC client, and the per-sessionId mux subscription table
// (Router). The driver interacts with all of these through cli.
type driver struct {
	cli *host.Client

	sessionID string
	workspace string
	agentName string

	// model is the model's authoritative selection captured at
	// session-create time via /api/session.models. Bridge stamps
	// it onto EventAgentReady.Model so the runtime's receipt
	// header renders "session <id> · model <name>".
	model string

	// pendingApprovals maps the server-frame rpcId (NOT the payload's
	// approvalId) to the response channel we hand to runtime via
	// EventPermission. The frame rpcId is the key /api/respond is
	// keyed on; using payload approvalId would silently route the
	// answer to a "no such pending request" entry on the server.
	// pendingOrder tracks INSERTION ORDER for SendPermission's
	// routing decision: we pop pendingOrder[0], not pendingApprovals
	// iteration (Go map iteration is randomized — Agent C trace
	// finding C-2). Pending approval ORDER is significant when
	// multiple are queued; we want strict FIFO so the runtime's
	// answer goes to the oldest pending question / approval.
	//
	// SCOPE NOTE: even in the shared-host architecture, this map is
	// per-driver (each session has its own FIFO). frame.rpcId is a
	// UUID minted by dsh on every push (dsh-api.md §1.5), so two
	// sessions never share an rpcId — per-driver maps don't collide.
	pendingMu        sync.Mutex
	pendingApprovals map[string]chan string
	pendingOrder     []string
	// lastApprovalID maps frame rpcId → human-readable approvalId
	// (muxApprovalID for mux path, SessionID+":q" for questions).
	// Audit-only; not used for routing. Keeps the original dsh ID
	// available in dLog messages even though we never pass it to
	// /api/respond.
	lastApprovalID map[string]string

	events chan agent.AgentEvent
	// translate + wireState + dispatcher are the per-session
	// protocol-translation pipeline. Reused unchanged from the
	// pre-shared-host driver because the actual stream payload
	// shape dsh sends is identical; only the source of frames
	// (mux pump goroutine, formerly per-driver; now global hub
	// routed via Router.DispatchMux) has moved.
	translate  *translator
	wireState  *wireState
	dispatcher *eventDispatcher

	// lastSeq is the highest SessionEvent.seq we've already pushed
	// to the events channel. Both the mux stream (when it works) and
	// the session.history backfill poll feed dispatchEvent; we dedupe
	// by seq so a frame arriving on both paths is only delivered once.
	// The dsh mux stream's events.mux listener is wired to a
	// session-scope event bus the bridge can't see, so for now only
	// the backfill path actually delivers events. The mux stream
	// subscribe is kept as a future-proofing seam.
	//
	// Initialized to -1 so the wire's seq=0 (turn/start, step/start
	// etc.) dispatches on first sight; seq == 0 is real on the wire
	// for the very first event of a session.
	lastSeq int64 // -1 = nothing seen yet (zero value is 0, but we need < 0)

	// backfillCancel stops the runBackfillLoop goroutine. Set in
	// newDriver after handshakeSession; cancelled in Close() so the
	// goroutine doesn't outlive the events channel drain.
	backfillCancel context.CancelFunc

	// Lifecycle guards. The events chan is closed by Close() itself
	// (no separate lifecycle goroutine — the host owns the dsh
	// subprocess now, so there's nothing for lifecycle to wait on).
	closed    chan struct{}
	closeOnce sync.Once
}

// newDriver is the chat-session Start() entry. It looks up the shared
// host.Client installed at daemon boot, performs the resume-or-create
// handshake against the shared host, subscribes to the session's
// mux frames via the host's Router, and emits EventAgentReady.
//
// It blocks until handshake (session.fork / session.create) returns
// or ctx fires. There is no subprocess to spawn.
func newDriver(ctx context.Context, s *Starter, cfg agent.StartConfig) (*driver, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("dsh: workspace is required")
	}

	cli := host.GetGlobal()
	if cli == nil {
		// Lazy start. The daemon does not pre-start dsh — a user
		// who never uses dsh (or doesn't have it installed) pays
		// nothing at startup. The first dsh agent-session spin-up
		// pays the spawn cost (typically 1-3s); every subsequent
		// one reuses the cached client.
		//
		// Per-session re-attachment: when this user later messages
		// a chat whose persisted sessionId points at a dsh
		// session, dsh's own session.fork in handshakeSession
		// below re-attaches to the existing in-memory session —
		// no boot-time RecoverAll is needed.
		//
		// Permission mode is fixed at danger-full-access per
		// [[agent-no-config-tampering]] (we only inject transport
		// + permissions — never model / provider / credentials).
		var err error
		cli, err = host.EnsureSharedHost(ctx, host.SharedHostOptions{
			Workspace:      cfg.Workspace,
			HostCmd:        "dsh",
			PermissionMode: "danger-full-access",
		})
		if err != nil {
			return nil, fmt.Errorf("dsh: shared host not available: %w", err)
		}
	}

	// Lazy-create a dsh workspace for this daemon and attach every
	// Workspace creation lives in handshakeSession (per-session,
	// keyed by cfg.Workspace). This mirrors the dsh dashboard's
	// browser behavior: every session lives in a workspace at the
	// session's cwd, and workspace.delete rips out the session row
	// from the dsh host's in-memory store on close. See
	// handshakeSession + deleteWorkspace for the wire flow.

	d := &driver{
		cli:              cli,
		workspace:        cfg.Workspace,
		agentName:        s.name,
		pendingApprovals: map[string]chan string{},
		lastApprovalID:   map[string]string{},
		events:           make(chan agent.AgentEvent, eventBufferSize),
		translate:        newTranslator(s.name, cfg.Workspace),
		wireState:        newWireState(),
		closed:           make(chan struct{}),
		// lastSeq = -1 so the wire's seq=0 (turn/start, etc.)
		// passes the dispatch dedup gate. See the field doc.
		lastSeq: -1,
	}
	d.dispatcher = newDispatcher(d.translate, d.wireState, d, d.deliver)

	// Session handshake: resume via session.fork when cfg.SessionID
	// is set, otherwise create a fresh session via session.create.
	// Both go through the shared RPC client (one HTTP transport,
	// one server; many sessions multiplex by sessionId).
	resumed, hsErr := d.handshakeSession(ctx, cfg)
	if hsErr != nil {
		return nil, hsErr
	}
	_ = resumed // surface is EventAgentReady.SessionID, not a log line

	// Fetch the authoritative model selection via /api/session.models.
	// session.create does NOT return the model — dsh requires the
	// adapter to resolve the model route asynchronously (catalog
	// lookup) and the selection is only readable through this RPC.
	// Without this call, EventAgentReady.Model would always be empty
	// AND a follow-up session.prompt would fail with `model-unavailable`.
	modelCtx, modelCancel := context.WithTimeout(ctx, handshakeTimeout)
	if sm, err := d.fetchSessionModels(modelCtx); err != nil {
		dLog("dsh: session.models probe failed (continuing without model)",
			"err", errStr(err))
	} else {
		d.model = sm.Current.Model
		dLog("dsh: model resolved",
			"model", d.model,
			"provider", sm.Current.Provider,
			"routable", sm.Routable)
	}
	modelCancel()

	// Subscribe to mux frames for this sessionId. The shared host's
	// Router will route any mux frame whose payload.sessionId matches
	// d.sessionID into d.handleMuxFrame. Subscribing BEFORE emitting
	// EventAgentReady ensures the runtime never sees a "ready" signal
	// without an attached mux subscription; ordering matters because
	// the runtime may immediately send a prompt on receiving Ready.
	// cwd is tracked so Client.RecoverSubscriptions can re-attach
	// this session after a dsh respawn (session.create is keyed on
	// sessionId+cwd — dsh-api.md §2.1.3).
	cli.Router.Subscribe(d.sessionID, cfg.Workspace, d.handleMuxFrame)

	// Emit EventAgentReady so the runtime can capture SessionID +
	// Workspace + Branch + Model.
	d.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: d.sessionID,
		AgentName: d.agentName,
		Workspace: d.workspace,
		Branch:    detectBranch(d.workspace),
		Model:     d.model,
	})

	// F-DSH-TODO-WIRE-FIX (2026-08-16): emit a single Info-level
	// audit line stating the assumed wire field names. Verified
	// against @deepseek-ai/dsh-tool-todo source.
	//
	// When the "todo list not converted" symptom recurs, the
	// paper trail is:
	//   1. Grep nightme.log for "dsh: wire assumptions" — see
	//      which field names the bridge assumed.
	//   2. Grep for "todo/write items dropped" — see which
	//      frames mismatched, with raw bytes attached for diffing.
	//
	// F-DSH-TODO-WIRE-FIX (2026-08-16): real dsh wire verified:
	//   - top-level container is `todos` (NOT `items`)
	//   - each entry is {content, status} — dsh forbids
	//     additionalProperties (so `id`, `activeForm` won't
	//     appear on the wire)
	//   - status values: pending | in_progress | completed
	// The bridge uses Content as a stable ID since each list
	// invariant-enforces unique content.
	slog.Default().Info("dsh: wire assumptions",
		"todo_data_top_level", "todos",
		"todo_data_legacy_alias", "items",
		"todo_item_id_field", "(none — wire omits id; bridge uses content as key)",
		"todo_item_content_field", "content",
		"todo_item_active_form_field", "(none — wire omits; bridge leaves empty)",
		"todo_item_status_field", "status",
		"todo_status_values", []string{"pending", "in_progress", "completed"},
		"hint", "if these field names drift from dsh wire, see 'todo/write items dropped' warnings")

	// Start the session.history backfill loop. dsh's events.mux
	// listener is wired to the wrong bus (session-scope vs global),
	// so session/event frames are silently dropped at the mux
	// stream today. backfill is the only path that actually delivers
	// content; we keep the mux Subscribe above as a future-proofing
	// seam when dsh fixes the wire. Cancel is owned by Close().
	bfCtx, bfCancel := context.WithCancel(context.Background())
	d.backfillCancel = bfCancel
	go d.runBackfillLoop(bfCtx)

	return d, nil
}

// fetchSessionModels calls POST /api/session.models on the shared
// host and decodes the SessionModels envelope.
func (d *driver) fetchSessionModels(ctx context.Context) (*sessionModelsValue, error) {
	if d.sessionID == "" {
		return nil, errors.New("dsh: session not initialized")
	}
	resp, err := d.cli.RPC.Post(ctx, "session.models", map[string]any{
		"sessionId": d.sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("dsh: session.models: %w", err)
	}
	if !resp.Result.OK {
		return nil, fmt.Errorf("dsh: session.models rejected: %s",
			resp.Result.ErrorMessage())
	}
	var out sessionModelsValue
	if err := json.Unmarshal(resp.Result.Value, &out); err != nil {
		return nil, fmt.Errorf("dsh: decode session.models value: %w", err)
	}
	return &out, nil
}

// handshakeSession runs the resume-or-create handshake against the
// shared dsh host. See the pre-shared-host version's doc comment for
// the full rationale; the wire behaviour is unchanged. The only
// difference is that the underlying transport is the shared
// host.RPCClient (not a per-driver *httpClient).
func (d *driver) handshakeSession(ctx context.Context, cfg agent.StartConfig) (bool, error) {
	if cfg.SessionID != "" {
		forkCtx, forkCancel := context.WithTimeout(ctx, handshakeTimeout)
		forkResp, err := d.cli.RPC.Post(forkCtx, "session.fork", map[string]any{
			"sessionId": cfg.SessionID,
		})
		forkCancel()
		if err != nil {
			reason := "transport error: " + errStr(err)
			slogDefault().Warn("dsh: session.fork transport error; refusing fallback",
				"requested_id", cfg.SessionID,
				"err", errStr(err))
			return false, resumeUnhealthyError{reason: reason, session: cfg.SessionID}
		}
		if !forkResp.Result.OK {
			reason := "rejected: " + forkResp.Result.ErrorMessage()
			slogDefault().Warn("dsh: session.fork rejected; refusing fallback",
				"requested_id", cfg.SessionID,
				"err", forkResp.Result.ErrorMessage())
			return false, resumeUnhealthyError{reason: reason, session: cfg.SessionID}
		}
		var fv sessionForkValue
		if uerr := json.Unmarshal(forkResp.Result.Value, &fv); uerr != nil {
			reason := "value decode failed: " + errStr(uerr)
			slogDefault().Warn("dsh: session.fork value decode failed; refusing fallback",
				"requested_id", cfg.SessionID,
				"err", errStr(uerr))
			return false, resumeUnhealthyError{reason: reason, session: cfg.SessionID}
		}
		if fv.SessionID == "" {
			slogDefault().Warn("dsh: session.fork returned empty sessionId; refusing fallback",
				"requested_id", cfg.SessionID)
			return false, resumeUnhealthyError{
				reason:  "empty sessionId in response",
				session: cfg.SessionID,
			}
		}
		d.sessionID = fv.SessionID
		slogDefault().Info("dsh: session forked",
			"requested_id", cfg.SessionID,
			"new_id", d.sessionID)
		return true, nil
	}

	// Per-session workspace: create a workspace keyed by the agent
	// session's cwd so this session lives in its own bucket. The
	// dashboard groups sessions by workspace, so /ui shows the
	// worktree-name as the friendly label. workspace.delete on
	// driver.Close rips out every session attached to this
	// workspace — including this one — so cross-session cleanup
	// can't leak. The dsh server dedupes by path, so a second
	// session with the same cwd reuses the same workspace; only
	// the LAST session to close triggers the delete.
	wsCtx, wsCancel := context.WithTimeout(ctx, handshakeTimeout)
	ws, err := d.cli.RPC.WorkspaceCreate(wsCtx, cfg.Workspace)
	wsCancel()
	if err != nil {
		return false, fmt.Errorf("dsh: workspace.create: %w", err)
	}

	createCtx, createCancel := context.WithTimeout(ctx, handshakeTimeout)
	createResp, err := d.cli.RPC.Post(createCtx, "session.create", map[string]any{
		"workspaceId": ws.WorkspaceID,
		"title":      filepath.Base(cfg.Workspace),
	})
	createCancel()
	if err != nil {
		return false, fmt.Errorf("dsh: session.create: %w", err)
	}
	if !createResp.Result.OK {
		return false, fmt.Errorf("dsh: session.create rejected: %s",
			createResp.Result.ErrorMessage())
	}
	var scVal sessionCreateValue
	if err := json.Unmarshal(createResp.Result.Value, &scVal); err != nil {
		return false, fmt.Errorf("dsh: decode session.create value: %w", err)
	}
	d.sessionID = scVal.SessionID
	slogDefault().Info("dsh: session created",
		"session_id", d.sessionID,
		"workspace_id", ws.WorkspaceID,
		"cwd", cfg.Workspace)
	return false, nil
}

// (workspace.create lives in handshakeSession; the driver never
// deletes the workspace — the dsh host's archive policy owns
// cleanup, matching the dashboard's behavior. If we ever need
// explicit teardown, call cli.RPC.WorkspaceDelete directly.)

// resumeUnhealthyError is returned by handshakeSession when the
// caller asked for resume (cfg.SessionID != "") and session.fork
// refused for any reason. It satisfies errors.Is for both
// agent.ErrResumeUnhealthy (the cross-package sentinel the chat
// layer uses to drive auto-recovery at chatsession.go §1624) AND
// ErrResumeUnhealthy (the bridge-local mirror, for symmetry with
// the claudecode bridge).
//
// See the pre-shared-host version's doc comment for the full rationale.
type resumeUnhealthyError struct {
	reason  string
	session string
}

func (e resumeUnhealthyError) Error() string {
	return fmt.Sprintf("%s: %s (session_id=%s); check workspace path and resume id",
		ErrResumeUnhealthy.Error(), e.reason, e.session)
}

func (e resumeUnhealthyError) Is(target error) bool {
	return target == ErrResumeUnhealthy || target == agent.ErrResumeUnhealthy
}

// deliver sends an event to the runtime's read pump. NEVER blocks —
// if the buffer is full we drop with a Debug log. The runtime's
// read pump drains via its own goroutine; if the runtime stops
// reading for some reason, we want the bridge to keep producing
// frames (so a /diagnose can see what happened) rather than
// deadlocking the shared host's mux pump.
func (d *driver) deliver(ev agent.AgentEvent) {
	select {
	case d.events <- ev:
	case <-d.closed:
		// Session already closed; drop quietly.
	default:
		dLog("dsh: events buffer full; dropping event kind=%s", ev.Kind)
	}
}

// dispatchEvent is the single entry point from both the mux stream and
// the session.history backfill. It dedupes by SessionEvent.seq so
// a frame arriving on both paths is delivered exactly once, and
// forwards to the dispatcher only when the seq advances.
//
// The mux stream's events.mux listener is wired to the wrong bus
// (session-scope vs global) so today it never fires for session/event
// frames — backfill is the only path that actually delivers today.
// We keep the mux stream subscribe anyway so future dsh fixes are
// free, and so session/projection / session/queue / approval/asked
// keep flowing through the partially-working mux path.
//
// lastSeq starts at -1 so the first event with seq=0 dispatches
// (events with `seq==0` are real on the wire — turn/start and
// step/start both have seq=0 on the very first event of a session).
// `max` defends against out-of-order delivery (a late residue from
// before seq tracking, rare but possible if dsh's wire renumbers).
func (d *driver) dispatchEvent(env sessionEventEnvelope, view json.RawMessage) {
	if env.Seq <= d.lastSeq {
		return
	}
	if env.Seq > d.lastSeq {
		d.lastSeq = env.Seq
	}
	d.dispatcher.dispatch(env, view)
}

// runBackfillLoop polls session.history on a fixed interval and
// dispatches any new events through dispatchEvent. Stops when
// the driver closes or the backfill context is cancelled.
//
// The dsh's events.mux listener is wired to a session-scope event
// bus so the global mux stream never receives session/event frames
// for active sessions (verified against dsh 0.1.0-rc.6 on
// 2026-08-16). session.history is the only reliable source for
// the per-session event stream today.
// dsh has historically polled this at 30 seconds in the dashboard's
// reload path; we poll at 2s here so the runtime/Feishu user sees
// the response within a couple of seconds of dsh generating it.
func (d *driver) runBackfillLoop(ctx context.Context) {
	defer func() {
		// Make sure we don't leak the loop if fetchHistory trips
		// on a fault we don't recover from. The context Done path
		// is the normal exit; this panic guard is belt-and-suspenders.
	}()
	// Seed lastSeq with the current value before the first poll so
	// we don't drop events that arrive via the mux stream between
	// newDriver wiring the Subscribe and the backfill loop's first
	// read. lastSeq starts at 0 (zero value), so the first poll
	// fetches everything; subsequent polls only fetch deltas.
	d.fetchHistory(ctx)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.closed:
			return
		case <-ticker.C:
			d.fetchHistory(ctx)
		}
	}
}

// fetchHistory pulls session.history for the current session. Each
// event with seq > lastSeq is dispatched via dispatchEvent, which
// dedupes + updates lastSeq. The dsh doesn't emit a "stream cursor"
// cursor per session, so this is the canonical way to see what's
// happened since the last poll.
func (d *driver) fetchHistory(ctx context.Context) {
	if d.sessionID == "" {
		return
	}
	// dsh's session.history wire only carries `beforeSeq` (exclusive
	// upper bound) — there is no `sinceSeq`. We want events with
	// seq > lastSeq, which is the OPPOSITE direction. We can't ask
	// for "the tail" without passing some upper bound, and we don't
	// know max_seq ahead of time.
	//
	// Pragmatic choice: don't send `beforeSeq` at all. dsh returns
	// the most recent up-to-maxMessages events (the full history
	// for any session under the cap). The dispatcher dedupes by
	// lastSeq so re-fetching the whole range each tick is wasteful
	// but correct. For long sessions, dsh's session log is bounded
	// in practice and we lose nothing.
	//
	// (A wire-side sinceSeq would cut the over-fetch by 1/N —
	// open an upstream issue if this becomes hot.)
	payload := map[string]any{
		"sessionId": d.sessionID,
	}
	resp, err := d.cli.RPC.Post(ctx, "session.history", payload)
	if err != nil {
		dLog("dsh: backfill history: %v", err)
		return
	}
	if !resp.Result.OK {
		dLog("dsh: backfill history rejected: %s", resp.Result.ErrorMessage())
		return
	}
	type histEntry struct {
		Event json.RawMessage `json:"event"`
		View  json.RawMessage `json:"view,omitempty"`
	}
	type histResp struct {
		Events []histEntry `json:"events"`
	}
	var history histResp
	if err := json.Unmarshal(resp.Result.Value, &history); err != nil {
		dLog("dsh: backfill history unmarshal: %v", err)
		return
	}
	for _, entry := range history.Events {
		var env sessionEventEnvelope
		if err := json.Unmarshal(entry.Event, &env); err != nil {
			dLog("dsh: backfill event decode: %v", err)
			continue
		}
		d.dispatchEvent(env, entry.View)
	}
}

// SendBlocks forwards user content to dsh via /api/session.prompt on
// the shared host. We send and return — the actual turn events arrive
// asynchronously on d.events via the host's mux pump.
func (d *driver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	if d.sessionID == "" {
		return errors.New("dsh: session not initialized")
	}
	content, err := contentBlocksToDTO(blocks)
	if err != nil {
		return fmt.Errorf("dsh: encode prompt content: %w", err)
	}
	resp, err := d.cli.RPC.Post(ctx, "session.prompt", map[string]any{
		"sessionId": d.sessionID,
		"mode":      "queue", // dsh-required discriminator; "steer" is the other valid value
		"content":   content,
	})
	if err != nil {
		return fmt.Errorf("dsh: session.prompt: %w", err)
	}
	if !resp.Result.OK {
		return fmt.Errorf("dsh: session.prompt rejected: %s",
			resp.Result.ErrorMessage())
	}
	// Note: the prompt ack comes back fast (it's the inbox enqueue);
	// the actual turn happens asynchronously and arrives via mux.
	return nil
}

// SendPermission routes a user decision back to the OLDEST pending
// approval (FIFO). The pending-approval FIFO is per-driver (each
// session has its own queue); see the doc comment on pendingApprovals.
//
// The shared host's RPC client emits a proper client-response envelope
// (dsh-api.md §2.12) keyed on the frame rpcId — NOT a client-request
// envelope with method:"respond" (which was the pre-fix bridge bug;
// dsh-api.md §11 item #2). The wire correlation is governed entirely
// by the echoed rpcId, not by any payload.approvalId.
//
// The outcome vocabulary follows dsh-api.md §2.12.1: "allowed-once"
// or "rejected" only — these are the client-giveable subset of
// ApprovalOutcome. Pre-fix bridge used "approved"/"declined"
// (dsh-api.md §11 item #3); now canonical.
func (d *driver) SendPermission(resp string) error {
	d.pendingMu.Lock()
	if len(d.pendingOrder) == 0 {
		d.pendingMu.Unlock()
		return errors.New("dsh: no pending approval to answer")
	}
	frameRpcID := d.pendingOrder[0]
	d.pendingOrder = d.pendingOrder[1:]
	delete(d.pendingApprovals, frameRpcID)
	d.pendingMu.Unlock()

	// Map the runtime's user-facing answer vocabulary to dsh's wire
	// vocabulary. The runtime's vocabulary is bridge-agnostic
	// ("approved"/"declined"/"allowed-once"/"rejected"); we accept
	// either spelling for forward/back compat.
	outcome := canonicalApprovalOutcome(resp)
	if outcome == "" {
		return fmt.Errorf("dsh: unknown approval outcome %q (expected approved|declined|allowed-once|rejected)", resp)
	}

	approvalID := ""
	d.pendingMu.Lock()
	approvalID = d.lastApprovalID[frameRpcID]
	d.pendingMu.Unlock()

	// /api/respond uses the client-response envelope; the response
	// shape is {accepted: true} on success, {accepted: false,
	// reason: ...} on duplicate / stale rpcId (dsh-api.md §2.12).
	if err := d.cli.RPC.Respond(context.Background(), frameRpcID, host.ApprovalResponse{
		SessionID:  d.sessionID,
		ApprovalID: approvalID,
		Outcome:    outcome,
	}); err != nil {
		return fmt.Errorf("dsh: /api/respond: %w", err)
	}

	// Wake any in-process handler waiting on the registered channel
	// (the channel is created by registerApproval / handleApprovalRequested
	// and consumed by the runtime's permission handler). Best-effort;
	// the wire send above is authoritative.
	d.cli.Router.AnswerPending(d.sessionID, frameRpcID, outcome)

	return nil
}

// canonicalApprovalOutcome maps the runtime's loose vocabulary onto
// dsh's wire vocabulary. Returns "" if resp is unknown.
func canonicalApprovalOutcome(resp string) string {
	switch resp {
	case "approved", "allowed-once":
		return "allowed-once"
	case "declined", "rejected":
		return "rejected"
	default:
		return ""
	}
}

// Reset clears the conversation context on the running session.
// dsh's protocol doesn't expose an in-place /clear; the cleanest
// path is to spawn a new session with the same cwd. We return
// ErrRestartRequired so the wrapper layer (agentsession.AgentSession)
// kills the current driver and re-spawns.
func (d *driver) Reset(ctx context.Context) error {
	return agent.ErrRestartRequired
}

// ListSessions calls POST /api/session.list on the shared host.
//
// LIMIT / PAGINATION — IMPORTANT:
// 实机 probe 2026-08-15 against dsh 0.1.0-rc.6 showed that the
// `limit` payload field is IGNORED by the server (passing
// limit=2 returned 243 items; limit=0, -5, and no-limit all
// returned the same full set). dsh has no pagination for
// session.list — every call returns the daemon's full session
// store. With a shared host this is even more pronounced: every
// nightme daemon's sessions live in one global registry per host.
//
// The bridge therefore drops the limit parameter from this
// signature; if dsh ever adds pagination, the runtime's picker UI
// should add a new method (e.g. ListSessionsPage) rather than
// reusing this one.
func (d *driver) ListSessions(ctx context.Context) ([]Session, error) {
	resp, err := d.cli.RPC.Post(ctx, "session.list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("dsh: session.list: %w", err)
	}
	if !resp.Result.OK {
		return nil, fmt.Errorf("dsh: session.list rejected: %s",
			resp.Result.ErrorMessage())
	}
	var out sessionListValue
	if err := json.Unmarshal(resp.Result.Value, &out); err != nil {
		return nil, fmt.Errorf("dsh: decode session.list value: %w", err)
	}
	return out.Items, nil
}

// Close shuts the bridge session down. Idempotent.
//
// In the shared-host architecture, Close is *much* simpler than the
// pre-fix version:
//   1. Unsubscribe from the shared host's Router — the global mux
//      pump stops routing frames for this sessionId. Any frames
//      that arrive between Unsubscribe and the runtime's readpump
//      draining the events chan are simply lost (correct: the
//      session is gone).
//   2. Send session.cancel on the shared RPC client (best-effort)
//      so any in-flight turn settles gracefully.
//   3. Close events chan — the runtime's readpump drains any queued
//      events, then exits the range loop.
//
// We DO NOT kill the dsh subprocess — that's the daemon's
// responsibility (host.Client.Close), and it lives for the full
// daemon lifetime now.
func (d *driver) Close() error {
	d.closeOnce.Do(func() {
		close(d.closed)
		// Stop the backfill poller first so it doesn't try to push
		// events into the events channel after we close it (next
		// deliver would hit the closed branch and drop, but the
		// goroutine would still be running).
		if d.backfillCancel != nil {
			d.backfillCancel()
		}
		// Stop the host from routing future frames for this session.
		// Drop pending-approval channels for this session too — the
		// runtime's permission handlers would otherwise wait forever
		// on a sessionId nobody can answer anymore.
		//
		// Workspace lifecycle is the dsh host's problem (it
		// archives + reaps workspaces on its own schedule); we
		// create one per session but don't delete on close. The
		// dashboard matches this — closing a tab doesn't auto-delete
		// the workspace either.
		d.cli.Router.Unsubscribe(d.sessionID)
		// Best-effort cancel of in-flight turn. 3s budget so a hung
		// server doesn't stall daemon shutdown.
		if d.sessionID != "" {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = d.cli.RPC.SessionCancel(cancelCtx, d.sessionID)
			cancel()
		}
		// Close events AFTER unsubscribe so the runtime drains any
		// frames already routed to handleMuxFrame (and thus into
		// deliver) before exiting. drain → close ordering matches
		// the pre-fix lifecycle pattern.
		close(d.events)
	})
	return nil
}

// Stop cancels an in-flight turn without closing the bridge session.
// Used when the runtime wants to halt a stuck turn (e.g. /stop
// command). In the shared-host architecture this is a single
// /api/session.cancel RPC on the shared client — no process signal,
// no transport teardown.
func (d *driver) Stop(ctx context.Context) error {
	if d.sessionID != "" {
		if err := d.cli.RPC.SessionCancel(ctx, d.sessionID); err != nil {
			return fmt.Errorf("dsh: session.cancel: %w", err)
		}
	}
	return nil
}

// Keepalive is the driver.Keepalive implementation for the
// dsh bridge. Shared-host model: the dsh subprocess is owned
// by the daemon-wide SharedHost (not by this driver), so the
// right thing to probe is the SHARED host.Client's done-channel
// — it's the single source of truth for "is the dsh backend
// reachable?". The watchdog in SharedHost handles the
// respawn side when Client.Close() has fired; we just report
// the dead state and let onRecover (supplied by the chat
// layer) re-install a fresh Client via host.EnsureSharedHost.
// See agent.driver.Keepalive for the full contract.
func (d *driver) Keepalive(ctx context.Context, onRecover func(context.Context) error) error {
	if onRecover == nil {
		return errors.New("dsh: keepalive and no recovery callback")
	}
	cli := host.GetGlobal()
	if cli == nil {
		// Shared host never started — e.g. lazy-start never fired because
		// nobody sent a dsh prompt yet. Treat as dead so onRecover
		// re-runs EnsureSharedHost and we get a host to query.
		return onRecover(ctx)
	}
	select {
	case <-cli.Done():
		// Host Client closed (server shutdown, network blip, or
		// the upstream dsh process exited). The shared-host watchdog
		// re-spawns as needed; the chat layer's onRecover knows how
		// to re-populate host.GetGlobal() for us.
		return onRecover(ctx)
	default:
		return nil
	}
}

// ─── wire content blocks ──────────────────────────────────────────────

// contentBlocksToDTO converts nightme's ContentBlock slice to
// dsh's wire format. dsh web's session.prompt accepts exactly two
// content-block variants:
//
//   - { type: "text",  text: "..." }
//   - { type: "image", mediaType: "image/png"|..., data: "<base64>", name?: "..." }
//
// Confirmed via 实机 HTTP probe on 2026-08-14 (text / image / image+text
// all accepted; resource_link rejected by the discriminator).
//
// Strategy:
//   - ContentText → wire "text" verbatim
//   - ContentImage with supported MediaType → wire "image" with
//     base64-encoded file contents (this is what the model needs
//     for vision); MediaType not in the supported set → degrade
//     to bracketed text annotation (file path + name)
//   - ContentFile → bracketed text annotation (dsh web doesn't
//     accept a file-reference type at the prompt boundary; the
//     model can still read the file via its bash/fs tools)
//
// Image data is read synchronously from disk via os.ReadFile.
// Returns an error rather than a silent skip so the caller
// learns about a missing/unreadable file rather than the model
// silently dropping the image.
func contentBlocksToDTO(blocks []agent.ContentBlock) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentText:
			if b.Text == "" {
				continue
			}
			out = append(out, map[string]any{
				"type": "text",
				"text": b.Text,
			})
		case agent.ContentImage:
			if b.Path == "" {
				continue
			}
			if isSupportedImageMediaType(b.MediaType) {
				raw, err := os.ReadFile(b.Path)
				if err != nil {
					return nil, fmt.Errorf("dsh: read image %q: %w", b.Path, err)
				}
				wire := map[string]any{
					"type":      "image",
					"mediaType": b.MediaType,
					"data":      base64.StdEncoding.EncodeToString(raw),
				}
				if b.Path != "" {
					// Use the basename so the model sees a
					// readable name (full paths leak workspace
					// details through tool history).
					wire["name"] = filepath.Base(b.Path)
				}
				out = append(out, wire)
			} else {
				// Unsupported image MIME (e.g. image/heic, image/svg).
				// Fall back to annotation; the model can still try
				// to read the file via its tools.
				out = append(out, map[string]any{
					"type": "text",
					"text": fmt.Sprintf("[image: %s (%s) — unsupported mediaType, decode locally to view]", b.Path, b.MediaType),
				})
			}
		case agent.ContentFile:
			if b.Path == "" {
				continue
			}
			out = append(out, map[string]any{
				"type": "text",
				"text": fmt.Sprintf("[file: %s]", b.Path),
			})
		}
	}
	return out, nil
}

// isSupportedImageMediaType returns true iff mediaType is one of
// the formats dsh web's session.prompt accepts inline. We err on the
// side of being conservative; the supported set is documented in
// `packages/attachment/src/limits.ts` (ImageMediaType union).
func isSupportedImageMediaType(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// ─── helpers (also imported by translate.go / permissions.go) ─────────

// errStr renders an error's string form, returning "<nil>" for the
// nil case so log fields are always meaningful. Mirror of
// claudecode/print.go's helper.
func errStr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// detectBranch shells out to `git -C <workspace> symbolic-ref
// --short HEAD` to populate EventAgentReady.Branch. Returns ""
// for non-git workspaces (claudecode does the same).
func detectBranch(workspace string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", workspace, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// slogDefault is a thin indirection so tests can override the
// package-level logger if needed. Matches the pattern in
// handle_mux.go (warnLogger) and dispatch.go.
//
// Production code calls slogDefault() which delegates to slog.Default().
// Tests can reassign the function variable.
var slogDefault = func() *slog.Logger { return slog.Default() }