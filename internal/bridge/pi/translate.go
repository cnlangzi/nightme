// Pi event -> agent.AgentEvent translation table.
//
// The translator is purely functional: it takes one parsed event
// envelope and returns the AgentEvent stream to feed into the
// session's events channel. It never touches the read loop, the
// process, or the pending registry. This makes it trivial to test
// against a JSON fixture per the F-32 acceptance matrix §9.2.
//
// Translation rules are documented in docs/feat/F-32-pi-rpc-bridge.md
// §2.3. Anything not explicitly listed there is dropped (logged at
// debug) so unknown upstream events do not terminate the session.

package pi

import (
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
)

// pendingTool records a tool_call that has emitted tool_execution_start
// but not yet the matching tool_execution_end. The bridge uses it to
// re-attach Name + raw Args on the end event — Pi's wire does not echo
// args on end, so without start->end correlation the renderer would
// fall back to "🔧 tool" and lose the type-aware summary.
//
// Entries are added on tool_execution_start and removed on
// tool_execution_end; agent_settled clears the map defensively to
// prevent leakage across turns when an end was missed.
type pendingTool struct {
	Name string
	Args string // raw JSON from tool_execution_start.args
}

// translator carries the per-session translation state that is
// independent of the protocol layer: the current session
// fingerprint (used to attribute EventInit), and a couple of flags
// for sticky behavior (have we already emitted EventInit; has the
// text stream already ended for the current assistant message).
type translator struct {
	agentName string
	workspace string
	branch    string

	// initSent is set after the first EventInit has been emitted
	// for the session, so subsequent get_state responses or model
	// changes do not re-fire the receipt header.
	initSent bool

	// sawTextEnd tracks whether the most recent text block on the
	// current assistant message has already produced a text_end.
	// We use it to suppress duplicate EventResult payloads when
	// message_end echoes a content[] we have already streamed as
	// deltas. Per-turn; reset when agent_settled fires.
	sawTextEnd bool

	// pendingTools keys in-flight tool calls (toolCallId) to their
	// start-time metadata so the matching tool_execution_end can
	// carry Name + Args into ToolEndEvent. Populated in
	// tool_execution_start, drained in tool_execution_end,
	// wholesale-cleared on agent_settled (defensive).
	//
	// Concurrency: translate() runs from readPump; session.New()
	// (called on /new) reassigns pendingTools from a different
	// goroutine. pendingMu serialises the map so the assignment
	// in New() cannot race against a read in translate(), which
	// would otherwise panic the daemon ("concurrent map read and
	// map write"). PendingMu is intentionally separate from
	// translatorMu (which guards initSent / emitInit) — the two
	// protect disjoint state and never need to be acquired
	// together, so there's no ordering risk.
	pendingMu     sync.Mutex
	pendingTools  map[string]pendingTool
}

func newTranslator(agentName, workspace, branch string) *translator {
	return &translator{
		agentName:    agentName,
		workspace:    workspace,
		branch:       branch,
		pendingTools: make(map[string]pendingTool),
	}
}

// translate consumes one Pi event and returns zero or more
// AgentEvent values. The returned slice is empty for events that
// should be dropped silently. A non-nil error indicates a malformed
// payload that the caller should treat as fatal for the session
// (the bridge closes the events channel and emits EventError in
// that case).
func (t *translator) translate(raw []byte, logger *slog.Logger) ([]agent.AgentEvent, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var env eventEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}

	switch env.Type {

	case "agent_start", "agent_end", "turn_start", "turn_end",
		"message_start", "auto_retry_start", "auto_retry_end",
		"summarization_retry_scheduled",
		"summarization_retry_attempt_start",
		"summarization_retry_finished",
		"queue_update", "bash_execution_update":
		// Lifecycle / queue / streaming events that the F-32 MVP
		// does not surface. Logged for debug; never terminate the
		// session.
		logger.Debug("pi event ignored",
			slog.String("type", env.Type),
			slog.String("raw", string(raw)),
		)
		return nil, nil

	case "agent_settled":
		// Turn-end marker. The session stays open; subsequent
		// prompts are still accepted. Reset per-turn sticky state
		// AND defensively clear pendingTools so a missed
		// tool_execution_end (e.g. Pi aborted mid-call) cannot
		// leak the entry across turns and mis-attribute a later
		// tool's args.
		t.sawTextEnd = false
		t.pendingMu.Lock()
		t.pendingTools = make(map[string]pendingTool)
		t.pendingMu.Unlock()
		return []agent.AgentEvent{{
			Kind: agent.EventDone,
			Done: &agent.DoneEvent{ExitCode: 0, Reason: "settled"},
		}}, nil

	case "message_update":
		return t.translateMessageUpdate(raw, logger)

	case "message_end":
		return t.translateMessageEnd(raw, logger)

	case "tool_execution_start":
		var ev toolExecutionStart
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		// Empty toolCallId is rejected at the wire level by Pi, but
		// belt-and-braces: storing under key "" would let two
		// malformed events collapse onto each other and a later
		// empty-id end would inherit the wrong Name/Args. Record
		// only when the id is non-empty; the matching end falls
		// back to wire ToolName. EventToolStart still fires with
		// ID=="" so consumers see the start; the orphan-end
		// fallback path covers the result line.
		if ev.ToolCallID != "" {
			t.pendingMu.Lock()
			t.pendingTools[ev.ToolCallID] = pendingTool{
				Name: ev.ToolName,
				Args: stringOrEmpty(ev.Args),
			}
			t.pendingMu.Unlock()
		} else {
			logger.Warn("pi tool_execution_start with empty toolCallId — pending correlation skipped",
				slog.String("raw", string(raw)))
		}
		return []agent.AgentEvent{{
			Kind: agent.EventToolStart,
			ToolStart: &agent.ToolStartEvent{
				ID:   ev.ToolCallID,
				Name: ev.ToolName,
				Args: stringOrEmpty(ev.Args),
			},
		}}, nil

	case "tool_execution_update":
		// MVP does not surface partial tool results. Logged for
		// debug so users with --verbose can still see streaming
		// progress in the bridge log.
		logger.Debug("pi tool_execution_update ignored",
			slog.String("raw", string(raw)),
		)
		return nil, nil

	case "tool_execution_end":
		var ev toolExecutionEnd
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		// Re-attach Name + Args recorded from tool_execution_start.
		// Fall back to the wire's own toolName when no start event
		// was observed (orphan end — e.g. tool started before the
		// bridge attached, or end arriving before start under a
		// reordered pump). Args stays empty in the fallback path.
		name := ev.ToolName
		args := ""
		if ev.ToolCallID != "" {
			t.pendingMu.Lock()
			if pending, ok := t.pendingTools[ev.ToolCallID]; ok {
				if name == "" {
					name = pending.Name
				}
				args = pending.Args
			}
			delete(t.pendingTools, ev.ToolCallID)
			t.pendingMu.Unlock()
		} else {
			logger.Warn("pi tool_execution_end with empty toolCallId — orphan path, wire ToolName only",
				slog.String("raw", string(raw)))
		}
		return []agent.AgentEvent{{
			Kind: agent.EventToolEnd,
			ToolEnd: &agent.ToolEndEvent{
				ID:     ev.ToolCallID,
				Name:   name,
				Args:   args,
				Output: stringOrEmpty(ev.Result),
				Err:    toolErrorIf(ev.IsError),
			},
		}}, nil

	case "compaction_start":
		// F-49: bridge abstracts the protocol difference. Pi emits a
		// start+end pair per compaction cycle; only the end is
		// surfaced to the runtime as one EventCompaction, so a single
		// Pi cycle bumps the AgentSession counter exactly once.
		// Reasons live in protocol.go's compactionStart.Reason but
		// are intentionally not propagated (no Subtype field after
		// F-49). See docs/feat/F-49-compaction-counter.md §1.3.
		return nil, nil

	case "compaction_end":
		var ev compactionEnd
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		return []agent.AgentEvent{{
			Kind: agent.EventCompaction,
		}}, nil

	case "extension_ui_request":
		// The F-32 MVP does not forward extension UI to the
		// channel. The caller (session.readPump) handles the
		// auto-cancelled response separately so the translator
		// can stay pure.
		logger.Warn("pi extension_ui_request ignored (F-32 MVP)",
			slog.String("raw", string(raw)),
		)
		return nil, nil

	case "extension_error":
		var ev extensionError
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		logger.Warn("pi extension_error",
			slog.String("path", ev.ExtensionPath),
			slog.String("event", ev.Event),
			slog.String("error", ev.Error),
		)
		return nil, nil

	case "state_update":
		// Defensive: the official pi-coding-agent RPC spec (docs/rpc.md)
		// does NOT list state_update as a server-emitted event. The
		// F-34 reset path in pi/session.go New() does NOT rely on this
		// case — it issues new_session + get_state synchronously and
		// pushes the resulting EventInit through deliver().
		//
		// We keep this case so a future pi version that DOES emit
		// state_update (informally) still surfaces the new sessionId
		// to the runtime. Bypasses initSent for the same reason as
		// the canonical path.
		var ev stateUpdate
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		if ev.SessionID == "" {
			logger.Debug("pi state_update ignored (no sessionId)",
				slog.String("raw", string(raw)))
			return nil, nil
		}
		return []agent.AgentEvent{{
			Kind: agent.EventInit,
			Init: &agent.InitEvent{
				SessionID: ev.SessionID,
				Model:     modelDisplay(ev.ModelID, ev.ModelName),
				AgentName: t.agentName,
				Workspace: t.workspace,
				Branch:    t.branch,
			},
		}}, nil

	default:
		// Unknown event. Drop silently with a debug log so a
		// future Pi version can add events without breaking the
		// bridge.
		logger.Debug("pi event unknown",
			slog.String("type", env.Type),
			slog.String("raw", string(raw)),
		)
		return nil, nil
	}
}

// translateMessageUpdate dispatches message_update events to the
// per-delta translator. The nested assistantMessageEvent.type field
// is the discriminator; the surrounding eventEnvelope is discarded.
func (t *translator) translateMessageUpdate(raw []byte, logger *slog.Logger) ([]agent.AgentEvent, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var ev messageUpdateEnvelope
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, err
	}
	var delta assistantMessageEvent
	if err := json.Unmarshal(ev.AssistantMessageEvent, &delta); err != nil {
		return nil, err
	}

	switch delta.Type {
	case "text_delta":
		return []agent.AgentEvent{{Kind: agent.EventText, Text: delta.Delta}}, nil
	case "text_start", "text_end":
		if delta.Type == "text_end" {
			t.sawTextEnd = true
		}
		// No-op events; deltas are sufficient.
		return nil, nil
	case "thinking_delta":
		// Reuse the claudecode "[思考] " prefix convention so the
		// receipt renderer can branch on the leading bracket.
		return []agent.AgentEvent{{
			Kind: agent.EventText,
			Text: "[思考] " + delta.Delta,
		}}, nil
	case "thinking_start", "thinking_end":
		// No-ops; we do not emit anything for these.
		return nil, nil
	case "toolcall_start":
		// toolcall_start precedes tool_execution_start in Pi's
		// event stream. We deliberately do NOT emit EventToolStart
		// here: tool_execution_start carries the canonical name
		// and args, and emitting twice would surface a phantom
		// "starting" line in the receipt between two genuine
		// tool invocations. The first time the renderer hears
		// about a tool id is at tool_execution_start.
		return nil, nil
	case "toolcall_delta":
		// Streaming tool-call argument delta; MVP drops it.
		return nil, nil
	case "toolcall_end":
		// Same rationale as toolcall_start: the canonical start
		// event is tool_execution_start. toolcall_end here would
		// only refresh the args payload, which the renderer
		// would not render until tool_execution_end anyway.
		return nil, nil
	case "start", "done", "error":
		// Lifecycle markers on the assistant message; no event.
		return nil, nil
	default:
		logger.Debug("pi message_update delta unknown",
			slog.String("type", delta.Type),
			slog.String("raw", string(raw)),
		)
		return nil, nil
	}
}

// translateMessageEnd dispatches message_end events by role.
// assistant -> one EventResult (with Usage co-located on the
// ResultEvent payload — see translateAssistantMessage);
// toolResult -> no-op (the matching tool_execution_end already
// produced EventToolEnd).
func (t *translator) translateMessageEnd(raw []byte, logger *slog.Logger) ([]agent.AgentEvent, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var env struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if len(env.Message) == 0 {
		return nil, nil
	}

	// Probe the role with a tiny decode so we can route on it
	// without committing to the heavier struct for the wrong
	// variant.
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
		return t.translateAssistantMessage(msg, logger), nil
	case "toolResult":
		// Already covered by tool_execution_end.
		return nil, nil
	default:
		logger.Debug("pi message_end role unknown",
			slog.String("role", probe.Role),
			slog.String("raw", string(raw)),
		)
		return nil, nil
	}
}

// translateAssistantMessage renders the terminal assistant message
// into a single EventResult with Usage co-located on the
// ResultEvent payload. The data is already paired on Pi's
// `message_end` (assistant role) wire event — emitting two
// separate events would force the runtime to buffer OutResult
// waiting for the late EventUsage, so we keep them together.
// to buffer OutResult to get the footer stamp right. Now they
// arrive together on the same AgentEvent.
//
// We suppress the result when the most recent text block was
// already finalized via text_end, to avoid double-rendering the
// same final text in the receipt.
func (t *translator) translateAssistantMessage(msg assistantMessage, _ *slog.Logger) []agent.AgentEvent {
	var out []agent.AgentEvent

	// Compose the final text from content[].text blocks in order.
	var finalText string
	if !t.sawTextEnd {
		for _, b := range msg.Content {
			if b.Type == "text" && b.Text != "" {
				if finalText != "" {
					finalText += "\n"
				}
				finalText += b.Text
			}
		}
	}

	result := &agent.ResultEvent{
		Text:    finalText,
		IsError: msg.StopReason == "error",
		Subtype: msg.StopReason,
	}
	// Attach usage from the same wire event (message_end carries
	// both Content and Usage on the assistant role). Empty usage
	// blocks are common (synthetic messages) and leaving
	// result.Usage as nil is the documented "no usage reported"
	// signal — runtime simply skips AccumulateUsage that invocation.
	if msg.Usage != nil && (msg.Usage.Total > 0 || (msg.Usage.Cost != nil && msg.Usage.Cost.Total > 0)) {
		usage := agent.UsageEvent{
			InputTokens:          msg.Usage.Input,
			OutputTokens:         msg.Usage.Output,
			CacheReadInputTokens: msg.Usage.CacheRead,
		}
		// Pi does not separate cache_creation from cache_write in
		// its user-facing usage block. Map cacheWrite to
		// CacheCreationInputTokens for parity with claudecode.
		usage.CacheCreationInputTokens = msg.Usage.CacheWrite
		if msg.Usage.Cost != nil {
			usage.CostUSD = msg.Usage.Cost.Total
		}
		result.Usage = &usage
	}

	out = append(out, agent.AgentEvent{
		Kind:   agent.EventResult,
		Result: result,
	})

	return out
}

// emitInit is invoked from session.go after the get_state handshake
// succeeds. It is independent of the per-event translate path
// because EventInit is driven by a response, not an event.
func (t *translator) emitInit(result *getStateResult) []agent.AgentEvent {
	if t.initSent {
		return nil
	}
	t.initSent = true
	modelID := ""
	modelName := ""
	if result != nil && result.Model != nil {
		modelID = result.Model.ID
		modelName = result.Model.Name
	}
	sessionID := ""
	if result != nil {
		sessionID = result.SessionID
	}
	return []agent.AgentEvent{{
		Kind: agent.EventInit,
		Init: &agent.InitEvent{
			SessionID: sessionID,
			Model:     modelDisplay(modelID, modelName),
			AgentName: t.agentName,
			Workspace: t.workspace,
			Branch:    t.branch,
		},
	}}
}

// stringOrEmpty renders a json.RawMessage as a string for the
// ToolStartEvent.Args / ToolEndEvent.Output fields, which both
// carry a free-form text payload in the agent package.
func stringOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

// toolErrorIf turns a boolean isError flag into a non-nil error
// suitable for ToolEndEvent.Err. We do not include the tool's
// stdout text in the error; channels render Err string when
// non-nil and ignore Output.
func toolErrorIf(isError bool) error {
	if !isError {
		return nil
	}
	return &piToolError{}
}

// piToolError is a sentinel error type so the renderer can detect
// "tool failed" without string-comparing the wrapped cause.
type piToolError struct{}

func (e *piToolError) Error() string { return "pi: tool execution failed" }

// modelDisplay composes a human-readable model label for EventInit
// from Pi's id and name. We prefer "name (id)" when both are
// present so the receipt shows the friendly name and the receipt
// log line keeps the id for grep.
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
