// protocol.go — wire types for the dsh web HTTP+WS protocol.
//
// These shapes are derived from dsh's `packages/host/apiproxy/src/api/*.ts`
// (TS schemas are the wire contract; we mirror them as Go structs).
// We use json.RawMessage liberally where the bridge does not yet
// decode a payload — the translate layer unmarshals on demand to
// stay ahead of upstream additions.
//
// IMPORTANT (2026-08-14): every session/event payload on the mux
// channel arrives wrapped under a top-level `data` object whose
// shape is type-specific (assistant/chunk puts the streaming
// chunk under `data.chunk`, assistant/message puts the complete
// message under `data.message`, tool/call flattens its fields into
// `data.{callId,name,arguments}`). The sessionEventEnvelope
// exposes that envelope as `Data json.RawMessage` so each case
// in translate.go can decode the type-specific payload without
// ambiguously re-unmarshalling the wire envelope. The previous
// version decoded per-type payloads directly from the envelope
// and silently produced zero-value fields for every event
// (assistant/message Content was always nil, tool/call callId
// always empty, …), which made the bridge a black box.
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

// sessionEventEnvelope is the common shape every mux session/event
// carries: a `type` discriminator plus a `data` object whose
// shape depends on Type. Decoders ignore everything except Type
// and Data; the per-type data structs (assistantChunkData,
// assistantMessageData, toolCallData, …) are unmarshalled FROM
// Data, never from the envelope itself.
//
// Captured wire shape (real, from the 2026-08-14
// session-3dbe433e-* lifecycle that exposed the schema bug):
//
//	{
//	  "type": "assistant/message",
//	  "data": {
//	    "turn": 1, "step": 1,
//	    "message": { "role": "assistant", "content": [ … ] }
//	  }
//	}
type sessionEventEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// assistantChunkData is the `data` body of an assistant/chunk event.
// F-52 contract: chunks are streaming deltas; the per-text-block
// accumulation lives in translator.textBuf, keyed by Chunk.Index.
// Chunk.Type discriminates the sub-shape ("text-delta",
// "tool-call-delta", "block-end", "block-start", "usage", "finish").
type assistantChunkData struct {
	Turn  int            `json:"turn"`
	Step  int            `json:"step"`
	Chunk assistantChunk `json:"chunk"`
}

// assistantChunk is one assistant/chunk's `data.chunk` body. Field
// presence depends on Chunk.Type:
//
//   - "block-start":     BlockType carries "text" | "tool-call" | …
//   - "block-delta":     reserved for future streaming block updates
//                        (current dsh builds only emit text-delta /
//                        tool-call-delta)
//   - "block-end":       Block carries the complete finalized block
//   - "text-delta":      Text carries the streaming fragment,
//                        Index is the content-block slot
//   - "tool-call-delta": ID + Name + ArgumentsDelta carry a partial
//                        tool-call accumulator
//   - "usage":           Usage carries the model's usage row
//   - "finish":          Reason carries the step-finish discriminator
type assistantChunk struct {
	Type           string            `json:"type"`
	Index          int               `json:"index,omitempty"`
	BlockType      string            `json:"blockType,omitempty"`
	Text           string            `json:"text,omitempty"`
	ArgumentsDelta string            `json:"argumentsDelta,omitempty"`
	ID             string            `json:"id,omitempty"`
	Name           string            `json:"name,omitempty"`
	Block          *assistantBlock   `json:"block,omitempty"`
	Usage          *usageInfo        `json:"usage,omitempty"`
	Reason         *chunkFinishReason `json:"reason,omitempty"`
}

// assistantBlock is the `block` payload of a `block-end` chunk —
// the complete block the model emitted in this content slot.
// Type discriminator: "text" (Text populated) | "tool-call"
// (ID/Name/Arguments populated) | other (audit-only).
type assistantBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// chunkFinishReason is the `reason` payload of a `finish` chunk.
// Examples observed on the wire:
//
//	{"kind":"tool-calls"}  — model produced tool calls
//	{"kind":"stop"}        — natural model stop
//	{"kind":"length"}      — truncated by maxTokens
type chunkFinishReason struct {
	Kind string `json:"kind"`
}

// assistantMessageData is the `data` body of an assistant/message
// event. The complete message — content blocks + role — lives at
// `data.message.content[]`. Text blocks have `{"type":"text",
// "text":"…"}`; tool-call blocks have
// `{"type":"tool-call","id":"…","name":"…","arguments":"…"}`.
//
// `data.message.content[]` is the F-52 flush input: the translator
// picks the text blocks and emits EventAgentText at this boundary.
type assistantMessageData struct {
	Turn    int                      `json:"turn"`
	Step    int                      `json:"step"`
	Message assistantMessageEnvelope `json:"message"`
	Usage   *usageInfo               `json:"usage,omitempty"`
}

// assistantMessageEnvelope wraps one committed assistant message.
type assistantMessageEnvelope struct {
	Role    string            `json:"role"`
	Content []contentBlockDTO `json:"content"`
}

// contentBlockDTO is one element of an assistant message's
// `content[]` (text blocks, tool-call blocks) or a tool-result's
// nested content.
//
// Type vocabulary (verified against dsh wire captures):
//
//	"text"        — Text populated, the model spoke.
//	"tool-call"   — ToolCallID/Name/Args populated, model wanted a tool.
//	"tool-result" — ToolCallID + IsError + nested Content (a
//	                 tool-result envelope wrapping a list of
//	                 text/image blocks under content.content).
//	"image"       — Path/MediaType populated for outbound (we
//	                 decode but the bridge doesn't currently use
//	                 the fields for inbound rendering).
type contentBlockDTO struct {
	Type       string            `json:"type"`
	Text       string            `json:"text,omitempty"`
	Path       string            `json:"path,omitempty"`
	MediaType  string            `json:"mediaType,omitempty"`
	ToolCallID string            `json:"id,omitempty"`
	Name       string            `json:"name,omitempty"`
	Arguments  string            `json:"arguments,omitempty"`
	IsError    bool              `json:"isError,omitempty"`
	Content    []contentBlockDTO `json:"content,omitempty"`
}

// toolCallData is the `data` body of a tool/call event. Fields
// are FLATTENED into the `data` object (not nested under a message
// sub-object): `data.callId`, `data.name`, `data.arguments`. The
// translator decodes these to populate pendingTools[id] so the
// matching tool/result can backfill Name + Args.
type toolCallData struct {
	Turn      int    `json:"turn"`
	Step      int    `json:"step"`
	CallID    string `json:"callId"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// toolResultData is the `data` body of a tool/result event. dsh
// nests the result inside a `message` user-role echo:
// `data.message.content[]` is a list of `tool-result` blocks
// (each tool-result can wrap inner text/image blocks under
// content.content). The translator walks the array itself to
// extract every tool-result, not just the first.
type toolResultData struct {
	Turn    int                    `json:"turn"`
	Step    int                    `json:"step"`
	Message toolResultUserEnvelope `json:"message"`
}

// toolResultUserEnvelope is the user-role message dsh echoes back
// to carry the tool/result payload. Its `content[]` is a flat
// list of `tool-result` blocks.
type toolResultUserEnvelope struct {
	Role    string            `json:"role"`
	Content []contentBlockDTO `json:"content"`
	ID      string            `json:"id,omitempty"`
}

// pickToolResultBlock returns the first tool-result block, or nil
// if none is present in the user envelope.
func (m toolResultUserEnvelope) pickToolResultBlock() *contentBlockDTO {
	for i := range m.Content {
		if m.Content[i].Type == "tool-result" {
			return &m.Content[i]
		}
	}
	return nil
}

// turnStartData is the `data` body of a turn/start event. Captured
// wire shape: `{"turn": 1}`. We deliberately ignore any extra
// fields (turnId/title) the upstream may add later — the
// translator only needs the integer counter to know the turn
// became live.
type turnStartData struct {
	Turn int `json:"turn"`
}

// turnEndData is the `data` body of a turn/end event. StopReason
// is the optional model stop reason ("stop" | "end_turn" |
// "toolUse" | "length" | "abort" | …); not all dsh builds emit
// it on every settle, so it's `omitempty`.
type turnEndData struct {
	Turn       int    `json:"turn"`
	StopReason string `json:"stopReason,omitempty"`
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

// compactionEndData is the `data` body of a compaction/end event.
// One cycle = one EventCompaction. The envelope strip happens in
// translate.go before unmarshalling this struct.
type compactionEndData struct {
	Reason  string `json:"reason,omitempty"`
	Aborted bool   `json:"aborted,omitempty"`
}

// todoWriteData is the `data` body of a todo/write event. Items
// is a full snapshot (last-write-wins), translated to F-38
// TaskList.
type todoWriteData struct {
	Items []todoItem `json:"items"`
}

// todoItem is one entry in todoWriteEvent.Items. Matches
// `packages/core/session/src/types.ts: TodoItem`.
type todoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // "pending" | "in_progress" | "completed"
}

// approvalAskedData is the `data` body of an approval/asked event
// (the session/event channel for approval flows; distinct from
// mux approval/requested which is the server-pushed version).
// Wire field names may not all match — translate.go's catch-all
// falls through to a debug log when the payload is empty, which
// is the expected behaviour when dsh skips this event entirely.
type approvalAskedData struct {
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

// ─── /api/session.models response ────────────────────────────────────

// sessionModelsValue is the `value` payload of an OK session.models
// response. Mirrors dsh's `SessionModels` shape from
// `packages/host/apiproxy/src/api/sessions.ts`.
//
// Current is the model's authoritative selection for the session's
// NEXT assembled step — what dsh's adapter will dispatch to if the
// user prompts right now. Bridge surfaces Current.Model on
// EventAgentReady so the runtime can render the receipt header
// (e.g. "session <id> · model <name>"). The dsh bridge intentionally
// does not route a live model-change call — model changes require a
// fresh session — but Current's shape is the same one a future
// /api/session.selectModel would set, so the field names line up
// with dsh's wire contract.
//
// Routable tells us whether an adapter currently serves
// Current.Provider; without it, the session can't start a turn at
// all. Bridge does not gate on this — dsh will return agent-busy
// or model-unavailable if a prompt is sent without a routable
// adapter — but the flag is preserved on the wire struct so a
// future caller can surface it.
//
// Groups + Failures are kept as RawMessage because the runtime
// does not currently surface a /model picker UI for dsh (deferred
// per docs/bridge/dsh.md §11). Future PR can decode them against
// ModelProviderGroup / ModelCatalogFailure when /model lands.
type sessionModelsValue struct {
	Current  modelSelectionWire `json:"current"`
	Routable bool                `json:"routable"`
	Groups   json.RawMessage     `json:"groups,omitempty"`
	Failures json.RawMessage     `json:"failures,omitempty"`
}

// modelSelectionWire is one entry of ModelSelection
// (`sessions.ts: ModelSelection`). `Provider` is the registered
// route key (e.g. "minimax-cn"), `Model` is the provider-owned
// model id (e.g. "MiniMax-M3"). ReasoningEffort is optional and
// only populated when the adapter exposes it for this exact route.
//
// The bridge stamps Model onto EventAgentReady.Model verbatim
// (provider:model would be too wide for the runtime's model
// string — runtime compares against `agent.UsageInfo.ContextWindow`
// table and the channel footer wants a single token).
type modelSelectionWire struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}
