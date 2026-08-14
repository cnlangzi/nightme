// protocol.go — wire types for the dsh web HTTP+WS protocol.
//
// These shapes are derived from dsh's `packages/host/apiproxy/src/api/*.ts`
// (TS schemas are the wire contract; we mirror them as Go structs).
// We use json.RawMessage liberally where the bridge does not yet
// decode a payload — the translate layer unmarshals on demand to
// stay ahead of upstream additions.
package dsh

import (
	"encoding/json"
)

// ─── HTTP RPC envelope ─────────────────────────────────────────────────

// clientRequest is the envelope we send on every POST /api/{method}.
// The wire format is documented in `packages/host/apiproxy/src/api/rpc.ts`;
// `type` MUST be the literal "client-request" (server validates against
// clientRequestSchema in fetch/handler.ts).
type clientRequest struct {
	Type    string          `json:"type"`              // always "client-request"
	RPCID   string          `json:"rpcId"`             // UUID we mint; echoed in response
	Method  string          `json:"method"`            // e.g. "session.prompt"
	Payload json.RawMessage `json:"payload,omitempty"` // method-specific body
}

// rpcResponse is the envelope we receive from every POST /api/{method}.
// `type` is always "server-response"; rpcId echoes our request id;
// `result` is one of OK(value) or Err(error).
type rpcResponse struct {
	Type   string     `json:"type"`
	RPCID  string     `json:"rpcId"`
	Result rpcResult  `json:"result"`
}

// rpcResult is the inner envelope of rpcResponse.result. Schema:
//   { "ok": true,  "value": <method-specific> }
//   { "ok": false, "error": { "code": "...", "message": "...", "details": {...} } }
type rpcResult struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value,omitempty"`
	Error *rpcError       `json:"error,omitempty"`
}

// ErrorMessage returns a one-line human-readable error string for
// surfacing on EventAgentError or wrapping into a Go error.
// Returns "" when the result is OK or has no Error payload (the
// latter shouldn't happen in practice but defensive).
func (r *rpcResult) ErrorMessage() string {
	if r == nil || r.Error == nil {
		return ""
	}
	return r.Error.Code + ": " + r.Error.Message
}

// rpcError is the bridge's view of a server-side business error.
// `code` strings are enumerated in
// `packages/host/apiproxy/src/api/rpc.ts: RpcErrorDetailsMap` — we
// keep the string opaque and forward it to the user on
// EventAgentError.
type rpcError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

// respondRequest is the payload for POST /api/respond — the only
// "API method" that doesn't go through the unary dispatch table but
// is the answer channel for server-initiated frames (approval /
// question). See fetch/handler.ts and the approval/requested mux
// frame below.
type respondRequest struct {
	Type    string          `json:"type"`   // "client-request"
	RPCID   string          `json:"rpcId"`  // we mint
	Method  string          `json:"method"` // "respond"
	Payload respondPayload  `json:"payload"`
}

// respondPayload is the inner body for the /api/respond call.
// `RpcID` here is the **server-frame's rpcId** (the approval/requested
// or question/requested we received), NOT our client's rpcId.
type respondPayload struct {
	RPCID   string          `json:"rpcId"`
	Outcome json.RawMessage `json:"outcome"`
}

// ─── WS server-request envelope ─────────────────────────────────────────

// serverFrame is the envelope server pushes on both /api/events.mux
// and /api/events.host. `type` is always "server-request";
// `method` is the event name (e.g. "session/event", "approval/requested").
type serverFrame struct {
	Type    string          `json:"type"`
	RPCID   string          `json:"rpcId"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload"`
}

// ─── Mux payload shapes (the ones we decode) ──────────────────────────

// muxSessionEvent is the payload of serverFrame{method:"session/event"}.
// `Event` is a raw SessionEvent (42+ types from
// `packages/core/session/src/types.ts`); we unmarshal inside the
// translator per Type.
type muxSessionEvent struct {
	SessionID string          `json:"sessionId"`
	Event     json.RawMessage `json:"event"`
	View      json.RawMessage `json:"view,omitempty"` // host-computed ToolEventView, optional
}

// muxSessionSubscribed is the payload of the per-session baseline frame
// delivered on stream open. We record `lastSeq` as the resume marker
// (used by session.history?sinceSeq when reconnecting).
type muxSessionSubscribed struct {
	SessionID string `json:"sessionId"`
	LastSeq   int64  `json:"lastSeq"`
}

// muxApprovalRequested is the payload of
// serverFrame{method:"approval/requested"}. `ApprovalID` is the
// **stable** id for routing SendPermission answers — we use it
// (not rpcId) so concurrent approvals stay unambiguous.
type muxApprovalRequested struct {
	SessionID  string `json:"sessionId"`
	ApprovalID string `json:"approvalId"`
	ToolName   string `json:"toolName"`
	CallID     string `json:"callId,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// muxApprovalResolved is the audit frame for an already-resolved
// approval. We log it (debug) and never act.
type muxApprovalResolved struct {
	SessionID  string `json:"sessionId"`
	ApprovalID string `json:"approvalId"`
	Outcome    string `json:"outcome"` // "approved" | "declined" | ...
}

// muxQuestionRequested is the payload of
// serverFrame{method:"question/requested"}. `Questions` is an array
// of AskUserQuestionItem (shape from `packages/user-questions/types.ts`).
// We only decode the surface fields we surface to nightme.
type muxQuestionRequested struct {
	SessionID string            `json:"sessionId"`
	Questions []questionPayload `json:"questions"`
}

// questionPayload is one entry in muxQuestionRequested.Questions.
// `Header` is the short label; `Question` is the full text;
// `Options` are the multiple-choice labels the model expects back.
type questionPayload struct {
	Header   string   `json:"header"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Multi    bool     `json:"multiSelect,omitempty"`
}

// ─── Mux session/event SessionEvent shapes (the 11 we decode) ────────

// sessionEventEnvelope is the common envelope of all SessionEvent
// variants. We unmarshal `Type` first then dispatch on it; payload
// is kept as RawMessage and unmarshaled per-Type.
type sessionEventEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"-"`
}

// assistantChunkEvent corresponds to event type "assistant/chunk".
// Carries a single delta token; the translator accumulates into
// textBuf[contentIndex] (F-52 buffering; mirror pi/translate.go).
type assistantChunkEvent struct {
	MessageID    string `json:"messageId"`
	ContentIndex int    `json:"contentIndex"`
	Delta        string `json:"delta"`
}

// assistantMessageEvent corresponds to event type "assistant/message".
// One complete committed message — flush point for pendingText.
// Image / file blocks degrade to text annotations on the bridge
// side (dsh web's baseline-only protocol doesn't carry inline
// images).
type assistantMessageEvent struct {
	MessageID string             `json:"messageId"`
	Content   []contentBlockDTO `json:"content"`
}

// contentBlockDTO is one element of assistantMessageEvent.Content.
// Type is "text" | "image" | "file" — see dsh-llm ContentBlock.
type contentBlockDTO struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Path     string `json:"path,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
}

// toolCallEvent corresponds to event type "tool/call". We record
// Name+Args in pendingTools[ID] so toolResultEvent can backfill.
type toolCallEvent struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Args       string `json:"args"`
	MessageID  string `json:"messageId,omitempty"`
}

// toolResultEvent corresponds to event type "tool/result".
type toolResultEvent struct {
	ToolCallID string `json:"toolCallId"`
	Result     string `json:"result"`
	IsError    bool   `json:"isError,omitempty"`
	MessageID  string `json:"messageId,omitempty"`
}

// turnStartEvent corresponds to event type "turn/start". The
// translator uses this to mark the turn as "active" so empty
// agent_settled doesn't synthesize a "Done." result card.
type turnStartEvent struct {
	TurnID string `json:"turnId"`
}

// turnEndEvent corresponds to event type "turn/end". This is the
// canonical F-52 turn-end signal: emit EventResult{Usage} +
// EventDone{Reason:"settled"} in order.
type turnEndEvent struct {
	TurnID    string     `json:"turnId"`
	StopReason string    `json:"stopReason,omitempty"`
	Usage     *usageInfo `json:"usage,omitempty"`
}

// usageInfo mirrors dsh's LLM usage payload. We decode the minimum
// fields nightme cares about for footer rendering.
type usageInfo struct {
	InputTokens          int     `json:"inputTokens"`
	OutputTokens         int     `json:"outputTokens"`
	CacheCreationTokens  int     `json:"cacheCreationTokens,omitempty"`
	CacheReadTokens      int     `json:"cacheReadTokens,omitempty"`
	CostUSD              float64 `json:"costUsd,omitempty"`
	ContextWindow        int     `json:"contextWindow,omitempty"`
	ContextWindowPct     float64 `json:"contextWindowPct,omitempty"`
}

// compactionEndEvent corresponds to event type "compaction/end".
// One cycle = one EventCompaction.
type compactionEndEvent struct {
	Reason   string `json:"reason,omitempty"`
	Aborted  bool   `json:"aborted,omitempty"`
}

// todoWriteEvent corresponds to event type "todo/write". Items is
// a full snapshot (last-write-wins), translated to F-38 TaskList.
type todoWriteEvent struct {
	Items []todoItem `json:"items"`
}

// todoItem is one entry in todoWriteEvent.Items. Matches
// `packages/core/session/src/types.ts: TodoItem`.
type todoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // "pending" | "in_progress" | "completed"
}

// approvalAskedEvent corresponds to event type "approval/asked"
// (the session/event channel for approval flows; distinct from
// mux approval/requested which is the server-pushed version).
type approvalAskedEvent struct {
	ToolCallID string   `json:"toolCallId"`
	ToolName   string   `json:"toolName"`
	Action     string   `json:"action"`
	Options    []string `json:"options"`
}

// ─── /api/session.create response ──────────────────────────────────────

// sessionCreateValue is the `value` payload of an OK session.create
// response. We only consume SessionID; the rest are kept as
// RawMessage for audit logs / future fields.
type sessionCreateValue struct {
	SessionID string `json:"sessionId"`
}

// sessionPromptValue is the `value` payload of an OK session.prompt
// response. MessageID is durable; we don't currently act on it
// (events flow via WS, not by polling this id).
type sessionPromptValue struct {
	MessageID string `json:"messageId"`
}
