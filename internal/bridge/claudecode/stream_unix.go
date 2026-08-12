//go:build !windows

package claudecode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
)

// streamState holds per-session scratch state that needs to
// survive across translate() calls. The toolUseArgs map is
// the only field for now — see F-34 review P0-2.
//
// Per Claude Code's stream-json protocol, tool_use blocks live
// in assistant-role messages and tool_result blocks live in
// user-role messages, correlated by tool_use_id. The args map
// MUST be session-scoped (not per-message), so a tool_result in
// user-message N can look up the args recorded when its matching
// tool_use was seen in assistant-message N-1.
//
// streamState is owned by pumpStream (one per session) and
// passed by pointer into each translate() call; translate MUST
// NOT mutate it other than through the documented hooks
// (assistant branch → record args; user branch → read args).
type streamState struct {
	toolUseArgs map[string]string

	// pendingTools is keyed by tool_use_id and records the most
	// recent pending tool call we observed. We keep the full
	// pendingTool (name + raw input JSON) rather than only the
	// input string so that:
	//   (a) the F-34 P0-2 type-aware summary still gets the
	//       args (replacing the prior toolUseArgs map);
	//   (b) the F-38 task tool parser can re-parse the input
	//       after the matching tool_result lands, without
	//       duplicating assistant/user branches.
	//
	// Entries are removed in the user/tool_result branch once
	// the corresponding result has been processed. Bridges MUST
	// NOT keep entries around between turns.
	pendingTools map[string]pendingTool

	// tasks is the provider-session-normalised task list. It is
	// owned by the claudecode bridge (see task.go) and used to
	// emit full AgentTaskListEvent snapshots on EventAgentTaskCreate /
	// EventAgentTaskUpdate. Map key is the provider-assigned task ID.
	tasks     map[string]agent.AgentTaskItem
	taskOrder []string
}

// pendingTool records a tool_use block that has not yet been
// matched with its tool_result. The Name + Input are needed both
// for the generic tool summary (OutToolEnd.Args) and for the
// F-38 task tool parser.
type pendingTool struct {
	Name  string
	Input json.RawMessage
}

// resetTasksForNewTurn clears the bridge's task state so a new
// turn (or resumed session) starts with an empty list. PumpStream
// already creates a fresh streamState per session, but the
// system/init event handler also calls this defensively so
// resumed Claude sessions — where pumpStream may reuse state
// across resumes — begin clean. Lives in stream.go to keep
// all task-map mutation in one place; the body is small enough
// to inline without a separate file hop.
func resetTasksForNewTurn(state *streamState) {
	if state == nil {
		return
	}
	state.tasks = make(map[string]agent.AgentTaskItem)
	state.taskOrder = nil
}

// streamEvent is the on-the-wire JSON shape emitted by Claude Code in
// --output-format stream-json mode. Only the fields we consume are
// modeled; unknown fields are ignored by the decoder (json.Unmarshal is
// permissive by default for extra keys).
//
// Schema source: experimentally verified against Claude Code 2.1.220
// (npm/native install) and cross-referenced with cc-connect's
// internal/agent/claudecode/session.go. The schema is not officially
// documented by Anthropic; we treat unknown fields as forward-compatible.
type streamEvent struct {
	Type      string        `json:"type"`
	Subtype   string        `json:"subtype,omitempty"`
	Message   *assistantMsg `json:"message,omitempty"`
	SessionID string        `json:"session_id,omitempty"`

	// model appears at the top level on system/init and result events
	// (assistant.message.model is the same field on assistant events).
	// extractModel probes both spots; the top-level field is the
	// authoritative source for init events.
	Model string `json:"model,omitempty"`

	// result fields
	Result     string `json:"result,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`

	// conversation_reset only — the id assigned to the freshly
	// cleared transcript. Diagnostic; the authoritative SessionID
	// arrives in the immediately-following system/init.
	NewConversationID string `json:"new_conversation_id,omitempty"`

	// result.usage / result.modelUsage — kept as RawMessage so the
	// decoder is permissive (extra keys / unexpected shapes are dropped
	// silently). decodeUsage / decodeCostUSD shape them into
	// agent.UsageInfo when translate() emits EventUsage.
	Usage      json.RawMessage `json:"usage,omitempty"`
	ModelUsage json.RawMessage `json:"modelUsage,omitempty"`
}

type assistantMsg struct {
	ID      string         `json:"id,omitempty"`
	Role    string         `json:"role,omitempty"`
	Model   string         `json:"model,omitempty"`
	Content []contentBlock `json:"content,omitempty"`
}

// syntheticModel is the model name Claude Code stamps on messages it
// fabricates locally rather than receiving from the API (empty-turn
// placeholders, interrupt notices, API-error surfaces).
const syntheticModel = "<synthetic>"

// noContentPlaceholder is the text Claude Code substitutes for an
// assistant turn that produced no output at all. Local slash commands
// executed over stream-json stdin always land here: the command runs,
// no assistant text is generated, and the CLI still emits one
// assistant message so the transcript stays well-formed.
const noContentPlaceholder = "(no content)"

// isSyntheticNoContent reports whether a text block is Claude Code's
// zero-information placeholder for an empty assistant turn. Both the
// synthetic model marker AND the literal placeholder text are required
// so a real model answering "(no content)" is never swallowed.
//
// Matching is strict (==, no TrimSpace). The contract is: only the
// byte sequence the CLI emits is dropped; whitespace-padded variants
// like " (no content)" or "(no content)\n" flow through to the
// channel — they are vanishingly rare and "be lenient" is the wrong
// failure mode (silent data loss is worse than a stray character).
//
// This is the /new path: AgentSession.New → driver.New writes
// `{"type":"user","message":{"role":"user","content":"/clear"}}` to
// the CLI's stdin, and the CLI answers with conversation_reset +
// system/init + a synthetic "(no content)" assistant message +
// result{result:""}. Only the assistant message has user-visible
// content, and it says nothing — dropping it here keeps /new to a
// single receipt card.
func isSyntheticNoContent(model, text string) bool {
	return model == syntheticModel && text == noContentPlaceholder
}

// contentBlock is the union shape for assistant message content. The
// Type field discriminates; only fields relevant to that type are set.
type contentBlock struct {
	Type string `json:"type"`

	// text / thinking
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (in user-role message)
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Content is the tool's result payload. In Claude Code's
	// stream-json schema this can be a plain JSON string OR a
	// nested array of content blocks (multi-modal). We accept it
	// as RawMessage and stringify at emit time so the renderer
	// can surface a single-line summary in the rolling log.
	Content json.RawMessage `json:"content,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

// pumpStream reads one JSON event per line from r and translates each
// into an agent.AgentEvent on events. It returns when r returns io.EOF
// or a permanent read error.
//
// Malformed lines are logged and skipped (do NOT abort the session —
// Claude Code may emit non-JSON noise during startup banner / hooks).
//
// On normal EOF, a final EventAgentDone is emitted with the captured exit
// code (-1 if not yet observed). The events channel is then closed.
//
// Permissions (AskUserQuestion) are routed through askHandler if non-nil;
// otherwise they fall through to EventAgentPermission with a default set of
// options. See ask.go for the dual-path (tool_use + text fallback)
// detection logic.
//
// agentName + workspace are stamped onto the EventAgentReady payload by
// translate (so channel-layer receipts can render the "Agent ·
// name | cwd · path" foot note). They are immutable for the
// session's lifetime and don't need a mutex.
func pumpStream(r io.Reader, events chan<- agent.AgentEvent, askHandler askHandlerFunc, agentName, workspace, branch string, logger *slog.Logger) {
	scanner := bufio.NewScanner(r)
	// Allow long lines (Claude Code may emit large content blocks).
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	// streamState holds per-session scratch state that needs to
	// survive across translate() calls. The toolUseArgs map is
	// the only field for now — see F-34 review P0-2.
	//
	// Per Claude Code's stream-json protocol, tool_use blocks
	// live in assistant-role messages and tool_result blocks live
	// in user-role messages, correlated by tool_use_id. The args
	// map MUST be session-scoped, not per-message, so a tool_result
	// in user-message N can look up the args recorded when its
	// matching tool_use was seen in assistant-message N-1.
	state := &streamState{
		toolUseArgs:  make(map[string]string),
		pendingTools: make(map[string]pendingTool),
		tasks:        make(map[string]agent.AgentTaskItem),
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if logger != nil {
				logger.Warn("claudecode: invalid json event",
					"err", err,
					"line", truncateForLog(string(line), 200))
			}
			continue
		}

		translate(ev, state, events, askHandler, agentName, workspace, branch, logger)
	}

	if err := scanner.Err(); err != nil {
		if logger != nil {
			logger.Warn("claudecode: stream read error", "err", err)
		}
		events <- agent.AgentEvent{
			Kind:  agent.EventAgentError,
			Err: fmt.Errorf("claudecode stream: %w", err),
		}
	}
}

// translate converts one streamEvent into AgentEvent(s) on events.
// Returns nothing — events that don't map to AgentEvent (e.g. unknown
// type) are silently dropped (logged at debug).
//
// agentName + workspace are stamped onto the EventAgentReady payload so
// channel-layer receipts can render the "Agent · name | cwd · path"
// foot note. Both are immutable for the session's lifetime.
func translate(ev streamEvent, state *streamState, events chan<- agent.AgentEvent, askHandler askHandlerFunc, agentName, workspace, branch string, logger *slog.Logger) {
	switch ev.Type {
	case "system":
		// system/init is informational; we surface it via EventAgentReady
		// so the channel can echo a "session <id> · model <name>"
		// header AND have access to SessionID for /resume. Other
		// subtypes (e.g. status, hook_progress) are ignored.
		if ev.Subtype == "init" {
			// F-38: a fresh Claude session means a fresh task
			// list. Clear any prior tasks so the next TaskCreate
			// emits a clean snapshot rather than a stale union.
			// pumpStream spawns a new streamState per session
			// (cold path), so this is also a belt-and-braces guard
			// for the resume path where streamState is reused.
			resetTasksForNewTurn(state)
			events <- agent.AgentEvent{
				Kind:      agent.EventAgentReady,
				SessionID: ev.SessionID,
				Model:     extractModel(ev),
				AgentName: agentName,
				Workspace: workspace,
				Branch:    branch,
			}
		}

	case "assistant":
		if ev.Message == nil {
			return
		}
		for _, block := range ev.Message.Content {
			switch block.Type {
			case "text":
				if block.Text == "" {
					continue
				}
				// Claude Code renders an assistant turn that produced
				// no text as a SYNTHETIC message whose single text
				// block is the literal placeholder "(no content)"
				// (model: "<synthetic>", usage all-zero). The most
				// common producer is a local slash command executed
				// over stream-json stdin — i.e. exactly what
				// driver.New writes for /new ("/clear"). Forwarding
				// it posts a content-free bubble to the chat right
				// after the /new receipt card.
				//
				// Gated on model == "<synthetic>" so a real model
				// that legitimately answers "(no content)" still
				// reaches the user. Other synthetic messages
				// (interrupt notices, API errors) carry real text
				// and are deliberately NOT filtered here.
				if isSyntheticNoContent(ev.Message.Model, block.Text) {
					continue
				}
				// TEXT-FALLBACK (F-24 §5.3): when AskUserQuestion
				// isn't exposed as a tool_use, Claude Code falls back
				// to rendering a markdown question with "please pick
				// one". Detect that pattern and emit EventAgentPermission
				// instead of EventAgentText so the channel renders a
				// proper interactive card.
				if q := detectAskInText(block.Text); q != nil && askHandler != nil {
					emitAskFromText(*q, events, logger)
				} else {
					events <- agent.AgentEvent{
						Kind: agent.EventAgentText,
						Text: block.Text,
					}
				}

			case "thinking":
				// Thinking blocks are surfaced as EventAgentText with a
				// "[思考] " prefix so the channel layer (Feishu
				// Renderer) can render them as a 💭 entry in the
				// per-message activity log. The prefix lets the
				// renderer tell thinking from a final reply and
				// from a system-init blurb ("session initialized
				// (model: …)") which already carries its own
				// context. Empty blocks (Claude Code can emit a
				// zero-length thinking delta) are skipped.
				if strings.TrimSpace(block.Thinking) == "" {
					continue
				}
				events <- agent.AgentEvent{
					Kind: agent.EventAgentText,
					Text: "[思考] " + block.Thinking,
				}

			case "tool_use":
				// F-34 review P0-2: tool_use blocks live in
				// assistant-role messages; their matching
				// tool_result blocks arrive later in user-role
				// messages. Record the raw input JSON here so
				// the user-role handler can stamp it onto
				// EventAgentToolEnd.Args for the type-aware summary
				// (and so the F-38 task tool parser can re-parse
				// the input after the result lands).
				if state != nil {
					inputCopy := make([]byte, len(block.Input))
					copy(inputCopy, block.Input)
					state.toolUseArgs[block.ID] = string(block.Input)
					state.pendingTools[block.ID] = pendingTool{
						Name:  block.Name,
						Input: inputCopy,
					}
				}
				handleToolUse(block, state, events, askHandler, logger)

			default:
				if logger != nil {
					logger.Debug("claudecode: unknown assistant block type",
						"type", block.Type)
				}
			}
		}

	case "user":
		// user-role messages in stream-json carry two shapes:
		//
		//   (a) tool_result blocks — echoed back so the model sees
		//       its own tool calls. Surface as EventAgentToolEnd with the
		//       tool_use_id stored on the ID field so start/end can
		//       be correlated.
		//
		//   (b) plain text blocks — only emitted when Claude Code is
		//       started with --replay-user-messages, OR by a
		//       third-party bridge that mimics the same shape. Each
		//       becomes an EventAgentText with a "[你] " prefix so the
		//       channel can render the user's own input distinctly
		//       from the assistant's replies.
		//
		//       DefaultArgs no longer passes --replay-user-messages
		//       to Claude Code (F-25 v1.1 rolling-log fix), so this
		//       branch is currently unreachable for the upstream
		//       Claude Code. We keep the parser for defensive
		//       compatibility with legacy sessions / future protocol
		//       revisions.
		if ev.Message == nil {
			return
		}
		for _, block := range ev.Message.Content {
			switch block.Type {
			case "tool_use":
				handleToolUse(block, state, events, askHandler, logger)
			case "tool_result":
				// F-38: route the result through the task tool
				// parser first. A successful task operation emits
				// EventAgentTaskCreate / EventAgentTaskUpdate and suppresses
				// the generic ToolEnd. A failure, unknown shape,
				// or non-task tool falls through to the existing
				// generic ToolEnd emission.
				if handled, taskEv := applyTaskToolResult(state, block, logger); handled {
					if taskEv != nil {
						events <- *taskEv
					}
					continue
				}

				// F-34 review P0-2: look up the raw input
				// JSON that the assistant-role message recorded
				// when the matching tool_use block was seen.
				// Without this correlation the Feishu adapter
				// has no way to render "Read /a/b.go" — only
				// "Read" with empty args.
				var args string
				if state != nil {
					args = state.toolUseArgs[block.ToolUseID]
				}
				events <- agent.AgentEvent{
					Kind: agent.EventAgentToolEnd,
					ToolEnd: &agent.AgentToolEndEvent{
						ID:     block.ToolUseID,
						Name:   block.Name,
						Args:   args,
						Output: stringifyToolResult(block.Content),
					},
				}
			case "text":
				if block.Text == "" {
					continue
				}
				events <- agent.AgentEvent{
					Kind: agent.EventAgentText,
					Text: "[你] " + block.Text,
				}
			}
		}

	case "result":
		// A result event has three sub-shapes — handled in order:
		//
		//   1. subtype "compact" / "compaction" — MID-TURN context
		//      compaction. NOT a turn end: subsequent assistant /
		//      tool events continue the same turn. Emit
		//      EventAgentCompaction and return without EventAgentDone.
		//
		//   2. final assistant reply + token usage — text lives in
		//      ev.Result; usage lives in ev.Usage / ev.ModelUsage.
		//      Both are co-located on the same wire event, so we
		//      emit ONE EventAgentResult with Result.Usage attached. The
		//      runtime accumulates Usage on the same dispatch,
		//      before stamping StatusBar — no separate
		//      EventUsage kind and no buffer needed. We emit even
		//      when Result is empty AND IsError=true so the header
		//      can flip to an error state.
		//
		//   3. terminal — EventAgentDone{ExitCode: 0}. ExitCode stays
		//      zero on the wire; IsError travels on the
		//      EventAgentResult payload instead.
		if ev.Subtype == "compact" || ev.Subtype == "compaction" {
			// F-49 compaction tracking removed: bridges no longer emit a
			// dedicated event for context compaction. Runtime is a
			// pure pass-through; the per-cycle counter / footer 🗜 N
			// rendering was dropped across the runtime.
			return
		}
		// Decode usage once and share the *UsageInfo pointer
		// across EventAgentResult (when text/error makes one) and
		// EventAgentDone. Both events are derived from the same wire
		// envelope so identical pointers reflect that fact and we
		// avoid a redundant JSON round-trip per turn.
		//
		// F-52: Usage rides on AgentDoneEvent so the runtime can read
		// it uniformly from the universal prompt-end signal
		// regardless of whether the bridge emitted a result-
		// bearing EventAgentResult. For one-shot bridges (Claude Code)
		// EventAgentDone here marks process exit; for long-lived
		// bridges it marks turn end.
		usage := decodeUsage(ev.Usage, ev.ModelUsage)
		if ev.Result != "" || ev.IsError {
			result := &agent.AgentResultEvent{
				Text:       ev.Result,
				DurationMs: ev.DurationMs,
				Subtype:    ev.Subtype,
			}
			// Attach usage from the same wire event. The previous
			// design emitted a separate EventUsage here; runtime
			// buffering was needed to re-stamp the OutResult footer.
			// Co-locating the usage on AgentResultEvent removes that
			// path entirely (calc-then-reply invariant now holds by
			// construction — usage IS on the result event).
			result.Usage = usage
			events <- agent.AgentEvent{
				Kind:   agent.EventAgentResult,
				Result: result,
			}
		}
		events <- agent.AgentEvent{
			Kind: agent.EventAgentDone,
			Done: &agent.AgentDoneEvent{
				ExitCode: 0,
				Usage:    usage,
			},
		}

	case "control_request":
		// Claude Code emits control_request (subtype: can_use_tool)
		// when started with --permission-prompt-tool stdio. We
		// currently spawn with --permission-mode bypassPermissions,
		// which bypasses this path entirely — so a control_request
		// reaching us is unexpected. Log at debug for visibility
		// without taking action; the future stdio-permission mode
		// will hook this case to emit EventAgentPermission.
		if logger != nil {
			logger.Debug("claudecode: control_request received (unhandled under bypassPermissions)")
		}

	case "conversation_reset":
		// Claude Code emits this right after a local slash command
		// over stream-json stdin resets the conversation (the
		// /clear path used by /new via driver.New). Carries the
		// new_conversation_id only; the authoritative SessionID +
		// Model arrive in the immediately-following system/init
		// event, which we already wire through EventAgentReady.
		//
		// Log at info (not debug) so a future protocol revision
		// that swaps or drops the matching system/init becomes
		// visible in default logs — without this, a break would
		// silently corrupt /new (no SessionID refresh → next
		// /resume pins to the dead conversation).
		//
		// The id is omitempty in streamEvent so a CLI revision
		// that drops the field decodes to "". Treat absent and
		// present-empty as the same shape: still log the event,
		// just don't claim the reset succeeded by stamping an
		// empty id — an operator looking at the log should be
		// able to tell "CLI did not report an id" apart from
		// "the reset cleared nothing".
		if logger != nil {
			attrs := []any{
				"agent_name", agentName,
				"workspace", workspace,
			}
			if ev.NewConversationID != "" {
				attrs = append(attrs, "new_conversation_id", ev.NewConversationID)
			} else {
				attrs = append(attrs, "new_conversation_id_absent", true)
			}
			logger.Info("claudecode: conversation_reset (post /clear)", attrs...)
		}

	default:
		if logger != nil {
			logger.Debug("claudecode: unknown event type", "type", ev.Type)
		}
	}
}

// handleToolUse routes a tool_use block. AskUserQuestion is intercepted
// via askHandler. Task tools (F-38) record their pending operation
// on the streamState but DO NOT emit anything here — the matching
// tool_result branch is the only place that emits EventAgentTaskCreate /
// EventAgentTaskUpdate, after confirming the operation succeeded.
// Other tools are emitted as EventAgentToolStart.
func handleToolUse(
	block contentBlock,
	_ *streamState,
	events chan<- agent.AgentEvent,
	askHandler askHandlerFunc,
	logger *slog.Logger,
) {
	if block.Name == "AskUserQuestion" && askHandler != nil {
		askHandler(block, events, logger)
		return
	}

	// F-38: task tools intentionally do nothing here. The pending
	// record (state.pendingTools[block.ID]) was already stored by
	// the assistant/tool_use branch; the result is processed in
	// the user/tool_result branch. Forwarding a generic ToolStart
	// here would double the user-visible noise once the result
	// also fails the parser / protocol drift fallback.
	if isTaskToolName(block.Name) {
		return
	}

	// Default: forward as a normal tool start. The channel can render
	// it as "🔧 Read(/tmp/foo.py)" etc.
	inputStr := string(block.Input)
	if len(inputStr) > 500 {
		inputStr = inputStr[:500] + "…"
	}
	events <- agent.AgentEvent{
		Kind: agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{
			ID:   block.ID,
			Name: block.Name,
			Args: inputStr,
		},
	}
}

// extractModel pulls the model name out of a system/init event. The
// field is not consistently located across Claude Code versions; we
// try the most likely spots and fall back to "".
//
// Order of precedence:
//
//  1. ev.Model — top-level field on system/init and result events.
//  2. ev.Message.Model — nested field on assistant events.
func extractModel(ev streamEvent) string {
	if ev.Model != "" {
		return ev.Model
	}
	if ev.Message != nil && ev.Message.Model != "" {
		return ev.Message.Model
	}
	return ""
}

// stringifyToolResult flattens a tool_result's content payload to a
// single-line string for the rolling-log display. Claude Code emits
// the content as either a JSON string (the common case) or a JSON
// array of content blocks (multi-modal). We accept both shapes and
// return a single-line summary that the renderer truncates to its
// own per-entry budget.
//
// The function is best-effort: malformed payloads return a JSON dump
// rather than an empty string, so the user at least sees that
// *something* came back.
func stringifyToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Case 1: content is a plain JSON string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	// Case 2: content is an array of content blocks. Flatten
	// each block's text (or a short representation of non-text
	// blocks) with a newline separator.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var out strings.Builder
		for i, b := range blocks {
			if i > 0 {
				out.WriteString(" | ")
			}
			switch b.Type {
			case "text":
				out.WriteString(b.Text)
			default:
				out.WriteString("[" + b.Type + "]")
			}
		}
		return out.String()
	}
	// Case 3: neither shape — return a compact JSON dump. The
	// renderer will truncate; the user sees something useful.
	return string(raw)
}

// truncateForLog caps a string for log lines so a runaway line does
// not blow up the log file.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// decodeUsage parses the result.usage payload (and the co-located
// result.modelUsage payload, which carries per-model costUSD +
// contextWindow) into a single *agent.UsageInfo. Returns nil
// when the usage payload is empty / malformed / all-zero — the
// caller should NOT emit a meaningless EventAgentResult-with-Usage in
// that case. The decoder is intentionally permissive: zero /
// unknown counts default to zero rather than erroring out.
//
// Combined decode (vs the previous split decodeUsage + decodeCostUSD)
// means the result handler can't forget to wire CostUSD or
// ContextWindow after decoding; one call site, complete payload.
//
// Claude Code schema:
//
//	usage:      {"input_tokens": N, "output_tokens": N,
//	             "cache_creation_input_tokens": N,
//	             "cache_read_input_tokens": N}
//	modelUsage: {"<model>": {"costUSD": 0.012, "contextWindow": N, ...},
//	             ...}
//
// nil usage → nil result (no usage on the wire this turn). nil
// modelUsage → CostUSD=0, contextWindow=0 (still produces a
// populated UsageInfo when the usage payload has any non-zero
// field; pct omitted because we lack the denominator).
//
// The bridge owns the context-window-pct calculation per
// docs/feat/F-45-session-footer.md §1.5 / F-54: it reads
// `modelUsage.<model>.contextWindow` into a bridge-local
// variable, divides the per-turn used tokens by it, and fills
// `UsageInfo.ContextWindowPct`. The window value itself is
// never stored on UsageInfo (F-54 §1.2). The runtime is a
// passive pass-through — it does NOT recompute pct, and it
// does NOT know about Anthropic's model-window conventions.
func decodeUsage(rawUsage, rawModelUsage json.RawMessage) *agent.UsageInfo {
	if len(rawUsage) == 0 {
		return nil
	}
	var u struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	}
	if err := json.Unmarshal(rawUsage, &u); err != nil {
		return nil
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0 {
		// All zero is indistinguishable from "absent" — don't emit
		// a meaningless EventAgentResult-with-Usage.
		return nil
	}
	out := &agent.UsageInfo{
		InputTokens:              int(u.InputTokens),
		OutputTokens:             int(u.OutputTokens),
		CacheCreationInputTokens: int(u.CacheCreationInputTokens),
		CacheReadInputTokens:     int(u.CacheReadInputTokens),
	}
	// `contextWindow` is a bridge-local value (F-54): parsed from
	// `modelUsage[<model>].contextWindow`, used immediately to
	// compute pct, never stored on UsageInfo. Any parse failure
	// or empty payload leaves CostUSD / contextWindow at 0
	// ("not reported" — footer omits the X% segment).
	contextWindow := 0
	if len(rawModelUsage) > 0 {
		var m map[string]struct {
			CostUSD       float64 `json:"costUSD"`
			ContextWindow int     `json:"contextWindow"`
		}
		if err := json.Unmarshal(rawModelUsage, &m); err == nil {
			for _, v := range m {
				if v.CostUSD > 0 {
					out.CostUSD = v.CostUSD
				}
				if v.ContextWindow > 0 {
					contextWindow = v.ContextWindow
				}
			}
		}
	}
	// Doc 1 context-window-pct formula: used / window * 100.
	// Bridge-computed, runtime does NOT recompute (see struct doc).
	// Skipped when either operand is 0 — a zero pct is meaningless
	// and would mislead the footer into rendering "0.0%".
	if contextWindow > 0 {
		used := out.InputTokens + out.OutputTokens +
			out.CacheCreationInputTokens + out.CacheReadInputTokens
		if used > 0 {
			out.ContextWindowPct = float64(used) / float64(contextWindow) * 100
		}
		// F-55: forward the window alongside X% so the footer can
		// render `X% (window)`. Single render-side consumer; the
		// runtime does not recompute / catalog / clamp based on
		// this value — see docs/feat/F-55-footer-show-context-window.md.
		out.ContextWindow = contextWindow
	}
	return out
}

// strings.HasPrefix shim so we don't import strings just for one call.
// (kept as a small helper to avoid unused import when this file is
// trimmed down in future refactors)
var _ = strings.HasPrefix
