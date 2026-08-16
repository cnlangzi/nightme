// session.go — long-lived chat session driver for `dsh --profile web`.
//
// newDriver is the entry point: it spawns dsh web, parses the bound
// URL from stdout, dials the two WebSocket downlinks, performs
// session.create, and emits EventAgentReady. It returns a *driver
// that satisfies the agent.driver interface (SendBlocks /
// SendPermission / Reset / Close / Stop).
//
// Lifecycle invariant: `close(events)` happens ONLY in the lifecycle
// goroutine. Close() closes WS + stdin + cancels, then waits for
// lifecycle to drain. Mirror the closeOnce pattern from codex to
// avoid the deadlock where Close and lifecycle both try to close
// events.
package dsh

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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/cnlangzi/nightme/internal/agent"
)

// eventBufferSize caps the events channel fed to the runtime's
// read pump. Sized larger than codex/opencode/acp/claudecode
// (all 40960) because dsh web can burst the mux stream hard:
// every line of assistant text, every tool call/result, every
// projection snapshot, every queue update — often hundreds of
// frames in a single tool-heavy turn. At ~200 bytes per
// AgentEvent, 131072 ≈ 26 MiB worst case, plenty of headroom for
// transient consumer lag (the runtime pumps events into the
// gateway's translate path which does I/O).
//
// We DO NOT rely solely on buffer size for deadlock prevention —
// deliver() is non-blocking and drops with a log warning when the
// buffer is full (see deliver). Larger buffer just means less
// frequent drops.
const eventBufferSize = 131072

// ErrResumeUnhealthy is the bridge-local mirror of
// agent.ErrResumeUnhealthy. handshakeSession returns this (via
// resumeUnhealthyError.Is) when the caller asked for resume
// (cfg.SessionID != "") and session.fork refused for any reason
// — server doesn't know the id, transport error mid-handshake,
// value decode failure, etc.
//
// Mirrors claudecode's bridge-local ErrResumeUnhealthy
// (claudecode/claudecode.go). The runtime's auto-recovery at
// chatsession.go §1624 matches against agent.ErrResumeUnhealthy
// directly; this local mirror exists for symmetry with other
// bridges and for any future in-bridge callers that want to
// detect a fork failure without importing the agent package.
var ErrResumeUnhealthy = errors.New("dsh: resume session unhealthy")

// handshakeTimeout bounds each of session.fork and session.create
// independently (handshakeSession derives a separate ctx for each).
// dsh web is already accepting HTTP by the time we dial WS, but the
// first session.create round-trips through Cordis plugin init, and
// session.fork can be slow if the parent history is large, so give
// each call generous slack.
//
// Exposed as a var (not const) so tests can override it via
// `defer func(orig) { handshakeTimeout = orig }(handshakeTimeout)`
// — see TestHandshakeSession_IndependentTimeouts for the pattern.
var handshakeTimeout = 15 * time.Second

// webURLParseTimeout bounds waiting for dsh web to print its bound
// URL on stdout. Real-machine cold start is ~1.5s; 10s is generous.
const webURLParseTimeout = 10 * time.Second

// Note: there is no lifecycle watchdog. dsh can run long tasks
// (15-30 min multi-step tool chains) and a wall-clock-from-spawn
// SIGKILL at any fixed threshold (5 min, 30 min, etc) will
// always kill some legitimate in-flight work. cmd.Wait() blocks
// until dsh actually exits — if dsh is genuinely deadlocked we
// rely on the OS / external monitor to detect and kill it. The
// runtime's "should this bridge still be alive" judgement lives
// at the chat-session layer (HungPrompt watchdog, /use switch,
// daemon restart), not at the bridge layer.

// dshURLPattern matches the first line of `dsh --profile web` stdout:
// "dsh web: http://127.0.0.1:3080". Captures host + port.
var dshURLPattern = regexp.MustCompile(`dsh web:\s+http://([^:\s]+):(\d+)`)

// driver is the runtime half of the chat-session bridge. It owns
// the spawned dsh process, the two WS downlinks, the pending-approval
// map, and the events chan that the runtime reads from.
type driver struct {
	cmd         *exec.Cmd
	stdout      io.ReadCloser
	stderr      io.ReadCloser
	muxWS       *websocket.Conn
	hostWS      *websocket.Conn
	http        *httpClient
	sessionID   string
	workspace   string
	agentName   string

	// stderrTail keeps the LAST stderrTailBytes of dsh web's stderr
	// for diagnostic capture on non-graceful exit. Always populated
	// by drainStderr (no Debug gating) so a `/diagnose` request can
	// surface "what did dsh say right before it died" without us
	// having to reproduce the failure. NOT thread-safe on its own;
	// drainStderr writes, lifecycle reads at exit.
	stderrTail *agent.StderrRingBuffer

	// model is the model's authoritative selection captured at
	// session-create time via /api/session.models. Bridge stamps
	// it onto EventAgentReady.Model so the runtime's receipt
	// header renders "session <id> · model <name>". A model
	// change requires restarting the session so a fresh probe
	// picks up the new default from ~/.dsh/settings.yaml.
	//
	// Format: provider-owned model id (e.g. "MiniMax-M3"). The
	// provider prefix is NOT included — runtime footer compares
	// against per-model context-window tables, and the
	// provider:model composite is too wide for that key.
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
	pendingMu       sync.Mutex
	pendingApprovals map[string]chan string
	pendingOrder    []string
	// lastApprovalID maps frame rpcId → human-readable approvalId
	// (muxApprovalID for mux path, SessionID+":q" for questions).
	// Audit-only; not used for routing. Keeps the original dsh ID
	// available in dLog messages even though we never pass it to
	// /api/respond.
	lastApprovalID  map[string]string

	events    chan agent.AgentEvent
	translate *translator

	// F-DSH-CHAT-001: wireState + dispatcher pair.
	// wireState holds the dsh-bridge-internal normalized truth
	// (tasks by ID, tools by CallID, inflight, title). It is fed
	// by both raw session/event (via dispatcher → handlers) and
	// session/projection (via handle_mux.go → applyProjection).
	// dispatcher routes session/event envelopes by Type through a
	// registration-driven handler map (replaces the prior
	// switch env.Type in translate.go).
	wireState  *wireState
	dispatcher *eventDispatcher

	// Lifecycle guards
	closed    chan struct{}
	closeOnce sync.Once
	exitDone  chan struct{}
	pumpWG    sync.WaitGroup
}

// newDriver is the chat-session Start() entry. It blocks until
// session.create returns or ctx fires.
func newDriver(ctx context.Context, s *Starter, cfg agent.StartConfig) (*driver, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("dsh: workspace is required")
	}

	// Spawn `dsh --profile web --port 0` and parse the bound URL.
	// We use --port 0 so the OS picks a free port; the URL is on
	// stdout in the first few lines.
	cmd := agent.NewCmd(ctx, "dsh", "--profile", "web", "--port", "0")
	cmd.Dir = cfg.Workspace
	// Per agent-no-config-tampering: only inject permissions.
	// Model / provider / credentials flow from ~/.dsh/settings.yaml.
	//
	// R-streamIdle: dsh-internal 5-min idle watch on the LLM stream
	// (DEFAULT_STREAM_IDLE_TIMEOUT_MS in dsh-llm-pi-ai / dsh-llm-deepseek).
	// Long DSH thinking + tools routinely idle 5+ min, after which dsh
	// aborts the LLM stream and the session dies (we observe dsh 26931
	// went from active to gone in 891 ms at 08:18:10). The streamIdle
	// timeout is overridable per-provider via plugin config, but we can't
	// touch the user's ~/.dsh/settings.yaml — by R4 we fold the env
	// forward here. 24h is the upper bound Node's setTimeout accepts
	// (MAX_TIMER_DELAY_MS); user can override at their end by leaving
	// this env unset and editing their config.
	streamIdleTimeoutMs := "86400000"
	if v := os.Getenv("NIGHTME_DSH_STREAM_IDLE_TIMEOUT_MS"); v != "" {
		streamIdleTimeoutMs = v
	}
	cmd.Env = append(os.Environ(),
		"DSH_PERMISSION_MODE=danger-full-access",
		"DSH_LLM_PI_AI_PROVIDERS_MINIMAX_CN_STREAMIDLETIMEOUTMS="+streamIdleTimeoutMs,
		"DSH_LLM_DEEPSEEK_PROVIDERS_DEEPSEEK_OFFICIAL_STREAMIDLETIMEOUTMS="+streamIdleTimeoutMs,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("dsh: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("dsh: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("dsh: start: %w", err)
	}

	// Info-level lifecycle log: emits the spawned dsh process pid
	// and the bound web URL — the two facts the runtime/operator
	// most need when triaging a "session won't start" report (e.g.
	// matching the pid against a stuck process, copying the URL
	// into a browser to reproduce a UI bug). Per
	// docs/bridge/dsh.md §8.4, real-machine cold start is ~1.5s;
	// seeing this line in nightme.log confirms the spawn actually
	// happened. dLog (Debug) would be invisible under the default
	// Info level and so useless for the support flow.
	slog.Default().Info("dsh: web spawned",
		"pid", cmd.Process.Pid,
		"argv", cmd.Args,
		"workspace", cfg.Workspace)

	urlCtx, urlCancel := context.WithTimeout(ctx, webURLParseTimeout)
	baseURL, err := parseWebURL(urlCtx, stdout)
	urlCancel()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("dsh: parse web url: %w", err)
	}
	// Info-level lifecycle log: the bound web URL is the operator's
	// primary entry point for live debugging (paste into browser,
	// watch HMR reloads, inspect permissions UI). Surfacing it on
	// every startup makes "which port did dsh pick this time?"
	// greppable from nightme.log. dLog would be filtered out at the
	// default Info level, defeating the purpose.
	slog.Default().Info("dsh: web url parsed",
		"url", baseURL,
		"pid", cmd.Process.Pid)

	// Allocate the driver skeleton BEFORE dialing the WSs so we
	// can pass d.closed to dialMux/dialHost as the per-WS ping
	// keeper's stop signal. Without this, d.closed doesn't exist
	// at dial time and the keeper has nothing to watch — which
	// used to be fine because there was no keeper. F-DSH-PING
	// added the keeper; threading the chan through here is the
	// matching half of the contract.
	d := &driver{
		cmd:           cmd,
		stdout:        stdout,
		stderr:        stderr,
		stderrTail:    agent.NewStderrRingBuffer(agent.StderrTailBytes),
		http:          newHTTPClient(baseURL),
		workspace:     cfg.Workspace,
		agentName:     s.name,
		pendingApprovals: map[string]chan string{},
		lastApprovalID:  map[string]string{},
		events:        make(chan agent.AgentEvent, eventBufferSize),
		translate:     newTranslator(s.name, cfg.Workspace),
		wireState:     newWireState(),
		closed:        make(chan struct{}),
		exitDone:      make(chan struct{}),
	}

	// Dial the two WebSocket downlinks. The server pushes frames
	// as soon as the upgrade completes; ordering between mux and
	// host upgrades is irrelevant — both feed the same translator.
	// Each dial spawns a ping-keeper goroutine that exits when
	// d.closed is closed in d.Close().
	muxWS, err := dialMux(ctx, baseURL, d.closed)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("dsh: dial mux: %w", err)
	}
	hostWS, err := dialHost(ctx, baseURL, d.closed)
	if err != nil {
		_ = muxWS.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("dsh: dial host: %w", err)
	}
	d.muxWS = muxWS
	d.hostWS = hostWS
	// Wire dispatcher AFTER wireState + translate are set. The
	// dispatcher's deliver closure captures d.deliver (which
	// closes-over d.events), so this must run before startPumps.
	d.dispatcher = newDispatcher(d.translate, d.wireState, d, d.deliver)

	// Session handshake: resume via session.fork when cfg.SessionID
	// is set, otherwise create a fresh session via session.create.
	// session.fork returns a new server-assigned sessionId whose
	// server-side history mirrors the parent's; session.create
	// always returns a brand-new empty session. The `resumed` flag
	// is consumed by the EventAgentReady emission below to surface
	// the resume source in the runtime's receipt header.
	//
	// handshakeSession owns its own per-call timeouts (it derives
	// forkCtx / createCtx from the parent ctx internally) so a
	// slow fork can't starve the create fallback's budget. We pass
	// the spawn ctx directly.
	//
	// F-62 (2026-08-15): handshakeSession runs BEFORE startPumps so
	// the WS / stderr pumps read a fully-initialized d.sessionID
	// (used in PanicEventHandler for crash reports). Previously the
	// order was inverted — pumps started first and raced the
	// handshake's write to d.sessionID, occasionally producing panic
	// reports with an empty sessionID and tripping the race detector.
	resumed, hsErr := d.handshakeSession(ctx, cfg)
	if hsErr != nil {
		_ = d.Close()
		return nil, hsErr
	}

	// F-62 (2026-08-15): startPumps now runs AFTER handshakeSession
	// returns — see newDriver init-order comment. d.sessionID is
	// populated before any goroutine reads it.
	d.startPumps()
	// handshakeSession already logs the success line at INFO
	// ("dsh: session forked" or "dsh: session created") with the
	// full session_id / requested_id / new_id trio. We deliberately
	// do NOT add a duplicate dLog here — the Info-level line is
	// the canonical support-flow trail, and the resumed flag is
	// only consumed by the EventAgentReady emission below.
	_ = resumed // surface is EventAgentReady.SessionID, not a log line

	// Pull the authoritative model selection via /api/session.models.
	// session.create does NOT return the model — dsh requires the
	// adapter to resolve the model route asynchronously (catalog
	// lookup) and the selection is only readable through this RPC.
	// Without this call, EventAgentReady.Model would always be empty
	// AND a follow-up session.prompt would fail with `model-unavailable`
	// (per the踩坑记录 in docs/bridge/dsh.md §8.7).
	//
	// Failure handling: we do NOT abort startup on a transport
	// hiccup — dsh's defaults (`~/.dsh/settings.yaml`'s
	// `agent-default-model`) mean a successful session.create was
	// almost certainly followed by a routable selection, and the
	// runtime renders EventAgentReady even when Model is empty.
	// A debug log is the right level; the next turn's session.prompt
	// will surface the real failure (server-side `model-unavailable`)
	// if the model is genuinely broken. We intentionally avoid
	// surfacing this as EventAgentError to keep the bridge tolerant
	// of transient catalog race conditions on cold start.
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

	// Emit EventAgentReady so the runtime can capture SessionID +
	// Workspace + Branch + Model. Model is filled from the
	// session.models probe above; stays empty when the probe
	// failed (the runtime renders EventAgentReady either way).
	d.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: d.sessionID,
		AgentName: d.agentName,
		Workspace: d.workspace,
		Branch:    detectBranch(d.workspace),
		Model:     d.model,
	})

	// F-DSH-TODO-FIX-LOG: emit a single Info-level audit line
	// stating the assumed wire field names. Combined with the
	// per-skipped-item Warn log in applyTodoWriteLocked /
	// applyTodoProjectionLocked, this gives ops a complete
	// paper trail when a "todo list not converted" report
	// comes in:
	//
	//   1. Grep nightme.log for "dsh: wire assumptions" — see
	//      which field names the bridge assumed.
	//   2. Grep for "wire field-name drift" — see which frames
	//      mismatched, with raw bytes attached for diffing.
	//
	// If the assumption list drifts from the dsh wire, the diff
	// is visible in the audit line and the per-mismatch warn
	// logs pinpoint the exact frame that broke.
	slog.Default().Info("dsh: wire assumptions",
		"todo_item_id_field", "id",
		"todo_item_content_field", "content",
		"todo_item_active_form_field", "activeForm",
		"todo_item_status_field", "status",
		"todo_status_values", []string{"pending", "in_progress", "completed"},
		"projection_field", "projection",
		"projection_value_field", "value",
		"tool_view_kind_field", "kind",
		"tool_view_kind_task_list_value", "task_list",
		"hint", "if these field names drift from dsh wire, see 'todo/write items dropped' warnings above")

	return d, nil
}

// fetchSessionModels calls POST /api/session.models with the
// driver's sessionId and decodes the SessionModels envelope.
// Returns the decoded value on success; callers decide how to
// degrade on error.
//
// We deliberately keep this a thin helper rather than baking it
// into newDriver — both the session-create probe and any future
// "refresh model after a runtime hint" path reuse it with a fresh
// modelCtx.
func (d *driver) fetchSessionModels(ctx context.Context) (*sessionModelsValue, error) {
	if d.sessionID == "" {
		return nil, errors.New("dsh: session not initialized")
	}
	resp, err := d.http.Post(ctx, "session.models", map[string]any{
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

// handshakeSession runs the resume-or-create handshake against
// dsh web and writes d.sessionID on success.
//
// The supplied ctx is the PARENT context (typically the spawn
// context from newDriver). We derive per-call deadlines INSIDE
// this function rather than accepting a pre-budgeted context,
// because the fork and create branches have independent timeout
// needs — a slow fork (large parent history → slow history copy)
// must NOT eat the budget for the create fallback that would
// normally follow it.
//
// When cfg.SessionID is non-empty, we first try session.fork —
// dsh web's documented resume primitive (docs/bridge/dsh.md §1.3,
// "从现有 session 开新 (daemon 重启续接用)"). session.fork creates
// a NEW server-assigned session that mirrors the parent's history;
// we capture the new id into d.sessionID and surface resumed=true.
//
// On ANY fork failure (transport error, business error, decode
// failure, empty id) we deliberately refuse to spawn rather than
// silently fall back to session.create. Mirrors claudecode's
// resume-preservation invariant (claudecode/starter.go §87-103):
// a silent fallback would let a stale sessionId linger in
// registry forever, and on every subsequent daemon restart the
// bridge would re-attempt the same bad fork and re-fall-back,
// costing the user their history without any operator-visible
// signal. Instead, we wrap the failure with agent.ErrResumeUnhealthy
// so the runtime's auto-retry path (chatsession.go §1624) clears
// the persisted sessionId and respawns fresh on the user's NEXT
// message. The user pays one extra cold-start; they do NOT pay
// permanent history loss.
//
// When cfg.SessionID is empty, we go straight to session.create.
// A failure there is a true bridge error (server down, bad config)
// — surfaced verbatim so the dispatcher can render it.
//
// Returns (resumed, nil) on success, where `resumed` indicates the
// cfg.SessionID was honored via session.fork. Returns (false, nil)
// on the "no SessionID requested" path. Returns (false, err)
// where err satisfies errors.Is(err, agent.ErrResumeUnhealthy) on
// any fork failure, and a plain wrapped error on create failure.
func (d *driver) handshakeSession(ctx context.Context, cfg agent.StartConfig) (bool, error) {
	if cfg.SessionID != "" {
		forkCtx, forkCancel := context.WithTimeout(ctx, handshakeTimeout)
		forkResp, err := d.http.Post(forkCtx, "session.fork", map[string]any{
			"sessionId": cfg.SessionID,
		})
		forkCancel()
		if err != nil {
			reason := "transport error: " + errStr(err)
			slog.Default().Warn("dsh: session.fork transport error; refusing fallback",
				"requested_id", cfg.SessionID,
				"err", errStr(err))
			return false, resumeUnhealthyError{reason: reason, session: cfg.SessionID}
		}
		if !forkResp.Result.OK {
			reason := "rejected: " + forkResp.Result.ErrorMessage()
			slog.Default().Warn("dsh: session.fork rejected; refusing fallback",
				"requested_id", cfg.SessionID,
				"err", forkResp.Result.ErrorMessage())
			return false, resumeUnhealthyError{reason: reason, session: cfg.SessionID}
		}
		var fv sessionForkValue
		if uerr := json.Unmarshal(forkResp.Result.Value, &fv); uerr != nil {
			reason := "value decode failed: " + errStr(uerr)
			slog.Default().Warn("dsh: session.fork value decode failed; refusing fallback",
				"requested_id", cfg.SessionID,
				"err", errStr(uerr))
			return false, resumeUnhealthyError{reason: reason, session: cfg.SessionID}
		}
		if fv.SessionID == "" {
			slog.Default().Warn("dsh: session.fork returned empty sessionId; refusing fallback",
				"requested_id", cfg.SessionID)
			return false, resumeUnhealthyError{
				reason:  "empty sessionId in response",
				session: cfg.SessionID,
			}
		}
		d.sessionID = fv.SessionID
		slog.Default().Info("dsh: session forked",
			"requested_id", cfg.SessionID,
			"new_id", d.sessionID)
		return true, nil
	}

	createCtx, createCancel := context.WithTimeout(ctx, handshakeTimeout)
	createResp, err := d.http.Post(createCtx, "session.create", map[string]any{
		"cwd":   cfg.Workspace,
		"title": filepath.Base(cfg.Workspace),
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
	slog.Default().Info("dsh: session created", "session_id", d.sessionID)
	return false, nil
}

// resumeUnhealthyError is returned by handshakeSession when the
// caller asked for resume (cfg.SessionID != "") and session.fork
// refused for any reason. It satisfies errors.Is for both
// agent.ErrResumeUnhealthy (the cross-package sentinel the chat
// layer uses to drive auto-recovery at chatsession.go §1624) AND
// ErrResumeUnhealthy (the bridge-local mirror, for symmetry with
// the claudecode bridge). fmt.Errorf's %w only retains the last
// wrap, so we expose Is() to match both sentinels.
//
// Mirrors claudecode/claudecode.go's resumeUnhealthyError.
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

// parseWebURL reads stdout until it sees the `dsh web: http://…` line
// or ctx fires. We use a goroutine + channel instead of bufio.Scanner
// because we want timeout cancellation mid-read.
func parseWebURL(ctx context.Context, stdout io.Reader) (string, error) {
	type result struct {
		url string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 4096), 16*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if m := dshURLPattern.FindStringSubmatch(line); m != nil {
				ch <- result{url: fmt.Sprintf("http://%s:%s", m[1], m[2])}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			ch <- result{err: fmt.Errorf("scan stdout: %w", err)}
			return
		}
		ch <- result{err: errors.New("dsh web: stdout closed before URL line appeared")}
	}()
	select {
	case r := <-ch:
		return r.url, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("timeout after %s waiting for dsh web url", webURLParseTimeout)
	}
}

// startPumps wires the four read goroutines + lifecycle. After
// this returns, the driver is "live": events flow on d.events
// until Close().
//
// We start FIVE goroutines, not four:
//   1. mux pump (events)
//   2. host pump (lifecycle)
//   3. stderr drain (debug)
//   4. stdout drain (URL was already extracted by parseWebURL,
//      but dsh keeps printing HMR / plugin init / debug lines
//      — we MUST keep the pipe flowing or dsh blocks once its
//      64 KiB stdout pipe buffer fills, deadlocking the bridge.)
//   5. lifecycle (event close + pending approval fail)
//
// Each pump is wrapped in agent.SafeGo (outer, daemon-level
// safety net) + agent.PanicEventHandler (inner, domain-level
// recovery) so a single bridge bug (e.g. a translator panic
// in the mux pump) cannot tear down the entire nightme daemon
// AND the runtime can surface a "bridge died" EventAgentError
// to the user instead of leaving a zombie session. The
// 2026-08-15 dsh textBuf panic that motivated this pattern is
// the prototype — pre-fix, it killed the daemon; with this
// two-layer recovery, the user would get a bridge-error card
// and the daemon would stay alive. See internal/agent/safego.go
// for the contract.
func (d *driver) startPumps() {
	// pumpWG counts the four data pumps: mux, host, stderr,
	// stdout. lifecycle is wrapped in SafeGo (so a panic there
	// is recovered and the daemon stays alive) but is NOT in
	// pumpWG — the lifecycle itself calls pumpWG.Wait() inside
	// its body, so adding itself to the WaitGroup would
	// deadlock (it would wait for its own Done). SafeGo gives
	// us daemon-level safety; no WaitGroup slot is needed
	// because lifecycle is the orchestrator, not a peer of the
	// pumps. See internal/agent/safego.go for the contract.
	d.pumpWG.Add(4)
	branch := detectBranch(d.workspace)
	agent.SafeGo("dsh:mux-pump", func() {
		defer d.pumpWG.Done()
		defer agent.PanicEventHandler(
			"dsh:mux-pump", d.deliver,
			d.sessionID, d.agentName, d.workspace, branch)
		readMuxPump(d.muxWS, "mux", d.handleMuxFrame)
	})
	agent.SafeGo("dsh:host-pump", func() {
		defer d.pumpWG.Done()
		defer agent.PanicEventHandler(
			"dsh:host-pump", d.deliver,
			d.sessionID, d.agentName, d.workspace, branch)
		readMuxPump(d.hostWS, "host", d.handleHostFrame)
	})
	agent.SafeGo("dsh:stderr-drain", func() {
		defer d.pumpWG.Done()
		defer agent.PanicEventHandler(
			"dsh:stderr-drain", d.deliver,
			d.sessionID, d.agentName, d.workspace, branch)
		d.drainStderr()
	})
	agent.SafeGo("dsh:stdout-drain", func() {
		defer d.pumpWG.Done()
		defer agent.PanicEventHandler(
			"dsh:stdout-drain", d.deliver,
			d.sessionID, d.agentName, d.workspace, branch)
		d.drainStdout()
	})
	// lifecycle is the supervisor that closes events / exitDone /
	// fails pending approvals. dsh's lifecycle already has
	// `defer close(d.events)` and `defer close(d.exitDone)` at
	// the top, so a panic in lifecycle still tears the bridge
	// down cleanly; SafeGo is the outer safety net in case any
	// of those defers panic (e.g. a nil deref in
	// d.pendingApprovals). lifecycle is NOT in pumpWG above —
	// it calls d.pumpWG.Wait() inside its body, so adding
	// itself would deadlock (it would wait for its own Done).
	agent.SafeGo("dsh:lifecycle", d.lifecycle)
}

// lifecycle owns the events-chan close and exitDone signal. It
// waits for the child process, fails pending approvals, and emits
// EventAgentError if the child died abnormally.
//
// Defer order is significant: exitDone FIRST so Close() can
// observe the lifecycle's exit signal; events SECOND so any caller
// that synchronizes on Close() returning knows the channel is
// closed and no more events will be observed (LIFO would invert
// this — exitDone closes AFTER events, but Close() only blocks on
// exitDone, so Close() returning would race against events still
// draining).
//
// gracefulClose marks whether the bridge was shut down by Close()
// (vs. dying on its own). When true, we suppress EventAgentError
// because a SIGINT'd-but-clean shutdown is not an error from the
// runtime's perspective (cf. codex's isGracefulClose() guard).
func (d *driver) lifecycle() {
	// Defer registration order is exitDone first, events second —
	// but defers execute in LIFO order, so events closes FIRST
	// and exitDone closes SECOND. Close() blocks on exitDone, so
	// Close() returning is guaranteed to be AFTER events is
	// closed and drained by the runtime. Reversing the
	// registration order would close exitDone first and let
	// Close() return while events is still being drained — a
	// real race.
	defer close(d.exitDone)
	defer close(d.events)

	waitErr := d.cmd.Wait()
	d.pumpWG.Wait() // wait for mux/host/stderr pumps to return

	// Fail any pending approvals so runtime handlers don't hang.
	d.pendingMu.Lock()
	for id, ch := range d.pendingApprovals {
		select {
		case ch <- "declined":
		default:
		}
		delete(d.pendingApprovals, id)
	}
	d.pendingOrder = nil
	d.lastApprovalID = nil
	d.pendingMu.Unlock()

	// isGracefulClose: Close() sets d.closed BEFORE signaling the
	// process, so by the time we read it the user explicitly
	// requested shutdown. SIGINT'd-but-clean waitErr is not a
	// runtime error (the runtime asked for it).
	graceful := isClosed(d.closed)
	exitKind := agent.ClassifyExit(waitErr, graceful)
	if !graceful {
		// Bridge died without our permission — emit EventAgentError
		// with a structured Diagnostic so the runtime / recovery
		// policy / /diagnose can act on it. Even when waitErr is
		// nil (clean-exit but unrequested), we still emit so the
		// user-visible event stream reflects the unexpected exit.
		tail := ""
		if d.stderrTail != nil {
			tail = d.stderrTail.String()
		}
		diag := &agent.BridgeDiagnostic{
			ExitKind:   exitKind,
			WaitErr:    waitErr,
			StderrTail: tail,
			SessionID:  d.sessionID,
			AgentName:  d.agentName,
			KilledAt:   time.Now(),
		}
		errMsg := fmt.Sprintf("dsh: lifecycle exit %s: %v", exitKind, errStr(waitErr))
		if tail != "" {
			errMsg += "\n--- stderr tail ---\n" + agent.TruncateForLog(tail, 1024)
		}
		d.deliver(agent.AgentEvent{
			Kind:       agent.EventAgentError,
			Err:        fmt.Errorf("%s", errMsg),
			Diagnostic: diag,
		})
	}
	dLog("dsh: lifecycle exit",
		"exit_kind", exitKind,
		"graceful", graceful,
		"err", errStr(waitErr))
}

// isClosed reports whether the chan has been signaled.
// Cheap non-blocking check via select-default.
func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// drainStderr consumes dsh's stderr into the dLog AND mirrors raw
// bytes into d.stderrTail. Why both:
//
//   - dLog is Debug-gated (kept quiet in production) — operators
//     don't want HMR / plugin-init noise.
//   - d.stderrTail is captured unconditionally (no Debug gating)
//     so when lifecycle() detects a non-graceful exit, it can
//     attach the last few hundred bytes of stderr to the
//     EventAgentError.Diagnostic. /diagnose and the recovery
//     policy's stderr fingerprint both read from this ring.
//
// dsh web's stderr is mostly noise but the LAST lines before a
// crash (e.g. "dsh: fatal load failure: TypeError: ...") are gold
// for post-mortem. Capturing them costs ~4 KiB per bridge.
func (d *driver) drainStderr() {
	drainStream(d.stderr, "stderr", d.closed, d.stderrTail)
}

// drainStdout keeps dsh's stdout pipe flowing after parseWebURL
// extracted the URL line. Without this, dsh web's stdout pipe
// fills its 64 KiB kernel buffer within seconds of HMR + plugin
// init logging, then dsh blocks on its next stdout write — which
// deadlocks the bridge even though our events path is independent.
//
// We don't parse the lines (the URL was already captured); we just
// discard bytes. dLog at debug level for the post-URL output.
func (d *driver) drainStdout() {
	drainStream(d.stdout, "stdout (post-url)", d.closed, nil)
}

// deliver sends an event to the runtime's read pump. NEVER blocks:
//
//   - closed chan: bridge is shut down, drop silently
//   - exitDone chan: lifecycle finished, drop silently
//   - default:     events buffer full + bridge alive = runtime
//                  consumer can't keep up; DROP and log warning.
//
// The default branch is critical: without it, a slow consumer
// (gateway doing I/O per event) can stall the WS readPump which
// then back-pressures dsh web's WS write, deadlocking the entire
// bridge. dsh web can burst hundreds of events per tool-heavy
// turn; we size eventBufferSize generously (131072) to make drops
// rare, but the non-blocking send is the real deadlock defense.
//
// Stamps SessionID / AgentName / Workspace on every event so
// downstream consumers (gateway, chatsession, channel adapters)
// can route / correlate without re-reading bridge state.
func (d *driver) deliver(ev agent.AgentEvent) {
	if ev.SessionID == "" {
		ev.SessionID = d.sessionID
	}
	if ev.AgentName == "" {
		ev.AgentName = d.agentName
	}
	if ev.Workspace == "" {
		ev.Workspace = d.workspace
	}

	select {
	case d.events <- ev:
	case <-d.closed:
		// bridge closed; drop silently — nobody reads anyway
	case <-d.exitDone:
		// lifecycle done; drop silently
	default:
		// Buffer full + bridge alive. Drop with a warning so
		// operators can size up if this fires often.
		dLog("dsh: deliver dropped (events buffer full)",
			"kind", ev.Kind.String(),
			"session_id", ev.SessionID)
	}
}

// ─── driver interface (for agent.NewAgent) ─────────────────────────────

// SendBlocks submits a user turn. dsh's session.prompt takes a
// content-block array (matching nightme's ContentBlock shape via
// contentBlocksToDTO) plus a required `mode` field
// ("queue" | "steer"). We send and return — the actual turn
// events arrive on d.events via the mux pump.
func (d *driver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	if d.sessionID == "" {
		return errors.New("dsh: session not initialized")
	}
	content, err := contentBlocksToDTO(blocks)
	if err != nil {
		return fmt.Errorf("dsh: encode prompt content: %w", err)
	}
	resp, err := d.http.Post(ctx, "session.prompt", map[string]any{
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
	// the actual turn happens asynchronously and arrives via WS.
	return nil
}

// SendPermission routes a user decision back to the OLDEST pending
// approval (FIFO). Maps and Go map iteration are randomized; we
// keep an explicit pendingOrder slice so this is deterministic.
// Mirrors codex §6.4 lastPendingID pattern but stricter — codex
// keeps "most-recent" via last-write-wins on a single field;
// dsh can have multiple in-flight approvals (tool permission + a
// question at the same time), so we use FIFO instead.
//
// The previous implementation iterated `range d.pendingApprovals`
// and picked an arbitrary key. With concurrent approvals (mux +
// session/event), the user's answer for one could land on another.
func (d *driver) SendPermission(resp string) error {
	d.pendingMu.Lock()
	if len(d.pendingOrder) == 0 {
		d.pendingMu.Unlock()
		return errors.New("dsh: no pending approval to answer")
	}
	approvalID := d.pendingOrder[0]
	d.pendingOrder = d.pendingOrder[1:]
	// delete is a no-op when the key is absent (per Go spec), so the
	// guard would only save a hash lookup — not worth the line.
	delete(d.pendingApprovals, approvalID)
	d.pendingMu.Unlock()

	// Outcome shape mirrors dsh's ApprovalOutcome. We only support
	// the two-string answer for now (approve / decline); richer
	// multi-option outcomes arrive once we know dsh's full surface.
	outcome := map[string]string{"kind": resp}
	_, err := d.http.Post(context.Background(), "respond", map[string]any{
		"rpcId":   approvalID,
		"payload": map[string]any{"outcome": outcome},
	})
	return err
}

// Reset clears the conversation context on the running session.
// dsh's protocol doesn't expose an in-place /clear; the cleanest
// path is to spawn a new session with the same cwd. We return
// ErrRestartRequired so the wrapper layer (agentsession.AgentSession)
// kills the current driver and re-spawns.
func (d *driver) Reset(ctx context.Context) error {
	return agent.ErrRestartRequired
}

// ListSessions calls POST /api/session.list and decodes the
// returned Session array. Used by the runtime's resume picker
// (the bridge exposes this through Starter.ListSessions, which
// spins up a fresh dsh web just long enough to hit the endpoint).
//
// LIMIT / PAGINATION — IMPORTANT:
// 实机 probe 2026-08-15 against dsh 0.1.0-rc.6 showed that the
// `limit` payload field is IGNORED by the server (passing
// limit=2 returned 243 items; limit=0, -5, and no-limit all
// returned the same full set). Other names we tried (pageSize,
// page_size, count, max) were likewise ignored. dsh has no
// pagination for session.list — every call returns the daemon's
// full session store. The bridge therefore drops the limit
// parameter from this signature; if dsh ever adds pagination,
// the runtime's picker UI should add a new method (e.g.
// ListSessionsPage) rather than reusing this one.
//
// Cross-workspace contamination: dsh web is daemon-wide — every
// session across every workspace lives in the same global store.
// Callers that want to filter to "sessions for THIS /cwd" must
// inspect Session.CWD themselves; the bridge does NOT pre-filter
// because (a) dsh doesn't expose a cwd scope and (b) a future
// daemon-wide picker UI might want to surface cross-workspace
// sessions explicitly.
//
// The returned slice ordering is server-defined; we don't
// re-sort. dsh appears to return most-recently-updated first
// (Session.UpdatedAt DESC), which is what the picker UI wants.
func (d *driver) ListSessions(ctx context.Context) ([]Session, error) {
	resp, err := d.http.Post(ctx, "session.list", map[string]any{})
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

// Close shuts the bridge down. Idempotent. Closes WS, sends
// shutdown signal via stdin, and waits for lifecycle (5s SIGTERM
// + 5s SIGKILL ladder).
func (d *driver) Close() error {
	var err error
	d.closeOnce.Do(func() {
		close(d.closed)
		// Close WS connections — pumps return EOF and exit.
		_ = d.muxWS.Close()
		_ = d.hostWS.Close()
		// Send session.cancel (best-effort) before tearing the child
		// down — this lets in-flight turns settle gracefully.
		if d.sessionID != "" {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = d.http.Post(cancelCtx, "session.cancel", map[string]any{
				"sessionId": d.sessionID,
			})
			cancel()
		}
		// SIGINT — dsh web handles SIGINT gracefully (closes WS,
		// persists final session, exits 0).
		if d.cmd.Process != nil {
			_ = d.cmd.Process.Signal(os.Interrupt)
		}
		// Wait for lifecycle (5s SIGTERM grace + 5s SIGKILL fallback).
		select {
		case <-d.exitDone:
		case <-time.After(5 * time.Second):
			if d.cmd.Process != nil {
				_ = d.cmd.Process.Kill()
			}
			select {
			case <-d.exitDone:
			case <-time.After(5 * time.Second):
				err = errors.New("dsh: child did not exit within SIGKILL grace")
			}
		}
		_ = d.stdout.Close()
		_ = d.stderr.Close()
	})
	return err
}

// Stop sends SIGTERM via the lifecycle path. Used when the runtime
// wants to halt a stuck turn without closing the bridge entirely.
// dsh web doesn't currently expose an in-flight cancel via signal;
// we route through Cancel HTTP call and fall back to Close if the
// process doesn't free up.
func (d *driver) Stop(ctx context.Context) error {
	if d.sessionID != "" {
		_, _ = d.http.Post(ctx, "session.cancel", map[string]any{
			"sessionId": d.sessionID,
		})
	}
	return nil
}

// ─── wire content blocks ──────────────────────────────────────────────

// contentBlocksToDTO converts nightme's ContentBlock slice to
// dsh's wire format. dsh web's session.prompt accepts exactly
// two content-block variants:
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
