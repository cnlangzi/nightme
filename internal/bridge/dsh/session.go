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
	"runtime/debug"
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
// (cfg.SessionID != "") and session.create attach failed —
// session-conflict, transport error, mismatched returned id, etc.
//
// Mirrors claudecode's bridge-local ErrResumeUnhealthy
// (claudecode/claudecode.go). The runtime's auto-recovery at
// chatsession.go §1624 matches against agent.ErrResumeUnhealthy
// directly; this local mirror exists for symmetry with other
// bridges and for any future in-bridge callers that want to detect
// a fork failure without importing the agent package.
var ErrResumeUnhealthy = errors.New("dsh: resume session unhealthy")

// handshakeTimeout bounds session.create attach / workspace.create
// / session.create in handshakeSession. Each RPC derives its own
// timeout from this value independently.
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
	// pendingQuestions maps frame rpcId → the AskUserQuestionItem
	// batch from question/requested. Present only for question
	// frames; SendPermission uses it to emit QuestionResponse
	// instead of ApprovalResponse. lastApprovalID maps frame rpcId
	// → human-readable approvalId (muxApprovalID for mux path,
	// SessionID+":q" for questions). Audit-only; not used for
	// routing. Keeps the original dsh ID available in dLog
	// messages even though we never pass it to /api/respond.
	pendingQuestions map[string][]questionPayload
	lastApprovalID   map[string]string

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
	// to the events channel. Mux session/event is the live path
	// (dashboard "select a session" semantics); session.history
	// backfill only fills gaps the mux pump missed. Both feed
	// dispatchEvent and dedupe by seq so a frame arriving on both
	// paths is delivered once.
	//
	// seqMu guards lastSeq: the mux pump and the backfill goroutine
	// both write it.
	//
	// Initialized to -1 so the wire's seq=0 (turn/start, step/start
	// etc.) dispatches on first sight; seq == 0 is real on the wire
	// for the very first event of a session. After attach, seedLastSeq
	// advances this to the server's current cursor so we don't
	// re-emit historical events as new Feishu bubbles.
	seqMu   sync.Mutex
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
// It blocks until handshake (resume-attach or session.create) returns
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
		// session, dsh's own resume-attach in handshakeSession
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
		pendingQuestions: map[string][]questionPayload{},
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

	// Session handshake: resume is dashboard "click a session in
	// the left list" — POST session.create({sessionId, cwd})
	// re-attaches the existing agent (dsh-api.md §2.1.3 /
	// dsh-shared-host.md §2.6). Empty SessionID creates a fresh
	// session. Both go through the shared RPC client.
	resumed, hsErr := d.handshakeSession(ctx, cfg)
	if hsErr != nil {
		return nil, hsErr
	}
	_ = resumed // surface is EventAgentReady.SessionID, not a log line

	// Seed lastSeq from session.history BEFORE subscribing so
	// resume does not replay the whole log as new Feishu bubbles.
	// Mux is the live path from here; backfill only fills gaps.
	d.seedLastSeq(ctx)

	// Subscribe immediately after attach/create. Router.DispatchMux
	// drops frames for unsubscribed sessionIds, and session.create
	// attach is what makes dsh push live session/event on the
	// already-open mux (dashboard select semantics). cwd is tracked
	// so Client.RecoverSubscriptions can re-attach after a dsh
	// respawn (session.create is keyed on sessionId+cwd).
	cli.Router.Subscribe(d.sessionID, cfg.Workspace, d.handleMuxFrame)

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

	d.ensureFullAccess(ctx)

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

	// Gap-fill via session.history. Mux session/event is the live
	// path; this loop only dispatches seq > lastSeq so a missed
	// mux frame still lands. Cancel is owned by Close().
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
// shared dsh host.
//
// Resume (cfg.SessionID != "") is dashboard "select a session in the
// left list": POST /api/session.create {sessionId, cwd}. Same id+cwd
// returns the same session and attaches this client so mux starts
// pushing live session/event. session-conflict / transport / a
// different returned id → resumeUnhealthyError (runtime clears the
// stale id and retries fresh). We do NOT fork — session.fork mints a
// child and abandons the parent (F-DSH-NO-FORK).
//
// Fresh start (cfg.SessionID == "") creates a workspace keyed by cwd
// then session.create {workspaceId, title}.
func (d *driver) handshakeSession(ctx context.Context, cfg agent.StartConfig) (bool, error) {
	if cfg.SessionID != "" {
		if err := d.attachSession(ctx, cfg.SessionID, cfg.Workspace); err != nil {
			return false, err
		}
		return true, nil
	}

	sid, err := d.createFreshSession(ctx, cfg.Workspace)
	if err != nil {
		return false, err
	}
	d.sessionID = sid
	return false, nil
}

// attachSession re-attaches an existing dsh session the way the
// dashboard does: session.create({sessionId, cwd}). Same id+cwd is
// a no-op create that returns the original sessionId and joins the
// mux live set (dsh-shared-host.md §2.6).
func (d *driver) attachSession(ctx context.Context, sessionID, cwd string) error {
	createCtx, createCancel := context.WithTimeout(ctx, handshakeTimeout)
	defer createCancel()
	got, err := d.cli.RPC.SessionCreate(createCtx, host.SessionCreateOpts{
		SessionID: sessionID,
		CWD:       cwd,
	})
	if err != nil {
		return resumeUnhealthyError{reason: err.Error(), session: sessionID}
	}
	if got != sessionID {
		return resumeUnhealthyError{
			reason:  fmt.Sprintf("session.create returned %q, want attach of %q", got, sessionID),
			session: sessionID,
		}
	}
	d.sessionID = got
	slogDefault().Info("dsh: session attached",
		"session_id", d.sessionID,
		"cwd", cwd)
	return nil
}

// createFreshSession allocates a new dsh session in a workspace
// keyed by cwd. Does not mutate d.sessionID — callers assign on
// success so Reset can create the replacement before dropping the
// old subscription.
func (d *driver) createFreshSession(ctx context.Context, workspace string) (string, error) {
	wsCtx, wsCancel := context.WithTimeout(ctx, handshakeTimeout)
	ws, err := d.cli.RPC.WorkspaceCreate(wsCtx, workspace)
	wsCancel()
	if err != nil {
		return "", fmt.Errorf("dsh: workspace.create: %w", err)
	}

	createCtx, createCancel := context.WithTimeout(ctx, handshakeTimeout)
	createResp, err := d.cli.RPC.Post(createCtx, "session.create", map[string]any{
		"workspaceId": ws.WorkspaceID,
		"title":       filepath.Base(workspace),
	})
	createCancel()
	if err != nil {
		return "", fmt.Errorf("dsh: session.create: %w", err)
	}
	if !createResp.Result.OK {
		return "", fmt.Errorf("dsh: session.create rejected: %s",
			createResp.Result.ErrorMessage())
	}
	var scVal sessionCreateValue
	if err := json.Unmarshal(createResp.Result.Value, &scVal); err != nil {
		return "", fmt.Errorf("dsh: decode session.create value: %w", err)
	}
	if scVal.SessionID == "" {
		return "", errors.New("dsh: session.create: empty sessionId in response")
	}
	slogDefault().Info("dsh: session created",
		"session_id", scVal.SessionID,
		"workspace_id", ws.WorkspaceID,
		"cwd", workspace)
	return scVal.SessionID, nil
}

// (workspace.create lives in handshakeSession; the driver never
// deletes the workspace — the dsh host's archive policy owns
// cleanup, matching the dashboard's behavior. If we ever need
// explicit teardown, call cli.RPC.WorkspaceDelete directly.)

// resumeUnhealthyError is returned by handshakeSession when the
// caller asked for resume (cfg.SessionID != "") and session.create
// attach refused (session-conflict, transport, mismatched id).
// It satisfies errors.Is for both agent.ErrResumeUnhealthy (the
// cross-package sentinel the chat layer uses to drive auto-recovery
// at chatsession.go §1624) AND ErrResumeUnhealthy (the bridge-local
// mirror, for symmetry with the claudecode bridge).
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

// dispatchEvent is the single entry point from both the mux stream
// (live path) and the session.history backfill (gap fill). It dedupes
// by SessionEvent.seq so a frame arriving on both paths is delivered
// exactly once, and forwards to the dispatcher only when the seq
// advances.
//
// lastSeq starts at -1 so seq=0 (turn/start, step/start) dispatches.
// `<=` drops duplicates and out-of-order frames.
func (d *driver) dispatchEvent(env sessionEventEnvelope, view json.RawMessage) {
	d.seqMu.Lock()
	if env.Seq <= d.lastSeq {
		d.seqMu.Unlock()
		return
	}
	d.lastSeq = env.Seq
	d.seqMu.Unlock()
	d.dispatcher.dispatch(env, view)
}

func (d *driver) bumpLastSeq(seq int64) {
	d.seqMu.Lock()
	if seq > d.lastSeq {
		d.lastSeq = seq
	}
	d.seqMu.Unlock()
}

func (d *driver) peekLastSeq() int64 {
	d.seqMu.Lock()
	defer d.seqMu.Unlock()
	return d.lastSeq
}

func (d *driver) resetLastSeq() {
	d.seqMu.Lock()
	d.lastSeq = -1
	d.seqMu.Unlock()
}

// runBackfillLoop polls session.history on a fixed interval and
// dispatches any new events through dispatchEvent. Stops when
// the driver closes or the backfill context is cancelled.
//
// Mux session/event is the live path (dashboard select). This loop
// is gap-fill only: seedLastSeq already advanced lastSeq to the
// attach cursor, so we only dispatch seq the mux pump missed.
func (d *driver) runBackfillLoop(ctx context.Context) {
	defer func() {
		// Panic recover: any handler panic in fetchHistory would
		// otherwise silently kill this goroutine and the bridge
		// would stop receiving events forever (verified in the
		// 9a3bad91 session where the loop died silently after
		// events=34, last_seq=10). Log + restart the loop in a
		// tight retry cycle so a bad event in one tick doesn't
		// permanently break the bridge.
		//
		// Bounds: 1s cooldown between recoveries so a tight
		// panic-loop doesn't burn CPU; ctx.Done / d.closed still
		// bail us out cleanly.
		if r := recover(); r != nil {
			slogDefault().Error("dsh: backfill loop panic recovered",
				"session_id", d.sessionID,
				"panic", fmt.Sprintf("%v", r),
				"stack", string(debug.Stack()))
			// Re-launch the loop with the SAME context. If ctx
			// is cancelled (session close), this call returns
			// immediately.
			go func() {
				time.Sleep(1 * time.Second)
				d.runBackfillLoop(ctx)
			}()
		}
	}()
	slogDefault().Info("dsh: backfill loop start",
		"session_id", d.sessionID)
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

// fetchHistory pulls session.history and dispatches seq > lastSeq.
func (d *driver) fetchHistory(ctx context.Context) {
	d.observeHistory(ctx, true)
}

// seedLastSeq advances lastSeq to the server's current cursor
// without dispatching. Used after attach/create so resume does not
// replay the whole log as new Feishu bubbles; mux + later backfill
// only deliver events after this cursor.
func (d *driver) seedLastSeq(ctx context.Context) {
	d.observeHistory(ctx, false)
}

func (d *driver) observeHistory(ctx context.Context, dispatch bool) {
	if d.sessionID == "" {
		return
	}
	// dsh's session.history wire only carries `beforeSeq` (exclusive
	// upper bound) — there is no `sinceSeq`. Don't send beforeSeq;
	// dsh returns the most recent page. Dedup is by lastSeq.
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
	slogDefault().Info("dsh: history observed",
		"session_id", d.sessionID,
		"events", len(history.Events),
		"last_seq", d.peekLastSeq(),
		"dispatch", dispatch)
	for _, entry := range history.Events {
		var env sessionEventEnvelope
		if err := json.Unmarshal(entry.Event, &env); err != nil {
			dLog("dsh: backfill event decode: %v", err)
			continue
		}
		if dispatch {
			d.dispatchEvent(env, entry.View)
			continue
		}
		d.bumpLastSeq(env.Seq)
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
// approval or question (FIFO). The pending FIFO is per-driver (each
// session has its own queue); see the doc comment on pendingApprovals.
//
// The shared host's RPC client emits a proper client-response envelope
// (dsh-api.md §2.12) keyed on the frame rpcId — NOT a client-request
// envelope with method:"respond" (which was the pre-fix bridge bug;
// dsh-api.md §11 item #2). The wire correlation is governed entirely
// by the echoed rpcId, not by any payload.approvalId.
//
// Two value shapes share this method:
//   - question/requested → QuestionResponse (dsh-api.md §2.12.2)
//   - approval/requested → ApprovalResponse with outcome
//     "allowed-once" | "rejected" (dsh-api.md §2.12.1)
func (d *driver) SendPermission(resp string) error {
	d.pendingMu.Lock()
	if len(d.pendingOrder) == 0 {
		d.pendingMu.Unlock()
		return errors.New("dsh: no pending approval to answer")
	}
	frameRpcID := d.pendingOrder[0]
	questions, isQuestion := d.pendingQuestions[frameRpcID]
	approvalID := d.lastApprovalID[frameRpcID]

	var value any
	outcome := resp
	if isQuestion {
		answer, err := questionAnswerFor(questions, resp)
		if err != nil {
			d.pendingMu.Unlock()
			return err
		}
		value = host.QuestionResponse{
			SessionID: d.sessionID,
			Answer:    answer,
		}
	} else {
		outcome = canonicalApprovalOutcome(resp)
		if outcome == "" {
			d.pendingMu.Unlock()
			return fmt.Errorf("dsh: unknown approval outcome %q (expected approved|declined|allowed-once|rejected)", resp)
		}
		value = host.ApprovalResponse{
			SessionID:  d.sessionID,
			ApprovalID: approvalID,
			Outcome:    outcome,
		}
	}

	d.pendingOrder = d.pendingOrder[1:]
	ch := d.pendingApprovals[frameRpcID]
	delete(d.pendingApprovals, frameRpcID)
	delete(d.pendingQuestions, frameRpcID)
	d.pendingMu.Unlock()

	if ch != nil {
		select {
		case ch <- resp:
		default:
		}
	}

	// /api/respond uses the client-response envelope; the response
	// shape is {accepted: true} on success, {accepted: false,
	// reason: ...} on duplicate / stale rpcId (dsh-api.md §2.12).
	if err := d.cli.RPC.Respond(context.Background(), frameRpcID, value); err != nil {
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
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case "approved", "allowed-once", "approve", "allow once", "allowed":
		return "allowed-once"
	case "declined", "rejected", "decline", "reject":
		return "rejected"
	default:
		return ""
	}
}

// fullAccessPreset is the dsh permission-presets table key that
// bundles sandbox danger-full-access + approval never. Dashboard
// shows it as "Full access". New sessions otherwise pin
// workspace-write (ask) via pinInitialPermission, which is why
// git writes outside the workspace root keep prompting.
const fullAccessPreset = "danger-full-access"

// ensureFullAccess switches the attached session to Full access
// the same way the dashboard chip does: slash command
// `/permission danger-full-access` via session.prompt. Host
// intercepts leading `/` without starting a model turn. apply()
// is a no-op when the session is already on that preset.
//
// We do not persist settings.defaultPreset (that would rewrite
// ~/.dsh); per-session switch is the allowed permissions injection.
func (d *driver) ensureFullAccess(ctx context.Context) {
	if d == nil || d.cli == nil || d.sessionID == "" {
		return
	}
	promptCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	err := d.cli.RPC.SessionPrompt(promptCtx, d.sessionID, "queue", []host.PromptPart{{
		Type: "text",
		Text: "/permission " + fullAccessPreset,
	}})
	if err != nil {
		dLog("dsh: /permission danger-full-access failed", "err", errStr(err))
		return
	}
	slogDefault().Info("dsh: session permission preset",
		"session_id", d.sessionID,
		"preset", fullAccessPreset)
}

// Reset starts a fresh dsh conversation on this same driver (dashboard
// "new session"), without returning ErrRestartRequired.
//
// The previous /new path returned ErrRestartRequired, which made the
// chat layer Close+respawn. That raced a second handshake: one
// session.create from the restart spawn, another from a concurrent
// Spawn seeing StatusExited, leaving Feishu bound to the empty first
// id while prompts landed on the second. In-place create keeps one
// driver, one mux subscription, one sessionId after /new.
func (d *driver) Reset(ctx context.Context) error {
	select {
	case <-d.closed:
		return errors.New("dsh: session closed")
	default:
	}
	if d.cli == nil {
		return errors.New("dsh: no host client")
	}

	oldID := d.sessionID
	newID, err := d.createFreshSession(ctx, d.workspace)
	if err != nil {
		return err
	}

	if oldID != "" && oldID != newID {
		d.cli.Router.Unsubscribe(oldID)
		cancelCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = d.cli.RPC.SessionCancel(cancelCtx, oldID)
		cancel()
	}

	d.sessionID = newID
	d.resetLastSeq()
	d.translate = newTranslator(d.agentName, d.workspace)
	d.wireState = newWireState()
	d.dispatcher = newDispatcher(d.translate, d.wireState, d, d.deliver)
	d.pendingMu.Lock()
	d.pendingApprovals = map[string]chan string{}
	d.pendingOrder = nil
	d.pendingQuestions = map[string][]questionPayload{}
	d.lastApprovalID = map[string]string{}
	d.pendingMu.Unlock()

	d.cli.Router.Subscribe(newID, d.workspace, d.handleMuxFrame)

	modelCtx, modelCancel := context.WithTimeout(ctx, handshakeTimeout)
	if sm, err := d.fetchSessionModels(modelCtx); err != nil {
		dLog("dsh: session.models probe failed after reset (continuing without model)",
			"err", errStr(err))
	} else {
		d.model = sm.Current.Model
	}
	modelCancel()

	d.ensureFullAccess(ctx)

	d.seedLastSeq(ctx)

	d.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: d.sessionID,
		AgentName: d.agentName,
		Workspace: d.workspace,
		Branch:    detectBranch(d.workspace),
		Model:     d.model,
	})
	return nil
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
// This is the dashboard session-row context menu "Archive Session":
//  1. Unsubscribe from the shared host's Router — the global mux
//     pump stops routing frames for this sessionId.
//  2. session.cancel so any in-flight turn settles (same RPC as
//     the InputBar stop button; benign if already idle).
//  3. workspace.archiveSession — drops the row from every grouping
//     surface (left list) while keeping the session log. There is
//     no session.delete on the wire (dsh-api.md §2: sessions.* has
//     no delete; archive lives under workspace.*).
//  4. Close events chan — the runtime's readpump drains then exits.
//
// We DO NOT kill the dsh subprocess — that's the daemon's
// responsibility (host.Client.Close), and it lives for the full
// daemon lifetime now. We also do not workspace.delete: archive
// keeps the cwd workspace so other sessions in it stay grouped.
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
		d.cli.Router.Unsubscribe(d.sessionID)
		if d.sessionID != "" {
			rpcCtx, rpcCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := d.cli.RPC.SessionCancel(rpcCtx, d.sessionID); err != nil && !isBenignCancelErr(err) {
				dLog("dsh: session.cancel on close: %v", err)
			}
			if err := d.cli.RPC.WorkspaceArchiveSession(rpcCtx, d.sessionID); err != nil {
				if isBenignCancelErr(err) {
					dLog("dsh: workspace.archiveSession already archived",
						"session_id", d.sessionID, "err", errStr(err))
				} else {
					dLog("dsh: workspace.archiveSession on close: %v", err)
				}
			} else {
				slogDefault().Info("dsh: session archived", "session_id", d.sessionID)
			}
			rpcCancel()
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
// This is the dashboard InputBar stop button: POST /api/session.cancel
// {sessionId} (dsh-api.md §2.1.12 / client-ui-conversation InputBar.stop).
// The dsh process and this sessionId stay alive; mux then emits
// turn/end{stopReason:"abort"}.
//
// Fire-and-forget: returns once the RPC is accepted (or already
// settled). Does not wait for turn/end. Dashboard swallows cancel
// failures with .catch(()=>{}); we treat session-not-found the same
// (idle / already settled) so a second /stop is a no-op, not "failed".
func (d *driver) Stop(ctx context.Context) error {
	if d.sessionID == "" {
		return agent.ErrNotSupported
	}
	stopCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := d.cli.RPC.SessionCancel(stopCtx, d.sessionID); err != nil {
		if isBenignCancelErr(err) {
			dLog("dsh: session.cancel already settled",
				"session_id", d.sessionID, "err", errStr(err))
			return nil
		}
		return fmt.Errorf("dsh: session.cancel: %w", err)
	}
	slogDefault().Info("dsh: session cancelled", "session_id", d.sessionID)
	return nil
}

// isBenignCancelErr is true when session.cancel failed because the
// turn is already gone — the dashboard stop button's .catch path.
func isBenignCancelErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "session-not-found")
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
