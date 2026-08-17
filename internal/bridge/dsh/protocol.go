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

// ─── WS server-request envelope ─────────────────────────────────────────
//
// RPC envelope types (clientRequest / rpcResponse / rpcResult /
// rpcError) live in internal/bridge/dsh/host/client.go alongside
// the RPCClient that produces them. The dsh bridge here no longer
// constructs those envelopes directly — every RPC call goes through
// host.Client, and the typed wrappers return host.RPCClient's
// shapes directly. Keeping duplicates here would create two types
// for one wire concept, both with zero callers outside this file.
//
// The mux / host wire frames still live here because the
// dispatch / permissions layer decodes them directly (handle_mux.go,
// permissions.go, translate.go). Decoding the mux payload needs the
// per-method typed shapes; we keep those.
//
// The serverFrame envelope itself is decoded by host.StreamHub (see
// internal/bridge/dsh/host/stream.go) which exposes typed callbacks
// with (method, rpcID, payload json.RawMessage) — the dsh bridge
// receives the inner payload directly and never needs to unmarshal
// the envelope itself, so serverFrame lives there.

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
// serverFrame{method:"approval/requested"}. `ApprovalID` is
// audit-only. /api/respond is keyed on the envelope rpcId.
type muxApprovalRequested struct {
	SessionID  string `json:"sessionId"`
	ApprovalID string `json:"approvalId"`
	ToolName   string `json:"toolName"`
	CallID     string `json:"callId,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// muxApprovalResolved is the audit frame after the host settled
// an approval (dashboard or /api/respond). Bridge drops local
// pending and PATCHes the Feishu card.
type muxApprovalResolved struct {
	SessionID  string `json:"sessionId"`
	ApprovalID string `json:"approvalId"`
	Outcome    string `json:"outcome"` // allowed-once | rejected | cancelled | unavailable
}

// muxQuestionResolved is the audit frame after a question batch is
// answered or cancelled (dashboard or /api/respond).
type muxQuestionResolved struct {
	SessionID     string `json:"sessionId"`
	QuestionRPCID string `json:"questionRpcId"`
	Outcome       string `json:"outcome"` // answered | cancelled
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
// `ID` is the stable caller-provided question id, echoed in the
// /api/respond answer (AskUserQuestionAnswerItem.id). Host
// matchesQuestions rejects a batch whose ids / length don't match
// the pending request. `Header` is the short label; `Question` is
// the full text; `Options` are the multiple-choice options.
//
// BUG FIX (F-dsh-shared-host §6 #4): pre-fix this used
// `[]string` (just labels). Canonical wire shape is
// `[]AskUserQuestionOption{label,description?}` per dsh-api.md
// §3.4.9 — objects, not bare strings. dsh 0.1.0-rc.6 happened to
// accept either form (so the bug didn't surface), but future
// versions and the runtime's display logic both need the object
// shape (label + description renders a richer feishu card).
type questionPayload struct {
	ID       string                  `json:"id"`
	Header   string                  `json:"header"`
	Question string                  `json:"question"`
	Options  []AskUserQuestionOption `json:"options"`
	Multi    bool                    `json:"multiSelect,omitempty"`
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
//	  "seq":  27,
//	  "time": 1786862336663,
//	  "data": {
//	    "turn": 1, "step": 1,
//	    "message": { "role": "assistant", "content": [ … ] }
//	  }
//	}
//
// Seq and Time are populated by dsh for every event but the dispatcher
// itself only reads Type. The bridge uses Seq for dedup across the
// mux stream (session/event) and the session.history backfill path
// (both feeds dispatchEvent) so a frame arriving on both delivers
// exactly once. Seq is monotonically increasing per session.
type sessionEventEnvelope struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq,omitempty"`
	Time int64           `json:"time,omitempty"`
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
//     (current dsh builds only emit text-delta /
//     tool-call-delta)
//   - "block-end":       Block carries the complete finalized block
//   - "text-delta":      Text carries the streaming fragment,
//     Index is the content-block slot
//   - "tool-call-delta": ID + Name + ArgumentsDelta carry a partial
//     tool-call accumulator
//   - "usage":           Usage carries the model's usage row
//   - "finish":          Reason carries the step-finish discriminator
type assistantChunk struct {
	Type           string             `json:"type"`
	Index          int                `json:"index,omitempty"`
	BlockType      string             `json:"blockType,omitempty"`
	Text           string             `json:"text,omitempty"`
	ArgumentsDelta string             `json:"argumentsDelta,omitempty"`
	ID             string             `json:"id,omitempty"`
	Name           string             `json:"name,omitempty"`
	Block          *assistantBlock    `json:"block,omitempty"`
	Usage          *usageInfo         `json:"usage,omitempty"`
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

// stepBoundaryData is the `data` body of step/start and step/end.
// Captured wire (dsh 0.1.0-rc.6 session.history):
//
//	{"type":"step/start","seq":6,"data":{"turn":1,"step":1}}
//	{"type":"step/end","seq":40,"data":{"turn":1,"step":1}}
//
// A step is one model-inference cycle (one LLM call plus the tool
// executions it requested). Dashboard uses these for sessionStats
// (TTFT / tok/s), not the TodoPanel — that panel is todo/write +
// the `todos` projection. The bridge registers the types so they
// are not counted unknown, and does not emit AgentEvents.
type stepBoundaryData struct {
	Turn int `json:"turn"`
	Step int `json:"step"`
}

// usageInfo mirrors dsh's LLM usage payload. We decode the minimum
// fields nightme cares about for footer rendering.
//
// Field naming gap vs agent.UsageInfo: dsh calls them
// CacheCreationTokens / CacheReadTokens (input-side cache accounting);
// agent.UsageInfo names them CacheCreationInputTokens /
// CacheReadInputTokens. Conversion happens in usageToAgent() so the
// bridge boundary stays a single point of truth.
type usageInfo struct {
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens,omitempty"`
	CacheReadTokens     int     `json:"cacheReadTokens,omitempty"`
	CostUSD             float64 `json:"costUsd,omitempty"`
	ContextWindow       int     `json:"contextWindow,omitempty"`
	ContextWindowPct    float64 `json:"contextWindowPct,omitempty"`
}

// compactionEndData is the `data` body of a compaction/end event.
// One cycle = one EventCompaction. The envelope strip happens in
// translate.go before unmarshalling this struct.
type compactionEndData struct {
	Reason  string `json:"reason,omitempty"`
	Aborted bool   `json:"aborted,omitempty"`
}

// todoWriteData is the `data` body of a todo/write event. Todos
// is a full snapshot (last-write-wins), translated to F-38
// TaskList.
//
// F-DSH-TODO-WIRE-FIX (2026-08-16): the JSON tag is `todos` to
// match the real dsh wire (verified via the @deepseek-ai/dsh-tool-todo
// source: `agent.session.append('todo/write', { todos })`). Pre-fix
// this struct used `items`, which silently failed every
// json.Unmarshal — wire data was present but the bridge decoded
// it as zero items, then emitted EventAgentTaskCreate{Items:[]}
// (the "clear checklist" signal) — exactly the "todo list not
// showing up" symptom.
//
// We accept `items` as a fallback for older dsh versions or
// future drift; the first decode wins, the second is a no-op
// (json.Unmarshal into a second struct with the alternative tag
// then merging). Production today reads `todos`.
type todoWriteData struct {
	Todos []todoItem `json:"todos"`
	// Items is the legacy field name (pre-fix this struct used
	// it as the primary tag). When the wire uses `items` instead
	// of `todos` (older dsh, or a future rename), decoding into
	// this field recovers the snapshot. Custom UnmarshalJSON
	// below picks the populated one. JSON tag is `items,omitempty`
	// so the canonical wire (`todos` only) doesn't carry a noisy
	// empty `items:null` after a round-trip.
	Items []todoItem `json:"items,omitempty"`
}

// UnmarshalJSON implements the wire-shape fallback: real dsh
// emits `{todos: [...]}`, older / fork builds may use
// `{items: [...]}`. We decode into both via a oneOf selector
// and pick the populated slice. If both are populated, `todos`
// wins (canonical).
func (d *todoWriteData) UnmarshalJSON(data []byte) error {
	var both struct {
		Todos []todoItem `json:"todos"`
		Items []todoItem `json:"items"`
	}
	if err := json.Unmarshal(data, &both); err != nil {
		return err
	}
	if len(both.Todos) > 0 {
		d.Todos = both.Todos
	} else {
		d.Todos = both.Items
	}
	d.Items = both.Items
	return nil
}

// ToolEventView is the host-computed rendering view that dsh web
// attaches to every session/event frame. Per docs/bridge/dsh.md
// §1.3, each `session/event` frame carries `view?: ToolEventView`
// alongside the raw `event` payload — the host has already merged
// the event into its UI state and shipped a snapshot back so the
// bridge doesn't have to re-derive tool / task state from the
// raw event stream.
//
// F-DSH-CHAT-001 P3: this struct is the authoritative source for
// tool status (running / completed / failed) when present. P3
// makes View the truth source instead of bridge-inferred tool
// state from raw event pairing.
//
// WIRE-PROBE-REQUIRED: field names below are best-guess from
// the upstream dsh "host-computed ToolEventView" comment. Real
// wire may use different casing or extra fields. If probe
// reveals a different shape, update these tags only — handler
// logic stays the same.
type ToolEventView struct {
	// Kind discriminates what's in the view. "tool_call" for a
	// tool invocation view; "task_list" for a todo snapshot view
	// (host emits a task_list view attached to todo/write or
	// session/projection frames). Empty / unknown = skip.
	Kind string `json:"kind,omitempty"`

	// Tool-call view fields (Kind == "tool_call").
	CallID  string `json:"callId,omitempty"`
	Name    string `json:"name,omitempty"`
	Status  string `json:"status,omitempty"` // "running" | "completed" | "failed"
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"isError,omitempty"`

	// Task-list view fields (Kind == "task_list"). Carries the
	// host-computed snapshot — same shape as todo/write data.
	Tasks []todoItem `json:"tasks,omitempty"`

	UpdatedAt int64 `json:"updatedAt,omitempty"`
}

// projectionEnvelope is the mux `session/projection` frame payload
// (F-DSH-CHAT-001 §4.5). dsh web emits these periodically (and on
// state changes) carrying host-computed derived state — title /
// tasks / session list metadata. Previously the bridge dropped
// these on the floor; we now route them through wireState.
//
// WIRE-PROBE-REQUIRED: field names below are inferred from
// docs/bridge/dsh.md §4.5 (which says
// `{sessionId, seq, projection, value}`). If probe shows different
// names (e.g. `name` instead of `projection`), update tags only.
type projectionEnvelope struct {
	SessionID string `json:"sessionId,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	// BUG FIX (F-dsh-shared-host §6 #1): the wire field is "key"
	// (dsh-api.md §3.4.3), not "projection". Pre-fix this struct
	// used `json:"projection"` so dsh's mux frames never matched
	// the decoder (dsh 0.1.0-rc.6 happened to also accept the
	// legacy form, masking the bug). The canonical wire is "key".
	Key string `json:"key"` // "todos" | "todo" | "tasks" | "title" | ...
	// Canonical wire (verified 2026-08-16 against captured
	// testdata/projections/todo_snapshot.json) is "todos"
	// (plural). Singular "todo" and "tasks" are accepted as
	// fallbacks for older / fork builds — dispatched in
	// applyProjectionLocked.
	Value json.RawMessage `json:"value"`
}

// todoItem is one entry in todoWriteEvent.Items. Matches
// `packages/core/session/src/types.ts: TodoItem`.
//
// F-DSH-CHAT-001: ID and ActiveForm added so wireState.tasks can
// index by stable ID (otherwise AgentTaskItem.ID == "" breaks
// runtime-side dedup / merge / delta). Both are best-effort: if
// dsh's wire omits them, the bridge falls back to Content as ID
// and ActiveForm to "".
//
// WIRE-PROBE-REQUIRED: the JSON tag names below are inferred from
// the upstream dsh type comment. If probe reveals different field
// names (e.g. `taskId` instead of `id`), update the tags only —
// handler logic stays the same.
type todoItem struct {
	ID         string `json:"id,omitempty"`
	Content    string `json:"content"`
	ActiveForm string `json:"activeForm,omitempty"`
	Status     string `json:"status"` // "pending" | "in_progress" | "completed"
}

// approvalAskedData is the `data` body of an approval/asked event
// (the session/event channel for approval flows; distinct from
// mux approval/requested which is the server-pushed version).
// Wire field names may not all match — translate.go's catch-all
// falls through to a debug log when the payload is empty, which
// is the expected behaviour when dsh skips this event entirely.
type approvalAskedData struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Action     string `json:"action"`
	// BUG FIX: pre-fix this was []string; canonical wire is
	// []AskUserQuestionOption (objects). Same reasoning as
	// questionPayload.Options above.
	Options []AskUserQuestionOption `json:"options"`
}

// AskUserQuestionOption is the option shape used by both
// muxQuestionRequested and approval/asked payloads (dsh-api.md
// §3.4.6 / §3.4.9). One selectable choice; Label is the visible
// answer and Description is auxiliary context shown alongside.
type AskUserQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// optionLabels extracts the visible Label from each option. The
// runtime's EventAgentPermission.Options field is a []string
// (label-only) for parity with other bridges (claudecode etc.); the
// canonical wire shape carries label + description, but the
// feishu card renders label only. Centralising the projection here
// keeps every callsite consistent.
func optionLabels(opts []AskUserQuestionOption) []string {
	if len(opts) == 0 {
		return nil
	}
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.Label
	}
	return out
}

// ─── /api/session.create response ──────────────────────────────────────

// sessionCreateValue is the `value` payload of an OK session.create
// response. We only consume SessionID; the rest are kept as
// RawMessage for audit logs / future fields.
type sessionCreateValue struct {
	SessionID string `json:"sessionId"`
}

// ─── /api/session.list response ───────────────────────────────────

// Session is one entry in the session.list result. Wire shape
// captured from a real `dsh --profile web` 0.1.0-rc.6 instance on
// 2026-08-15 (docs/probe/dsh-2026-08-15.sh in this commit's notes
// — pre-PR-validation probe). Every field except ID is audit/UI
// only, but we decode them so a future IM-rendered picker card
// (blank/running for "is this resumable?", cwd for "is this mine?")
// doesn't need a second decode pass.
//
// Field notes (verified against real wire):
//
//   - ID          — the session's wire id. Format is a UUID
//     prefixed with "session-" (e.g.
//     "session-e4fe0be6-c082-48a5-be70-77628e7486bc").
//     Same shape as sessionCreateValue.SessionID.
//   - UpdatedAt   — unix millis of the last write. Use this as
//     the "most recent" sort key for picker UI.
//   - Running     — bool; true while the session has an in-flight
//     turn. Resuming a session that has no completed
//     turn is fine — the bridge just attaches and
//     waits for new events. (session.fork is no
//     longer used by this bridge.)
//   - Blank       — bool; true for sessions with zero completed
//     turns. Picker should pre-filter blanks for a
//     smoother UX.
//   - CWD         — directory the session was created against.
//     Used by the picker to filter sessions for the
//     current /cwd (cross-workspace contamination is
//     annoying — docs/bridge/dsh.md §11).
//   - AgentPreset — registered agent preset key (e.g.
//     "standard"). Audit only; the bridge doesn't
//     dispatch on this.
//   - Projections — optional. Some sessions include a
//     "projections.values" object carrying derived
//     metadata (title, turnCount, tokenUsage). The
//     bridge does NOT decode it — kept as RawMessage
//     so a server-side projection schema bump
//     doesn't break us. Future "show me the title"
//     picker card would decode `projections.values.title`.
type Session struct {
	ID          string          `json:"sessionId"`
	UpdatedAt   int64           `json:"updatedAt"`
	Running     bool            `json:"running"`
	Blank       bool            `json:"blank"`
	CWD         string          `json:"cwd,omitempty"`
	AgentPreset string          `json:"agentPreset,omitempty"`
	Projections json.RawMessage `json:"projections,omitempty"`
}

// sessionListValue is the `value` payload of an OK session.list
// response. IMPORTANT: the wire field is `items` (NOT `sessions`)
// — confirmed via 实机 HTTP probe 2026-08-15 against dsh 0.1.0-rc.6.
// The first version of this struct assumed `sessions` and silently
// produced zero-value Sessions for every entry; the picker UI
// would have shown "all blank entries". Fixed.
type sessionListValue struct {
	Items []Session `json:"items"`
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
	Routable bool               `json:"routable"`
	Groups   json.RawMessage    `json:"groups,omitempty"`
	Failures json.RawMessage    `json:"failures,omitempty"`
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
