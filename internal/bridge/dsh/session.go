// session.go — long-lived chat session driver for `dsh --profile web`.
//
// newDriver is the entry point: it spawns dsh web, parses the bound
// URL from stdout, dials the two WebSocket downlinks, performs
// session.create, and emits EventAgentReady. It returns a *driver
// that satisfies the agent.driver interface (SendBlocks /
// SendPermission / Reset / Close / Stop / SetModel).
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

// handshakeTimeout bounds the session.create POST. dsh web is
// already accepting HTTP by the time we dial WS, but the first
// session.create round-trips through Cordis plugin init, so give
// it generous slack.
const handshakeTimeout = 15 * time.Second

// lifecycleWatchdogTimeout bounds cmd.Wait() inside the lifecycle
// goroutine. If the child doesn't exit within this window, the
// watchdog fires SIGKILL. This prevents a wedged dsh web (e.g.
// plugin init hang that doesn't honor SIGINT) from holding the
// bridge forever — the bridge would otherwise pin ~26 MiB of
// events buffer + 1 goroutine indefinitely. Sized generously
// (5 min) because normal dsh shutdown should complete within
// seconds; the watchdog is the "nothing else worked" fallback,
// not the common path.
const lifecycleWatchdogTimeout = 5 * time.Minute

// webURLParseTimeout bounds waiting for dsh web to print its bound
// URL on stdout. Real-machine cold start is ~1.5s; 10s is generous.
const webURLParseTimeout = 10 * time.Second

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

	// model is the model's authoritative selection captured at
	// session-create time via /api/session.models. Bridge stamps
	// it onto EventAgentReady.Model so the runtime's receipt
	// header renders "session <id> · model <name>". Updated by
	// SetModel after a successful /api/session.selectModel so
	// the next EventAgentReady (or the next turn's header) shows
	// the new model without a separate re-emit.
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
	cmd.Env = append(os.Environ(), "DSH_PERMISSION_MODE=danger-full-access")

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

	// Dial the two WebSocket downlinks. The server pushes frames
	// as soon as the upgrade completes; ordering between mux and
	// host upgrades is irrelevant — both feed the same translator.
	muxWS, err := dialMux(ctx, baseURL)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("dsh: dial mux: %w", err)
	}
	hostWS, err := dialHost(ctx, baseURL)
	if err != nil {
		_ = muxWS.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("dsh: dial host: %w", err)
	}

	d := &driver{
		cmd:           cmd,
		stdout:        stdout,
		stderr:        stderr,
		muxWS:         muxWS,
		hostWS:        hostWS,
		http:          newHTTPClient(baseURL),
		workspace:     cfg.Workspace,
		agentName:     s.name,
		pendingApprovals: map[string]chan string{},
		lastApprovalID:  map[string]string{},
		events:        make(chan agent.AgentEvent, eventBufferSize),
		translate:     newTranslator(s.name, cfg.Workspace),
		closed:        make(chan struct{}),
		exitDone:      make(chan struct{}),
	}
	d.startPumps()

	// session.create — required before any session.prompt. We send
	// cwd + title (just the basename for now). sessionId is captured
	// into d.sessionID; without it nothing else works.
	hsCtx, hsCancel := context.WithTimeout(ctx, handshakeTimeout)
	defer hsCancel()
	createResp, err := d.http.Post(hsCtx, "session.create", map[string]any{
		"cwd":   cfg.Workspace,
		"title": filepath.Base(cfg.Workspace),
	})
	if err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("dsh: session.create: %w", err)
	}
	if !createResp.Result.OK {
		_ = d.Close()
		return nil, fmt.Errorf("dsh: session.create rejected: %s",
			createResp.Result.ErrorMessage())
	}
	var scVal sessionCreateValue
	if err := json.Unmarshal(createResp.Result.Value, &scVal); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("dsh: decode session.create value: %w", err)
	}
	d.sessionID = scVal.SessionID
	dLog("dsh: session created", "session_id", d.sessionID)

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

	return d, nil
}

// fetchSessionModels calls POST /api/session.models with the
// driver's sessionId and decodes the SessionModels envelope.
// Returns the decoded value on success; callers decide how to
// degrade on error.
//
// We deliberately keep this a thin helper rather than baking it
// into newDriver — future SetModel may want to refresh the
// selection after a switch (server-side `current` may lag), at
// which point the same helper takes a fresh modelCtx.
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

	// Watchdog: if cmd.Wait() never returns (e.g. dsh web hung on a
	// plugin init that didn't honor SIGINT/SIGTERM), force-kill
	// after a generous timeout. This is the "Stop() didn't work
	// AND Close() never fired" worst case — without this the
	// bridge would hold 26 MiB of events buffer + 1 goroutine
	// forever.
	watchdog := time.AfterFunc(lifecycleWatchdogTimeout, func() {
		if d.cmd.Process != nil {
			dLog("dsh: lifecycle watchdog firing SIGKILL",
				"timeout", lifecycleWatchdogTimeout)
			_ = d.cmd.Process.Kill()
		}
	})
	defer watchdog.Stop()

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
	if !graceful && waitErr != nil {
		d.deliver(agent.AgentEvent{Kind: agent.EventAgentError, Err: waitErr})
	}
	dLog("dsh: lifecycle exit", "err", errStr(waitErr), "graceful", graceful)
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

// drainStderr consumes dsh's stderr into the dLog. dsh web may
// emit warnings (HMR, plugin init) which we want visible in debug
// logs but don't need to surface to the user.
func (d *driver) drainStderr() {
	drainStream(d.stderr, "stderr", d.closed)
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
	drainStream(d.stdout, "stdout (post-url)", d.closed)
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
	if _, ok := d.pendingApprovals[approvalID]; ok {
		delete(d.pendingApprovals, approvalID)
	}
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

// SetModel switches the active model on the running session via
// /api/session.selectModel. Mirrors the dsh sessions.ts wire:
// { sessionId, provider, model } — `reasoningEffort` is left
// empty so the adapter preserves the route's existing effort
// (omit = preserve default per the SessionsApi.selectModel JSDoc).
//
// On success we update d.model from the server's authoritative
// selection (`result.selected.model`) so the next EventAgentReady
// (or the next receipt header render) reflects the new model.
// The runtime does not currently re-emit EventAgentReady from
// SetModel — that path is the runtime's job, see agentsession.
//
// The session-id guard matches codex's acp pattern: SetModel on
// an uninitialized driver returns a transport error rather than
// silently no-op'ing, so callers can distinguish "bridge not
// ready" from "model unchanged".
func (d *driver) SetModel(ctx context.Context, providerID, modelID string) error {
	if d.sessionID == "" {
		return errors.New("dsh: session not initialized")
	}
	if providerID == "" || modelID == "" {
		return fmt.Errorf("dsh: SetModel requires both providerID and modelID (got %q, %q)",
			providerID, modelID)
	}
	req := selectModelRequest{
		SessionID: d.sessionID,
		Provider:  providerID,
		Model:     modelID,
	}
	resp, err := d.http.Post(ctx, "session.selectModel", req)
	if err != nil {
		return fmt.Errorf("dsh: session.selectModel: %w", err)
	}
	if !resp.Result.OK {
		return fmt.Errorf("dsh: session.selectModel rejected: %s",
			resp.Result.ErrorMessage())
	}
	var out selectModelValue
	if err := json.Unmarshal(resp.Result.Value, &out); err != nil {
		// Don't fail the switch on decode — server already
		// accepted the change. Log and continue; the next
		// session.models probe (or the next turn's usage
		// payload) will refresh d.model anyway.
		dLog("dsh: selectModel decode warning (using requested model)",
			"err", errStr(err))
		d.model = modelID
		return nil
	}
	d.model = out.Selected.Model
	dLog("dsh: model switched",
		"new_model", d.model,
		"new_provider", out.Selected.Provider)
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
