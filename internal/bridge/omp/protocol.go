// Wire-level structs for the omp RPC protocol. Permissive:
// every optional or evolving field is captured as
// json.RawMessage when a future omp version might add a
// sibling we don't translate yet. Only the fields we actively
// drive are decoded into typed structs.
//
// omp wire shape (per docs maintained at https://omp.sh):
//
//	command  : {"id":<id>, "type":<command name>, ...fields}
//	response : {"id":<id>, "type":"response", "command":<name>, "success":<bool>, "data":<obj|absent>, "error":<string|absent>}
//	event    : {"type":<event name>, ...event-specific fields}
//	ready    : {"type":"ready", "protocolVersion":1, ...}
//
// Strict LF framing. No JSON-RPC 2.0 envelope. The "id" is
// opaque; commands and responses share the same id space.

package omp

import "encoding/json"

// responseEnvelope is the wire shape for server -> client
// command acknowledgements. Mirrors the pi bridge's
// responseEnvelope.
type responseEnvelope struct {
	ID json.RawMessage `json:"id,omitempty"`

	// Type is the literal string "response".
	Type string `json:"type"`

	// Command mirrors the command name that produced this
	// response (get_state, prompt, abort, new_session).
	Command string `json:"command"`

	// Success is true when the command was accepted by omp.
	Success bool `json:"success"`

	// Data carries the command-specific result payload
	// (get_state.data.sessionId, etc.). Absent on failures.
	Data json.RawMessage `json:"data,omitempty"`

	// Error is the human-readable failure reason on
	// !Success.
	Error string `json:"error,omitempty"`
}

// eventEnvelope is the wire shape for server -> client async
// events. Events are distinguished from responses by the
// absence of "type":"response"; both share the same stdout
// stream.
type eventEnvelope struct {
	Type string `json:"type"`
}

// promptParams encodes the body of a `prompt` command. Wire
// shape:
//
//	{"type":"prompt", "message":"..."}
type promptParams struct {
	Message string `json:"message"`
}

// getStateResult is the `data` payload of a successful get_state
// response. omp returns many more fields than we consume;
// capture only the ones the bridge actually needs.
type getStateResult struct {
	// Model is the currently selected model. omp nests model
	// metadata under "model": { id, name, provider,
	// contextWindow }.
	Model *getStateModel `json:"model"`

	// SessionID is the running omp session identifier.
	// Used as EventAgentReady.SessionID.
	SessionID string `json:"sessionId"`

	// SessionFile is the on-disk path omp persists the
	// session to. Captured for diagnostics; not currently
	// surfaced.
	SessionFile string `json:"sessionFile,omitempty"`

	// ThinkingLevel is the current reasoning-effort tier
	// ("off" / "minimal" / "low" / "medium" / "high"). omp's
	// only canonical setting name across versions; recorded
	// on get_state and New() re-emission.
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
}

// getStateModel captures the subset of omp's model metadata the
// bridge uses. omp may extend this freely; the bridge tolerates
// unknown fields.
//
// ContextWindow bridges the gap between the SDK-native
// handshake and the per-message usage block: F-54 in the pi
// bridge derives the per-turn ContextWindowPct denominator
// from this value. omp's print-mode never reaches get_state,
// so ContextWindow stays at 0 for /gtw commit and similar
// one-shot callers — runtime renders `(window)` only when the
// chat-session bridge hands back a non-zero value.
type getStateModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	ContextWindow int    `json:"contextWindow"`
}

// messageUpdateEnvelope is the body of a "message_update"
// event. omp nests the per-delta descriptor under
// "assistantMessageEvent"; that nested object is the real
// payload.
type messageUpdateEnvelope struct {
	AssistantMessageEvent json.RawMessage `json:"assistantMessageEvent"`
}

// assistantMessageEvent is the discriminated union carried
// inside every message_update from omp. The Type field selects
// the per-delta struct.
type assistantMessageEvent struct {
	// Type is the per-delta discriminator. omp emits the
	// six kinds enumerated in translate.go (text_* /
	// thinking_*) plus start/done markers.
	Type string `json:"type"`

	// ContentIndex is the index of the content block within
	// the parent message. omp keys its stream on this.
	ContentIndex int `json:"contentIndex,omitempty"`

	// Delta is the incremental text for text_delta /
	// thinking_delta.
	Delta string `json:"delta,omitempty"`
}

// assistantMessage is the body of a "message_end" event when
// role=="assistant". omp attaches the finalized assistant
// message plus per-message model + usage on this single frame.
type assistantMessage struct {
	Role     string        `json:"role"`
	Model    string        `json:"model"`
	Provider string        `json:"provider"`
	Usage    *messageUsage `json:"usage,omitempty"`
}

// messageUsage is the per-message usage block. omp nests cost
// under "cost" with input / output / cacheRead / cacheWrite
// / total in dollars — same shape pi emits.
type messageUsage struct {
	Input      int        `json:"input"`
	Output     int        `json:"output"`
	CacheRead  int        `json:"cacheRead"`
	CacheWrite int        `json:"cacheWrite"`
	Total      int        `json:"totalTokens"`
	Cost       *usageCost `json:"cost,omitempty"`
}

// usageCost holds the dollar cost breakdown. Mirrored from
// the pi bridge so the runtime sees the same UsageInfo surface.
type usageCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// toolExecutionStart is the body of "tool_execution_start".
// omp populates intent with the human-readable title (e.g.
// "Reading notes.txt"); the bridge prefers intent over
// toolName for the card title.
type toolExecutionStart struct {
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Intent     string          `json:"intent,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
}

// toolExecutionEnd is the body of "tool_execution_end". omp
// always populates ToolName on the wire so orphan ends still
// render the right name.
type toolExecutionEnd struct {
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Result     json.RawMessage `json:"result,omitempty"`
	IsError    bool            `json:"isError"`
}

// sessionMeta is the print-mode opening frame. omp emits it
// (not a `ready` frame) before the first message of a one-shot
// `omp -p --mode json` invocation; the bridge peeks the id
// for the call's SessionID.
type sessionMeta struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Version int    `json:"version"`
}
