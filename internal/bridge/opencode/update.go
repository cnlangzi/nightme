// Package opencode — sessionUpdate → AgentEvent translator.
//
// ACP spec sessionUpdate variants the opencode acp server emits
// (verified against opencode 1.18.x source —
// https://raw.githubusercontent.com/sst/opencode/dev/packages/opencode/src/acp/event.ts):
//
//   - user_message_chunk        (replay of the user message after
//                                session/load; drops because the
//                                chat layer already rendered the
//                                user's input)
//   - agent_message_chunk       → EventAgentText (assistant
//                                text stream)
//   - agent_thought_chunk       → EventAgentText prefixed with
//                                "[思考] " (reasoning / thinking
//                                stream; matches the legacy
//                                opencode-serve bridge's [思考]
//                                convention so the channel
//                                renderer needs no change)
//   - tool_call                 → EventAgentToolStart
//   - tool_call_update          (status=running → log only,
//                                status=completed → EventAgentToolEnd,
//                                status=failed    → EventAgentToolEnd with Err)
//
// All other / unknown sessionUpdate variants are tolerated (logged
// at debug, no event emitted, no readpump abort) so a future
// opencode release that adds new variants does not break the
// bridge.
//
// Note: opencode's acp server does NOT surface usage_update,
// available_commands_update, current_mode_update,
// config_option_update, session_info_update, or plan as ACP
// sessionUpdate notifications today — those are opencode-internal
// events consumed by the opencode process itself. Usage info
// reaches the runtime via the session/prompt response, which the
// generic acp bridge decodes in translatePromptResponse and
// stamps on EventAgentDone.Usage.
//
// The translator is wired via acp.Driver.SetUpdateHandler in
// starter.go::Start, AFTER the generic acp handshake completes
// but BEFORE the readPump observes the first session/update.
// Late registration is racy — see SetUpdateHandler doc.
package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
)

// updateHandler is the opencode-specific ACP sessionUpdate
// translator. One per live *agent.Agent.
//
// textBuf accumulates agent_message_chunk payloads WITHOUT emitting
// per chunk — the chat layer would otherwise render every token as
// its own bubble (and, pre-fix with the opencode bridge in place, render
// each one twice because both the bridge and the generic acp fallback
// emitted EventAgentText). Flushes when:
//
//   - the buffered text ends with a sentence-terminating punctuation
//     (".?!。！？"); the chat channel then renders ONE send_card carrying
//     the accumulated sentence,
//   - a tool_call arrives (tool-boundary — match pi/dsh F-52 contract),
//   - the explicit Flush(view) hook fires (turn-end, via
//     SessionView.FlushPending driven from translatePromptResponse).
//
// State is per-handler (per-driver, per-session); the generic acp bridge
// guarantees FlushPending reaches the same handler instance it
// delivered the chunks to.
type updateHandler struct {
	agentName string
	workspace string

	// textBuf holds the accumulated reply text since the last flush.
	// Reset every time it flushes. A *strings.Builder is value-safe
	// (no aliasing concerns) and supports Grow() if a chunk is unusually
	// large.
	textBuf *strings.Builder
}

// newUpdateHandler constructs the translator. The view passed to
// the returned closure is supplied by the generic acp bridge and
// exposes only the channels / fields the translator needs
// (events emit, session id lookup).
// newUpdateHandler constructs the translator. Returns the concrete
// *updateHandler so callers (starter.go) can install both the UpdateHandler
// closure AND the Flush method via SetUpdateHandler / SetFlushHandler. The
// bridge's extension hooks (acp.DriverHandle) only see an UpdateHandler
// closure — never the concrete — so widening the return type here is safe.
func newUpdateHandler(workspace string) *updateHandler {
	return &updateHandler{
		agentName: bridgeName,
		workspace: workspace,
		textBuf:   &strings.Builder{},
	}
}

// asUpdateHandler adapts the concrete translator to the bridge's
// UpdateHandler type. Used by starter.go after constructing the
// translator so the bridge receives the function signature it
// expects while the starter retains access to Flush.
//
// Returns a fresh closure on each call — no aliasing concerns
// since updateHandler.handle is read-only on its state except
// for h.textBuf, which the bridge serializes via the readPump.
func (h *updateHandler) asUpdateHandler() acp.UpdateHandler {
	return func(view *acp.SessionView, raw json.RawMessage) error {
		return h.handle(view, raw)
	}
}

// Flush is the FlushHandler the generic acp bridge invokes at turn
// boundaries (right before EventAgentDone). Drains whatever is in
// textBuf to the channel as a single EventAgentText then resets the
// buffer. No-op if the buffer is empty.
//
// Wired via acp.DriverHandle.SetFlushHandler in starter.go::Start —
// see F-OPENCODE-ACP-MIGRATION §5.2 (drain trailing text on turn-end).
func (h *updateHandler) Flush(view *acp.SessionView) {
	h.flushText(view)
}

// handle is the dispatch entry point. Reads the sessionUpdate
// discriminator and routes to the per-kind handler. Returns an
// error only on JSON decode failures; wire-level decoding stays
// tolerant of malformed individual updates.
func (h *updateHandler) handle(view *acp.SessionView, raw json.RawMessage) error {
	var head struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return fmt.Errorf("opencode/acp: decode sessionUpdate head: %w", err)
	}
	kind := head.SessionUpdate

	// Tool-boundary flush (F-52 invariant, mirrors pi / dsh). Any
	// non-text agent_message_chunk arriving after a tool-call is a
	// break in the assistant's reply stream; drain whatever the
	// buffer still holds so the user sees the in-progress reply
	// before the tool receipt appears.
	if kind != "agent_message_chunk" && kind != "agent_thought_chunk" && kind != "user_message_chunk" {
		h.flushText(view)
	}

	switch kind {
	// ── text ────────────────────────────────────────────────
	case "user_message_chunk":
		// Replay of the user's input after session/load.
		// Drop — the channel already rendered the inbound
		// message; re-emitting it would duplicate the receipt.
		return nil
	case "agent_message_chunk":
		return h.handleAgentText(view, raw)
	case "agent_thought_chunk":
		return h.handleAgentThought(view, raw)

	// ── tools ───────────────────────────────────────────────
	case "tool_call":
		return h.handleToolCall(view, raw)
	case "tool_call_update":
		return h.handleToolCallUpdate(view, raw)

	// ── unknown / future-proofing ───────────────────────────
	default:
		oLog("sessionUpdate: unknown kind (tolerated)", "kind", kind)
		return nil
	}
}

// ─── text handlers ────────────────────────────────────────────

// handleAgentText emits EventAgentText for an agent_message_chunk
// update. Extracts the text from the ContentChunk content block.
//
// ContentChunk shape (ACP spec):
//
//   { "content": { "type": "text", "text": "..." } }
//
// The "content" envelope is the only field; updates with
// type != "text" (image, resource) are dropped — the chat
// layer does not currently render inline images.
func (h *updateHandler) handleAgentText(view *acp.SessionView, raw json.RawMessage) error {
	text := decodeTextChunk(raw)
	if text == "" {
		return nil
	}
	h.textBuf.WriteString(text)
	// Sentence-boundary flush: surface the buffered reply to the
	// chat channel as soon as the agent has produced a coherent
	// sentence, instead of waiting for the turn-end firehose that
	// would otherwise deliver the full reply in a single send_card
	// at Done-time. Mirrors how pi's flushPendingTextLocked drains
	// at tool boundaries — same idea, different trigger.
	if endsWithSentencePunctuation(h.textBuf.String()) {
		h.flushText(view)
	}
	return nil
}

// handleAgentThought emits EventAgentText prefixed with
// "[思考] " for an agent_thought_chunk. Matches the
// opencode-serve bridge's convention so the channel renderer
// needs no change.
func (h *updateHandler) handleAgentThought(view *acp.SessionView, raw json.RawMessage) error {
	text := decodeTextChunk(raw)
	if text == "" {
		return nil
	}
	view.Emit(agent.AgentEvent{
		Kind:      agent.EventAgentText,
		Text:      "[思考] " + text,
		SessionID: view.SessionID(),
		AgentName: h.agentName,
		Workspace: h.workspace,
	})
	return nil
}

// ─── tool handlers ────────────────────────────────────────────

// handleToolCall emits EventAgentToolStart for a tool_call update
// (initial pending state).
//
// ToolCall shape (ACP spec):
//
//   {
//     "toolCallId": "tc_xxx",
//     "title":      "Bash",
//     "kind":       "execute" | "read" | "edit" | ...,
//     "status":     "pending" | "running" | "completed" | "failed",
//     "content":    [ToolCallContent],
//     "locations":  [ToolCallLocation],
//     "rawInput":   { ... },
//     "rawOutput":  { ... }
//   }
func (h *updateHandler) handleToolCall(view *acp.SessionView, raw json.RawMessage) error {
	var tc struct {
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		RawInput   json.RawMessage `json:"rawInput"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return fmt.Errorf("opencode/acp: decode tool_call: %w", err)
	}
	argsJSON := tc.RawInput
	if len(argsJSON) == 0 {
		argsJSON = json.RawMessage("{}")
	}
	view.Emit(agent.AgentEvent{
		Kind:      agent.EventAgentToolStart,
		SessionID: view.SessionID(),
		AgentName: h.agentName,
		Workspace: h.workspace,
		ToolStart: &agent.AgentToolStartEvent{
			ID:   tc.ToolCallID,
			Name: tc.Title,
			Args: string(argsJSON),
		},
	})
	return nil
}

// handleToolCallUpdate emits EventAgentToolEnd for a
// tool_call_update notification. Status determines the shape:
//
//   completed → ToolEnd with Output = rawOutput (JSON string)
//   failed    → ToolEnd with top-level Err; Output = rawOutput
//               so the user can see the failure detail
//   running   → log only (no EventAgentToolProgress today)
//   pending   → log only (initial state; emits via handleToolCall)
//   other     → log only (tolerated; future opencode versions
//               may add new statuses)
func (h *updateHandler) handleToolCallUpdate(view *acp.SessionView, raw json.RawMessage) error {
	var tc struct {
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		Status     string          `json:"status"`
		RawOutput  json.RawMessage `json:"rawOutput"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return fmt.Errorf("opencode/acp: decode tool_call_update: %w", err)
	}
	switch tc.Status {
	case "completed":
		out := tc.RawOutput
		if len(out) == 0 {
			out = json.RawMessage("{}")
		}
		view.Emit(agent.AgentEvent{
			Kind:      agent.EventAgentToolEnd,
			SessionID: view.SessionID(),
			AgentName: h.agentName,
			Workspace: h.workspace,
			ToolEnd: &agent.AgentToolEndEvent{
				ID:     tc.ToolCallID,
				Name:   tc.Title,
				Output: string(out),
			},
		})
		return nil
	case "failed":
		out := tc.RawOutput
		if len(out) == 0 {
			out = json.RawMessage("{}")
		}
		view.Emit(agent.AgentEvent{
			Kind:      agent.EventAgentToolEnd,
			SessionID: view.SessionID(),
			AgentName: h.agentName,
			Workspace: h.workspace,
			ToolEnd: &agent.AgentToolEndEvent{
				ID:     tc.ToolCallID,
				Name:   tc.Title,
				Output: string(out),
			},
			Err: fmt.Errorf("opencode: tool %s failed: %s", tc.Title, string(out)),
		})
		return nil
	default:
		// "running" / "pending" / future statuses — log
		// only; we don't have an in-progress tool receipt
		// shape today.
		oLog("tool_call_update: status logged only (no event emitted)",
			"tool_call_id", tc.ToolCallID, "title", tc.Title, "status", tc.Status)
		return nil
	}
}

// ─── helpers ──────────────────────────────────────────────────

// decodeTextChunk extracts the text payload from a
// ContentChunk. Returns "" for non-text chunks or empty text —
// the caller drops empty results silently.
//
// ContentChunk shape (ACP spec):
//
//   { "content": { "type": "text" | "image" | ...,
//                  "text": "..." (for type=text) } }
//
// We do not currently route image chunks to EventAgentText;
// future v2 may add an EventAgentImage kind for inline
// rendering.
func decodeTextChunk(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var c struct {
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return ""
	}
	if c.Content.Type != "text" {
		return ""
	}
	return c.Content.Text
}

// flushText drains textBuf to the channel as a single EventAgentText
// and clears the buffer. No-op (no event emitted) when the buffer
// is empty or all-whitespace so callers can fire it unconditionally
// at every tool / unknown / turn-end boundary.
//
// Receives *SessionView (not via a field) so the EventAgentText
// carries the same SessionID / AgentName / Workspace stamps as a
// direct emit would. Held under no lock — textBuf is per-handler
// and the generic acp bridge serializes session/update dispatches
// through the readPump goroutine.
func (h *updateHandler) flushText(view *acp.SessionView) {
	if h.textBuf == nil {
		return
	}
	text := strings.TrimSpace(h.textBuf.String())
	h.textBuf.Reset()
	if text == "" {
		return
	}
	view.Emit(agent.AgentEvent{
		Kind:      agent.EventAgentText,
		Text:      text,
		SessionID: view.SessionID(),
		AgentName: h.agentName,
		Workspace: h.workspace,
	})
}

// endsWithSentencePunctuation reports whether s ends with a sentence-
// terminating punctuation mark — ASCII ".!?" or full-width
// "。！？" — after trimming trailing whitespace. The set matches the
// pi / dsh bridges' delimiter convention so the channel renders all
// three backends identically; an extendable future can pass a custom
// set without rewiring the call site (this helper is the single
// source of truth for "sentence ended").
func endsWithSentencePunctuation(s string) bool {
	if s == "" {
		return false
	}
	for i := len(s) - 1; i >= 0; i-- {
		r, size := utf8.DecodeLastRuneInString(s[:i+1])
		if size == 0 {
			return false
		}
		if !unicode.IsSpace(r) {
			switch r {
			case '.', '?', '!', '。', '！', '？':
				return true
			default:
				return false
			}
		}
	}
	return false
}
