// omp event -> agent.AgentEvent translation table.
//
// Like the pi bridge, the translator is purely functional from
// the caller's point of view: it takes one parsed event envelope
// and returns the AgentEvent stream to feed into the session's
// events channel. It never touches the read loop, the process,
// or the pending registry.
//
// Translation rules (per issue #443 R5/R6/R7/R8):
//
//	ready                       — handshake-only (consumed in
//	                             agent.go before translate is
//	                             ever called; never reaches here)
//	message_start{user}         — no event (echo suppression)
//	message_start{assistant}    — no event
//	message_end{user}           — no event (echo suppression)
//	message_end{assistant}      — record usage + model
//	                             (overwriting snapshot)
//	message_update              — dispatch per inner type
//	                             (text_start/text_delta/
//	                             text_end/thinking_start/
//	                             thinking_delta/thinking_end)
//	tool_execution_start        — EventAgentToolStart
//	                             (title prefers intent)
//	tool_execution_end          — EventAgentToolEnd
//	turn_start / turn_end       — no event, no settle
//	agent_start / agent_end     — no event, no settle
//	                             EXCEPT agent_end{isTerminal:true}
//	                             which is the sole turn-end signal
//	extension_ui_request        — ignored (omp does not block
//	                             on no-response)
//	advisor_cost_changed /      — ignored
//	available_commands_update /
//	prompt_result /
//	session_settled
//	ANYTHING else               — ignored, must NOT break the
//	                             text buffer
//
// F-52-style stream aggregation: text_delta accumulates in a
// per-contentIndex textBuf; thinking_delta accumulates in a
// per-contentIndex thinkBuf. text_end moves textBuf into
// pendingText; thinking_end emits EventAgentThinking
// immediately. flushPendingTextLocked runs at tool boundaries;
// finishTurnLocked runs on agent_end{isTerminal:true}.

package omp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cnlangzi/nightme/internal/agent"
)

// pendingTool records a tool_call that has emitted
// tool_execution_start but not yet the matching
// tool_execution_end.
type pendingTool struct {
	Name    string
	Intent  string
	ArgsRaw string
	Started bool
}

// turnState is every piece of mutable state scoped to a single
// turn (prompt -> ... -> agent_end{isTerminal:true}). Replaced
// wholesale on each terminal event.
//
// Concurrency: all fields are read and written under
// translator.turnMu. See the comment on that field for who
// contends.
type turnState struct {
	// textBuf accumulates text_delta payloads per
	// contentIndex. Keyed rather than a single Builder
	// because omp's wire allows several content blocks to
	// interleave on one message; keying costs nothing and
	// removes an ordering assumption.
	textBuf map[int]*strings.Builder

	// thinkBuf accumulates thinking_delta payloads per
	// contentIndex.
	thinkBuf map[int]*strings.Builder

	// pendingText holds text blocks that have been completed
	// (text_end) but not yet delivered. Drained by either
	// flushPendingTextLocked at a tool boundary or by
	// finishTurnLocked on the agent_end terminal signal.
	pendingText string

	// pendingTools keys in-flight tool calls (toolCallId) to
	// their start-time metadata so the matching
	// tool_execution_end can carry Name + Intent + Args.
	pendingTools map[string]pendingTool

	// lastUsage is the usage block of the most recent
	// assistant message_end.
	//
	// OVERWRITTEN, never summed. Each omp API call reports
	// an input side that already contains the entire
	// conversation history, so it is a snapshot of current
	// context occupancy. Summing the snapshots of a
	// multi-call turn would overstate it.
	lastUsage *agent.UsageInfo

	// lastModel is the model reported on the assistant
	// message_end.
	lastModel string

	// active records that this turn observed something worth
	// reporting. A settled turn that saw none produces no
	// EventAgentResult at all.
	active bool
}

func newTurnState() *turnState {
	return &turnState{
		textBuf:      make(map[int]*strings.Builder),
		thinkBuf:     make(map[int]*strings.Builder),
		pendingTools: make(map[string]pendingTool),
	}
}

// translator carries the per-session translation state.
type translator struct {
	agentName string
	workspace string
	branch    string

	// connectedSent is set after the first EventAgentReady
	// has been emitted for the session, so subsequent
	// get_state responses or model changes do not re-fire
	// the receipt header. Guarded by the session's
	// translatorMu (NOT turnMu).
	connectedSent bool

	// contextWindow is the API-reported model context-window
	// size in tokens, captured from get_state.data.model.
	// contextWindow on the boot handshake. Bridge-local:
	// decodeMessageUsage receives it as a parameter,
	// computes ContextWindowPct, and the value itself never
	// crosses the UsageInfo boundary (except via ContextWindow
	// after F-55 re-introduction).
	contextWindow atomic.Int64

	// turnMu serialises all access to turn state. translate
	// runs on the readPump goroutine; nothing else mutates
	// the turn today, but we keep the mutex for symmetry
	// with pi's translator and to allow a future /new reset
	// to share the same lock.
	turnMu sync.Mutex
	turn   *turnState
}

func newTranslator(agentName, workspace, branch string) *translator {
	return &translator{
		agentName: agentName,
		workspace: workspace,
		branch:    branch,
		turn:      newTurnState(),
	}
}

// translate consumes one omp event and returns zero or more
// AgentEvent values. The returned slice is empty for events
// that should be dropped silently. A non-nil error indicates a
// malformed payload that the caller should treat as fatal for
// the session.
//
// The whole body runs under turnMu.
func (t *translator) translate(raw []byte, logger *slog.Logger) ([]agent.AgentEvent, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var env eventEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}

	t.turnMu.Lock()
	defer t.turnMu.Unlock()

	switch env.Type {

	case "message_start":
		// Both assistant and user message_start are echoes
		// or wire markers — neither produces an event.
		return nil, nil

	case "message_end":
		return t.translateMessageEndLocked(raw, logger)

	case "message_update":
		return t.translateMessageUpdateLocked(raw, logger)

	case "tool_execution_start":
		var ev toolExecutionStart
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		// Flush the narration that preceded this tool call
		// so the user sees what the agent is doing while it
		// runs. Clearing pendingText here is also what
		// guarantees the turn's final EventAgentResult
		// carries only the last segment.
		t.turn.active = true
		out := t.flushPendingTextLocked()
		if ev.ToolCallID != "" {
			t.turn.pendingTools[ev.ToolCallID] = pendingTool{
				Name:    ev.ToolName,
				Intent:  ev.Intent,
				ArgsRaw: stringOrEmpty(ev.Args),
				Started: true,
			}
		} else {
			logger.Warn("omp tool_execution_start with empty toolCallId — pending correlation skipped",
				slog.String("raw", string(raw)))
		}
		// Title prefers intent (the human-readable label
		// omp sends, e.g. "Reading notes.txt"); fall back to
		// toolName when intent is empty.
		title := ev.Intent
		if title == "" {
			title = ev.ToolName
		}
		return append(out, agent.AgentEvent{
			Kind: agent.EventAgentToolStart,
			ToolStart: &agent.AgentToolStartEvent{
				ID:   ev.ToolCallID,
				Name: title,
				Args: stringOrEmpty(ev.Args),
			},
		}), nil

	case "tool_execution_end":
		var ev toolExecutionEnd
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		name := ev.ToolName
		intent := ""
		args := ""
		if ev.ToolCallID != "" {
			if pending, ok := t.turn.pendingTools[ev.ToolCallID]; ok {
				if name == "" {
					name = pending.Name
				}
				intent = pending.Intent
				args = pending.ArgsRaw
			}
			delete(t.turn.pendingTools, ev.ToolCallID)
		} else {
			logger.Warn("omp tool_execution_end with empty toolCallId — orphan path, wire ToolName only",
				slog.String("raw", string(raw)))
		}
		if intent != "" && name == "" {
			name = intent
		}
		return []agent.AgentEvent{{
			Kind: agent.EventAgentToolEnd,
			ToolEnd: &agent.AgentToolEndEvent{
				ID:     ev.ToolCallID,
				Name:   name,
				Args:   args,
				Output: stringOrEmpty(ev.Result),
			},
			Err: toolErrorIf(ev.IsError),
		}}, nil

	case "tool_execution_update":
		// MVP does not surface partial tool results.
		logger.Debug("omp tool_execution_update ignored",
			slog.String("raw", string(raw)))
		return nil, nil

	case "agent_start", "turn_start", "turn_end":
		// Lifecycle / cycle markers that the MVP does not
		// surface. agent_end is handled below as the
		// dedicated terminal-signal case.
		logger.Debug("omp lifecycle event ignored",
			slog.String("type", env.Type),
			slog.String("raw", string(raw)))
		return nil, nil

	case "agent_end":
		// Turn-end marker (when isTerminal != false).
		// omp emits agent_end with `isTerminal:false` for
		// non-terminal loops within one turn; ignore those.
		var probe struct {
			IsTerminal *bool `json:"isTerminal"`
		}
		// An absent isTerminal field is treated as terminal
		// per the R8 contract: any value other than literal
		// false settles the turn.
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, err
		}
		if probe.IsTerminal != nil && !*probe.IsTerminal {
			logger.Debug("omp agent_end (isTerminal:false) ignored",
				slog.String("raw", string(raw)))
			return nil, nil
		}
		out := t.finishTurnLocked()
		out = append(out, agent.AgentEvent{
			Kind: agent.EventAgentDone,
			Done: &agent.AgentDoneEvent{ExitCode: 0, Reason: "settled"},
		})
		t.turn = newTurnState()
		return out, nil

	case "extension_ui_request":
		// omp does not block on no-response. Ignore.
		logger.Debug("omp extension_ui_request ignored (no response)",
			slog.String("raw", string(raw)))
		return nil, nil

	case "advisor_cost_changed", "available_commands_update",
		"prompt_result", "session_settled":
		// Wire-level housekeeping. omp's documented
		// session_settled is informational only (and print-
		// mode does not emit it); prompt_result arrives
		// after agent_end and is ignored to avoid double
		// settling.
		logger.Debug("omp housekeeping event ignored",
			slog.String("type", env.Type),
			slog.String("raw", string(raw)))
		return nil, nil

	default:
		logger.Debug("omp event unknown",
			slog.String("type", env.Type),
			slog.String("raw", string(raw)))
		return nil, nil
	}
}

// translateMessageUpdateLocked dispatches message_update
// events to the per-delta handler. Caller must hold turnMu.
func (t *translator) translateMessageUpdateLocked(raw []byte, logger *slog.Logger) ([]agent.AgentEvent, error) {
	var ev messageUpdateEnvelope
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, err
	}
	var delta assistantMessageEvent
	if err := json.Unmarshal(ev.AssistantMessageEvent, &delta); err != nil {
		return nil, err
	}

	switch delta.Type {
	case "text_start":
		delete(t.turn.textBuf, delta.ContentIndex)
		return nil, nil

	case "text_delta":
		if delta.Delta == "" {
			return nil, nil
		}
		b := t.turn.textBuf[delta.ContentIndex]
		if b == nil {
			b = &strings.Builder{}
			t.turn.textBuf[delta.ContentIndex] = b
		}
		b.WriteString(delta.Delta)
		t.turn.active = true
		return nil, nil

	case "text_end":
		t.closeTextBlockLocked(delta.ContentIndex)
		return nil, nil

	case "thinking_start":
		delete(t.turn.thinkBuf, delta.ContentIndex)
		return nil, nil

	case "thinking_delta":
		if delta.Delta == "" {
			return nil, nil
		}
		b := t.turn.thinkBuf[delta.ContentIndex]
		if b == nil {
			b = &strings.Builder{}
			t.turn.thinkBuf[delta.ContentIndex] = b
		}
		b.WriteString(delta.Delta)
		t.turn.active = true
		return nil, nil

	case "thinking_end":
		b := t.turn.thinkBuf[delta.ContentIndex]
		if b != nil {
			delete(t.turn.thinkBuf, delta.ContentIndex)
			text := strings.TrimSpace(b.String())
			if text != "" {
				return []agent.AgentEvent{{
					Kind: agent.EventAgentThinking,
					Text: text,
				}}, nil
			}
		}
		return nil, nil

	case "start", "done", "error":
		// Message lifecycle markers; no event.
		return nil, nil

	default:
		logger.Debug("omp message_update delta unknown",
			slog.String("type", delta.Type),
			slog.String("raw", string(raw)))
		return nil, nil
	}
}

// translateMessageEndLocked dispatches message_end events by
// role. Caller must hold turnMu.
func (t *translator) translateMessageEndLocked(raw []byte, logger *slog.Logger) ([]agent.AgentEvent, error) {
	var env struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if len(env.Message) == 0 {
		return nil, nil
	}

	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(env.Message, &probe); err != nil {
		return nil, err
	}

	switch probe.Role {
	case "assistant":
		var msg assistantMessage
		if err := json.Unmarshal(env.Message, &msg); err != nil {
			return nil, err
		}
		// Record usage + model; this turn has at least one
		// assistant message so EventAgentResult is warranted
		// even if the message carried no text blocks.
		t.turn.active = true
		if msg.Model != "" {
			t.turn.lastModel = msg.Model
		}
		if u := decodeMessageUsage(msg.Usage, int(t.contextWindow.Load())); u != nil {
			t.turn.lastUsage = u
		}
		return nil, nil
	case "user":
		// User echo. omp's print mode does not appear to
		// surface it; if it does appear in some future
		// version, drop silently.
		return nil, nil
	default:
		logger.Debug("omp message_end role unknown",
			slog.String("role", probe.Role),
			slog.String("raw", string(raw)))
		return nil, nil
	}
}

// closeTextBlockLocked drains the accumulated deltas for one
// content index into pendingText.
func (t *translator) closeTextBlockLocked(idx int) {
	b := t.turn.textBuf[idx]
	if b == nil {
		return
	}
	delete(t.turn.textBuf, idx)
	text := b.String()
	if text == "" {
		return
	}
	if t.turn.pendingText != "" {
		t.turn.pendingText += "\n"
	}
	t.turn.pendingText += text
}

// closeOpenBlocksLocked drains every still-open text block, in
// ascending content-index order so the result is deterministic
// regardless of Go's map iteration order.
func (t *translator) closeOpenBlocksLocked() {
	if len(t.turn.textBuf) == 0 {
		return
	}
	idxs := make([]int, 0, len(t.turn.textBuf))
	for i := range t.turn.textBuf {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		t.closeTextBlockLocked(i)
	}
}

// flushPendingTextLocked delivers the accumulated narration as
// a single EventAgentText and clears it.
func (t *translator) flushPendingTextLocked() []agent.AgentEvent {
	t.closeOpenBlocksLocked()
	text := strings.TrimSpace(t.turn.pendingText)
	t.turn.pendingText = ""
	if text == "" {
		return nil
	}
	return []agent.AgentEvent{{Kind: agent.EventAgentText, Text: text}}
}

// finishTurnLocked builds the turn's single EventAgentResult.
func (t *translator) finishTurnLocked() []agent.AgentEvent {
	t.closeOpenBlocksLocked()

	if !t.turn.active {
		return nil
	}

	text := strings.TrimSpace(t.turn.pendingText)
	t.turn.pendingText = ""

	return []agent.AgentEvent{{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:  text,
			Usage: t.turn.lastUsage,
		},
	}}
}

// decodeMessageUsage converts omp's per-message usage block
// into the canonical agent.UsageInfo. Returns nil when the
// block is absent or reports nothing.
func decodeMessageUsage(u *messageUsage, ctxWindow int) *agent.UsageInfo {
	if u == nil {
		return nil
	}
	costZero := u.Cost == nil || u.Cost.Total == 0
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 && costZero {
		return nil
	}
	out := &agent.UsageInfo{
		InputTokens:              u.Input,
		OutputTokens:             u.Output,
		CacheReadInputTokens:     u.CacheRead,
		CacheCreationInputTokens: u.CacheWrite,
	}
	if u.Cost != nil {
		out.CostUSD = u.Cost.Total
	}
	if ctxWindow > 0 {
		used := u.Input + u.Output + u.CacheRead + u.CacheWrite
		if used > 0 {
			out.ContextWindowPct = float64(used) / float64(ctxWindow) * 100
		}
		out.ContextWindow = ctxWindow
	}
	return out
}

// emitConnected emits the EventAgentReady (only once per
// session, gated by connectedSent).
func (t *translator) emitConnected(result *getStateResult) []agent.AgentEvent {
	if t.connectedSent {
		return nil
	}
	t.connectedSent = true
	t.contextWindow.Store(0)
	modelID := ""
	modelName := ""
	if result != nil && result.Model != nil {
		modelID = result.Model.ID
		modelName = result.Model.Name
		if result.Model.ContextWindow > 0 {
			t.contextWindow.Store(int64(result.Model.ContextWindow))
		}
	}
	sessionID := ""
	if result != nil {
		sessionID = result.SessionID
	}
	return []agent.AgentEvent{{
		Kind:      agent.EventAgentReady,
		SessionID: sessionID,
		Model:     modelDisplay(modelID, modelName),
		AgentName: t.agentName,
		Workspace: t.workspace,
		Branch:    t.branch,
	}}
}

// stringOrEmpty renders a json.RawMessage as a string for
// AgentToolStartEvent.Args / AgentToolEndEvent.Output.
func stringOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

// toolErrorIf turns a boolean isError flag into a non-nil
// error suitable for AgentToolEndEvent.Err.
func toolErrorIf(isError bool) error {
	if !isError {
		return nil
	}
	return &ompToolError{}
}

// ompToolError is a sentinel error type so the renderer can
// detect "tool failed" without string-comparing the wrapped
// cause.
type ompToolError struct{}

func (e *ompToolError) Error() string { return "omp: tool execution failed" }

// modelDisplay composes a human-readable model label for
// EventAgentReady from omp's id and name. We prefer
// "name (id)" when both are present.
func modelDisplay(id, name string) string {
	if id == "" && name == "" {
		return ""
	}
	if name == "" {
		return id
	}
	if id == "" {
		return name
	}
	return name + " (" + id + ")"
}

// ensureFmtError references fmt so the import is kept
// unconditionally (used by other future branches; today the
// placeholder keeps the file compile-clean).
var _ = fmt.Errorf
