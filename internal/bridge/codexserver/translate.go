// Package codexserver — translator + notification / server-request dispatch.
//
// This file owns the F-52-style state machine that turns app-server
// wire envelopes into agent.AgentEvent values. Design reference:
// docs/feat/F-32-pi-rpc-bridge.md §2 + docs/feat/F-52-pi-stream-aggregation.md.
//
// All exported behavior follows the agent.Agent contract:
//   - EventAgentDone is per-turn (Reason:"settled") and does NOT close events
//   - EventAgentResult.Text is always non-empty (fallback to "Done.")
//   - Usage is OVERWRITTEN (per-turn snapshot, never summed)
//   - Unknown events / fields → debug log + drop, never terminate
package codexserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
)

// thinkingPrefix marks an EventAgentText as a reasoning block. The
// gateway strips this prefix and routes the payload to OutThinking;
// claudecode / pi bridges use the same sentinel so the channel layer
// needs no per-bridge branching.
const thinkingPrefix = "[思考] "

// emptyReplyFallback is the EventAgentResult text used when a turn
// settles without any un-flushed assistant text. NOT cosmetic — see
// pi/translate.go for the rationale (gateway drops empty Text and
// would take the turn's Usage with it).
const emptyReplyFallback = "Done."

// pendingTool tracks a tool invocation that has emitted item/started
// but not yet the matching item/completed. Re-attached on the end
// event for renderer context (id-stable correlation).
type pendingTool struct {
	Name string
	Args string
}

// turnState is mutable state scoped to a single turn (turn/started →
// turn/completed / turn/failed / thread/status/changed.idle). Reset
// wholesale on each new turn so a future per-turn field cannot be
// forgotten by a reset path.
//
// Concurrency: every field is read / written under turnMu.
type turnState struct {
	// pendingMsgs accumulates agentMessage.text across completed
	// items. Drained by flushPendingMsgsLocked at tool boundaries
	// or on turn/completed. Joined with "\n" on flush.
	pendingMsgs []string

	// thinkBuf accumulates reasoning text. Flushed as one
	// EventAgentText with the thinkingPrefix at reasoning_end
	// (item/completed reasoning).
	thinkBuf strings.Builder

	// pendingTools maps item ID → {name, args} for tool items seen
	// started but not yet completed. Drained on item/completed.
	pendingTools map[string]*pendingTool

	// textDelivered records that at least one EventAgentText for a
	// reply block has gone out this turn. Used to suppress an
	// EventAgentResult.Text that would re-deliver already-shown text.
	textDelivered bool

	// active records the turn observed something worth reporting
	// (any text / tool / usage). A turn with active==false produces
	// NO EventAgentResult / EventAgentDone.
	active bool

	// lastUsage is the per-turn usage snapshot from turn/completed.
	// OVERWRITTEN, never summed.
	lastUsage *agent.UsageInfo

	// doneEmitted guards against the thread/status/changed.idle
	// signal duplicating the same Result + Done pair that
	// turn/completed already emitted. The first signal wins.
	doneEmitted bool
}

func newTurnState() *turnState {
	return &turnState{
		pendingTools: make(map[string]*pendingTool),
	}
}

func (t *turnState) reset() {
	t.pendingMsgs = t.pendingMsgs[:0]
	t.thinkBuf.Reset()
	for k := range t.pendingTools {
		delete(t.pendingTools, k)
	}
	t.textDelivered = false
	t.active = false
	t.lastUsage = nil
	t.doneEmitted = false
}

// translator turns one app-server wire envelope into a slice of
// agent.AgentEvent values. It is pure (no I/O): the session's read
// loop calls into it for every notification.
//
// All emitted events are delivered via session.deliver so the Agent
// can stamp session context (SessionID / Model / AgentName /
// Workspace / Branch) on every event.
type translator struct {
	mu       sync.Mutex
	turn     *turnState
	deliver  func(agent.AgentEvent) agent.AgentEvent
	agentName, workspace, branch string
	stderrTail *ringBuffer
}

func newTranslator(deliver func(agent.AgentEvent) agent.AgentEvent,
	agentName, workspace, branch string,
	stderrTail *ringBuffer) *translator {
	return &translator{
		turn:       newTurnState(),
		deliver:    deliver,
		agentName:  agentName,
		workspace:  workspace,
		branch:     branch,
		stderrTail: stderrTail,
	}
}

// ─── dispatch entrypoints (called by session) ───

// notify is the public dispatch entrypoint used by both session and
// tests. Single switch + per-method handler so the bridge stays
// testable per notification type.
func (t *translator) notify(method string, params json.RawMessage) {
	switch method {
	case "thread/started":
		// thread/started is fired on thread/start or thread/resume and
		// echoes the threadId / model we already saw on the response.
		// No event needs to be emitted (EventAgentReady is synthesised
		// by the Agent after handshake).
	case "turn/started":
		// Reset turn state but do NOT mark active — active is set
		// by per-item handlers when actual content arrives. This
		// ensures an empty turn (turn/started → turn/completed
		// with no items) produces no Result / Done pair.
		t.mu.Lock()
		t.turn.reset()
		t.mu.Unlock()
		case "thread/tokenUsage/updated":
		t.handleTokenUsageUpdated(params)
	case "item/started":
		t.handleItemStarted(params)
	case "item/completed":
		t.handleItemCompleted(params)
	case "turn/completed":
		t.completeTurn(params, "completed")
	case "turn/failed":
		t.completeTurn(params, "failed")
	case "thread/status/changed":
		t.handleThreadStatusChanged(params)
	case "account/rateLimits/updated":
		t.handleRateLimits(params)
	case "error":
		t.handleError(params, t.stderrTail)
	default:
		slog.Default().Debug("codexserver: unknown notification",
			slog.String("method", method))
	}
}

// onNotification is wired into rpcClient as the callback for
// server-pushed notifications. Forwards to translator.notify; tests
// call translator.notify directly.
func (s *session) onNotification(method string, params json.RawMessage) {
	if s.translator == nil {
		slog.Default().Debug("codexserver: notification before translator ready",
			slog.String("method", method))
		return
	}
	s.translator.notify(method, params)
}

// onServerRequest is wired into rpcClient as the callback for frames
// carrying an id AND a method — server-initiated requests that need
// a response on the same id. The actual decision logic lives in
// permissions.go (handleApprovalRequest / handleRequestUserInput /
// handleDynamicToolCall).
func (s *session) onServerRequest(method string, rawID, params json.RawMessage) {
	s.handleServerRequest(method, rawID, params)
}

// handleError translates a wire "error" notification into
// EventAgentError, attaching the captured stderr tail.
func (t *translator) handleError(params json.RawMessage, stderrTail *ringBuffer) {
	var ev struct {
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	_ = json.Unmarshal(params, &ev)
	msg := ev.Message
	if msg == "" {
		msg = "codexserver: server error"
	}
	tail := ""
	if stderrTail != nil {
		tail = stderrTail.String()
	}
	if tail != "" {
		msg = msg + "\n--- stderr tail ---\n" + tail
	}
	t.deliver(agent.AgentEvent{
		Kind: agent.EventAgentError,
		Err:  errors.New(msg),
	})
}

// handleRateLimits is a debug-level capture; nightme has no quota UI
// so we just log the snapshot.
// handleTokenUsageUpdated captures per-turn token usage from the
// codex ≥0.125 wire path. The notification carries BOTH a `last`
// (just-finished turn) and a `total` (cumulative) snapshot. We
// prefer `last` because it matches the per-turn contract documented
// on agent.UsageInfo; falling back to `total` is only a defensive
// measure for older / malformed envelopes.
//
// The handler is called before turn/completed or
// thread/status/changed.idle — completeTurn reads t.turn.lastUsage
// as the source of truth, so the ordering is not fragile.
func (t *translator) handleTokenUsageUpdated(params json.RawMessage) {
	var notif appServerTokenUsageNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}
	u := notif.TokenUsage.Last
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		u = notif.TokenUsage.Total
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedInputTokens == 0 {
		return
	}
	info := appServerUsageToUsageInfo(&u)
	if info != nil {
		info.ContextWindow = notif.TokenUsage.ModelContextWindow
		if info.ContextWindow > 0 {
			used := info.InputTokens + info.OutputTokens + info.CacheCreationInputTokens + info.CacheReadInputTokens
			info.ContextWindowPct = float64(used) / float64(info.ContextWindow) * 100
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.turn.lastUsage = info
}

func (t *translator) handleRateLimits(params json.RawMessage) {
	slog.Default().Debug("codexserver: rateLimits updated (ignored)",
		slog.String("raw", string(params)))
}

// ─── item dispatch ───

func (t *translator) handleItemStarted(params json.RawMessage) {
	var notif itemStartedNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		slog.Default().Debug("codexserver: item/started bad json",
			slog.String("err", err.Error()))
		return
	}
	id, typ := itemSplit(notif.Item)
	switch typ {
	case itemTypeAgentMessage:
		// item/started for agentMessage is a hint; the canonical
		// text lands on item/completed. We don't buffer here so a
		// item/started + item/completed pair does not duplicate the
		// text into pendingMsgs.
	case itemTypeReasoning:
		// Reasoning accumulates on item/started (summary or text
		// fields may both arrive incrementally); flushed on
		// item/completed.
		var p struct {
			Summary string `json:"summary"`
			Text    string `json:"text"`
		}
		if err := json.Unmarshal(notif.Item, &p); err == nil {
			t.mu.Lock()
			if p.Summary != "" {
				t.turn.thinkBuf.WriteString(p.Summary)
			}
			if p.Text != "" {
				t.turn.thinkBuf.WriteString(p.Text)
			}
			t.turn.active = true
			t.mu.Unlock()
		}
	case itemTypeCommandExecution, itemTypeFileChange, itemTypeWebSearch,
		itemTypeMCPToolCall, itemTypeDynamicToolCall:
		// Flush any pending assistant text BEFORE we open a tool,
		// so the user sees one EventAgentText per "narration
		// segment preceding a tool call" — exactly the F-52
		// contract.
		t.mu.Lock()
		t.flushPendingMsgsLocked()
		t.mu.Unlock()

		// Emit the tool start event.
		name, args := decodeToolItem(typ, notif.Item)
		t.deliver(agent.AgentEvent{
			Kind: agent.EventAgentToolStart,
			ToolStart: &agent.AgentToolStartEvent{
				ID:   id,
				Name: name,
				Args: args,
			},
		})
		// Track for end-event correlation; mark active so a
		// tool-only turn still produces a Result on turn/completed.
		t.mu.Lock()
		t.turn.pendingTools[id] = &pendingTool{Name: name, Args: args}
		t.turn.active = true
		t.mu.Unlock()
	default:
		slog.Default().Debug("codexserver: item/started ignored",
			slog.String("type", typ))
	}
}

func (t *translator) handleItemCompleted(params json.RawMessage) {
	var notif itemCompletedNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		slog.Default().Debug("codexserver: item/completed bad json",
			slog.String("err", err.Error()))
		return
	}
	id, typ := itemSplit(notif.Item)
	switch typ {
	case itemTypeAgentMessage:
		// Append to pending; do not flush yet.
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(notif.Item, &p); err == nil && p.Text != "" {
			t.mu.Lock()
			t.turn.pendingMsgs = append(t.turn.pendingMsgs, p.Text)
			t.turn.active = true
			t.mu.Unlock()
		}
	case itemTypeReasoning:
		// Reasoning is delivered on item/completed because the
		// `reasoning/summaryTextDelta` / `reasoning/textDelta`
		// streams are opted out.
		t.mu.Lock()
		text := strings.TrimSpace(t.turn.thinkBuf.String())
		t.turn.thinkBuf.Reset()
		t.mu.Unlock()
		if text != "" {
			t.deliver(agent.AgentEvent{
				Kind: agent.EventAgentText,
				Text: thinkingPrefix + text,
			})
		}
	case itemTypeCommandExecution, itemTypeFileChange, itemTypeWebSearch,
		itemTypeMCPToolCall, itemTypeDynamicToolCall:
		// Re-attach name + args from the started-side, then emit
		// tool end. Surface the status (failed / declined /
		// interrupted) on AgentEvent.Err so channels render the
		// error icon.
		t.mu.Lock()
		pt := t.turn.pendingTools[id]
		delete(t.turn.pendingTools, id)
		t.mu.Unlock()
		name, output, failed := decodeToolItemEnd(typ, notif.Item, pt)
		ev := agent.AgentEvent{
			Kind:    agent.EventAgentToolEnd,
			ToolEnd: &agent.AgentToolEndEvent{
				ID:     id,
				Name:   name,
				Args:   "",
				Output: output,
			},
		}
		if failed {
			ev.Err = fmt.Errorf("tool %s failed", name)
		}
		t.deliver(ev)
	case itemTypeContextCompaction:
		t.deliver(agent.AgentEvent{
			Kind: agent.EventAgentText,
			Text: "[context 已压缩]",
		})
		// Compaction may fire mid-turn; the F-49 design removed the
		// dedicated event so the runtime is a pure pass-through.
		// Here we keep pendingMsgs / tools intact — the assistant
		// reply being composed survives the cycle.
	default:
		slog.Default().Debug("codexserver: item/completed ignored",
			slog.String("type", typ))
	}
}

// ─── turn end ───

// completeTurn is the single path that emits the per-turn
// EventAgentResult + EventAgentDone pair. It is called from
// turn/completed, turn/failed, AND thread/status/changed.idle (with
// doneEmitted guarding against double emission on the latter).
func (t *translator) completeTurn(params json.RawMessage, status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn.doneEmitted {
		return
	}
	if !t.turn.active {
		// No meaningful activity this turn; skip Result + Done.
		t.turn.doneEmitted = true
		return
	}

	// Capture usage. Two wire paths carry per-turn usage:
	//   - turn/completed.notif.Usage (legacy path)
	//   - thread/tokenUsage/updated.notif.tokenUsage.last
	//     (codex ≥0.125; arrives BEFORE thread/status/changed.idle
	//      on the modern path). handleTokenUsageUpdated stores into
	//      t.turn.lastUsage; we fall back to it when params is nil
	//      (thread/status/changed.idle path) or the legacy path
	//      didn't include a Usage field.
	var usage *agent.UsageInfo
	if params != nil && (status == "completed" || status == "failed") {
		var notif turnCompletedNotification
		if err := json.Unmarshal(params, &notif); err == nil && notif.Usage != nil {
			usage = appServerUsageToUsageInfo(notif.Usage)
		}
	}
	if usage == nil {
		usage = t.turn.lastUsage
	} else {
		t.turn.lastUsage = usage
	}

	// Flush any remaining assistant text as EventAgentText first, so
	// the user sees the closing paragraphs.
	text := t.flushPendingMsgsReturnLocked()

	result := agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:  text,
			Usage: usage,
		},
	}
	if status == "failed" {
		// Surface the failure on the same Result event so channels
		// render the error icon. Parse `error` as a JSON object
		// with a "message" field; fall back to the raw text when
		// the field is missing or not an object.
		var notif turnCompletedNotification
		_ = json.Unmarshal(params, &notif)
		errMsg := decodeTurnError(notif.Error)
		if errMsg == "" {
			errMsg = "codexserver: turn failed"
		}
		result.Err = errors.New(errMsg)
	}
	t.deliver(result)

	t.deliver(agent.AgentEvent{
		Kind: agent.EventAgentDone,
		Done: &agent.AgentDoneEvent{
			ExitCode: 0,
			Reason:   "settled",
			Usage:    usage,
		},
	})
	t.turn.doneEmitted = true
}

// handleThreadStatusChanged processes the codex ≥0.125 thread idle
// signal. Same as a turn/completed for our purposes (idempotent
// against completeTurn via doneEmitted).
func (t *translator) handleThreadStatusChanged(params json.RawMessage) {
	var p struct {
		Status struct {
			Type string `json:"type"`
		} `json:"status"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.Status.Type != "idle" {
		return
	}
	// Pass nil params; completeTurn does not require them for the
	// status==idle path (no usage extraction).
	t.completeTurn(nil, "completed")
}

// ─── helpers ───

// flushPendingMsgsLocked emits any buffered agentMessage text as one
// or more EventAgentText events and clears the buffer. Caller must
// hold t.mu.
func (t *translator) flushPendingMsgsLocked() string {
	return t.flushPendingMsgsReturnLocked()
}

// flushPendingMsgsReturnLocked is the return-the-text variant used
// by completeTurn (so it can populate Result.Text without re-walking
// the buffer).
func (t *translator) flushPendingMsgsReturnLocked() string {
	if len(t.turn.pendingMsgs) == 0 {
		t.turn.textDelivered = true
		return emptyReplyFallback
	}
	text := strings.Join(t.turn.pendingMsgs, "\n")
	t.turn.pendingMsgs = t.turn.pendingMsgs[:0]
	if text != "" {
		t.deliver(agent.AgentEvent{
			Kind: agent.EventAgentText,
			Text: text,
		})
		t.turn.textDelivered = true
	}
	return text
}

// decodeToolItem extracts the human-friendly tool name + args string
// for an item/started notification.
func decodeToolItem(itemType string, raw json.RawMessage) (name, args string) {
	switch itemType {
	case itemTypeCommandExecution:
		var p struct {
			Command string `json:"command"`
			CWD     string `json:"cwd"`
		}
		_ = json.Unmarshal(raw, &p)
		name = "Bash"
		args = p.Command
		if p.CWD != "" {
			args += "\n(in " + p.CWD + ")"
		}
	case itemTypeFileChange:
		var p struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(raw, &p)
		name = "Patch"
		args = p.Reason
	case itemTypeWebSearch:
		var p struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(raw, &p)
		name = "WebSearch"
		args = p.Query
	case itemTypeMCPToolCall:
		var p struct {
			Server string `json:"server"`
			Tool   string `json:"tool"`
			Args   json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(raw, &p)
		name = "mcp:" + p.Server + ":" + p.Tool
		args = string(p.Args)
	case itemTypeDynamicToolCall:
		var p struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(raw, &p)
		name = p.Name
		args = string(p.Args)
	default:
		name = itemType
	}
	return
}

// decodeToolItemEnd extracts the tool name, output summary, and
// failed flag from an item/completed notification. Falls back to
// the corresponding pendingTool for name/args correlation.
func decodeToolItemEnd(itemType string, raw json.RawMessage, pt *pendingTool) (name, output string, failed bool) {
	name = itemType
	if pt != nil {
		name = pt.Name
	}
	switch itemType {
	case itemTypeCommandExecution:
		var p struct {
			Command  string `json:"command"`
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
			ExitCode *int   `json:"exitCode"`
			Status   string `json:"status"`
		}
		_ = json.Unmarshal(raw, &p)
		if p.Status == "failed" || p.Status == "declined" {
			failed = true
		}
		if p.ExitCode != nil {
			output = fmt.Sprintf("exit=%d", *p.ExitCode)
		}
		if p.Stdout != "" {
			output += "\n" + strings.TrimSpace(p.Stdout)
		}
		if p.Stderr != "" {
			output += "\nstderr: " + strings.TrimSpace(p.Stderr)
		}
		output = strings.TrimSpace(output)
	case itemTypeFileChange:
		var p struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &p)
		if p.Status == "failed" || p.Status == "declined" {
			failed = true
		}
		output = p.Status
	case itemTypeWebSearch, itemTypeMCPToolCall, itemTypeDynamicToolCall:
		var p struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &p)
		if p.Status == "failed" {
			failed = true
		}
		output = p.Status
	}
	return
}

// decodeTurnError extracts a human-readable message from the
// `error` field of a turn/completed or turn/failed notification.
// The wire shape varies (string OR {message:string}); we handle
// both and fall back to "" when unparseable.
func decodeTurnError(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try object form first.
	var obj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Message != "" {
		return obj.Message
	}
	// Fallback: raw string (without surrounding quotes).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// appServerUsageToUsageInfo maps app-server's wire shape to the
// internal agent.UsageInfo. Note: codex does not report
// ContextWindow / ContextWindowPct on the turn wire, so those
// fields stay zero (footer omits "(window)" segment alongside X%).
func appServerUsageToUsageInfo(u *appServerUsage) *agent.UsageInfo {
	if u == nil {
		return nil
	}
	return &agent.UsageInfo{
		InputTokens:            u.InputTokens,
		OutputTokens:           u.OutputTokens,
		CacheReadInputTokens:   u.CachedInputTokens,
	}
}
