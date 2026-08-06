// Wire-level structs for the Pi RPC protocol. These are intentionally
// permissive: every optional or evolving field is captured as
// json.RawMessage so unknown additions from upstream Pi do not break
// the bridge. Only the fields we actively translate or correlate on
// are decoded into typed structs.
//
// Wire shape (per Pi docs/rpc.md):
//
//   command  : {"id":<id>, "type":<command name>, ...fields}
//   response : {"id":<id>, "type":"response", "command":<name>, "success":<bool>, "data":<obj|absent>, "error":<string|absent>}
//   event    : {"type":<event name>, ...event-specific fields}
//
// Strict LF framing. No JSON-RPC 2.0 envelope. The "id" is opaque;
// commands and responses share the same id space.
package pi

import "encoding/json"

// responseEnvelope is the wire shape for server -> client command
// acknowledgements.
type responseEnvelope struct {
	// ID matches the corresponding commandEnvelope.ID.
	ID json.RawMessage `json:"id,omitempty"`

	// Type is the literal string "response".
	Type string `json:"type"`

	// Command mirrors the command name that produced this response.
	Command string `json:"command"`

	// Success is true when the command was accepted by Pi.
	Success bool `json:"success"`

	// Data carries the command-specific result payload (e.g.
	// get_state's model + session metadata). Absent on failures.
	Data json.RawMessage `json:"data,omitempty"`

	// Error is the human-readable failure reason on !Success.
	Error string `json:"error,omitempty"`
}

// eventEnvelope is the wire shape for server -> client async events.
// Events are distinguished from responses by the absence of
// "type":"response"; both share the same stdout stream.
type eventEnvelope struct {
	// Type is the event name (e.g. "agent_start", "message_update",
	// "tool_execution_end", "agent_settled"). Discriminator for the
	// translate table.
	Type string `json:"type"`

	// Raw captures the full original JSON so translators can decode
	// event-specific fields without losing data.
	Raw json.RawMessage `json:"-"`
}

// promptParams encodes the body of a `prompt` command. The wire shape
// (per docs/rpc.md):
//
//	{"type":"prompt", "message":"...", "images":[ImageContent]?}
//
// where ImageContent is {"type":"image", "data":<base64>, "mimeType":<MIME>}.
//
// We capture images with the "data" name in the bridge to match Pi's
// spec, and rename to "data" here for the same reason. The bridge
// passes these through unchanged to the encoder.
type promptParams struct {
	Message string            `json:"message"`
	Images  []imageAttachment `json:"images,omitempty"`
}

// imageAttachment matches Pi's ImageContent shape: type=image,
// data=base64, mimeType=MIME. The "type" tag is fixed to "image" by
// Pi and not exposed as a Go field.
type imageAttachment struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// getStateResult is the `data` payload of a successful get_state
// response. Pi returns many more fields than we consume; capture
// only the ones the bridge actually needs.
type getStateResult struct {
	// Model is the currently selected model, or null if no model is
	// configured. Bridge takes the first non-null entry and surfaces
	// its ID + name in EventInit.
	Model *getStateModel `json:"model"`

	// SessionID is the running Pi session identifier. Used as the
	// EventInit.SessionID.
	SessionID string `json:"sessionId"`

	// SessionName is the optional human-readable name. Captured for
	// the log footer; not currently surfaced in AgentEvent.
	SessionName string `json:"sessionName,omitempty"`
}

// getStateModel captures the subset of Pi's model metadata the bridge
// uses. Pi may extend this freely; the bridge tolerates unknown
// fields.
type getStateModel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

// messageUpdateEnvelope is the body of a "message_update" event. Pi
// nests the actual delta discriminator under
// "assistantMessageEvent"; that nested object is the real payload.
type messageUpdateEnvelope struct {
	// AssistantMessageEvent holds the per-delta descriptor. The
	// "type" field inside is the discriminator (text_delta,
	// thinking_delta, toolcall_start, toolcall_end, ...).
	AssistantMessageEvent json.RawMessage `json:"assistantMessageEvent"`
}

// assistantMessageEvent is the discriminated union carried inside
// every message_update. The Type field selects the per-delta struct.
type assistantMessageEvent struct {
	// Type is the per-delta discriminator. See F-32 §2.3 for the
	// full list and translator behavior.
	Type string `json:"type"`

	// ContentIndex is the index of the content block within the
	// parent message. Present on most deltas; used to pair
	// start/delta/end triplets on the same block.
	ContentIndex int `json:"contentIndex,omitempty"`

	// Delta is the incremental text for text_delta / thinking_delta.
	Delta string `json:"delta,omitempty"`

	// ToolCallID identifies the tool call across toolcall_start,
	// toolcall_delta, toolcall_end and tool_execution_* events.
	ToolCallID string `json:"toolCallId,omitempty"`

	// Name is the tool name on toolcall_start. May be empty on
	// later deltas.
	Name string `json:"name,omitempty"`

	// Partial holds the in-progress content block for
	// toolcall_start. May be a partial toolCall with id, name, and
	// partial arguments. Captured as raw to avoid coupling to Pi's
	// evolving toolCall struct.
	Partial json.RawMessage `json:"partial,omitempty"`

	// ToolCall on toolcall_end carries the completed tool call
	// (id, name, arguments). Captured as raw.
	ToolCall json.RawMessage `json:"toolCall,omitempty"`

	// Reason is the stop reason on done / error deltas. Common
	// values: "stop", "aborted", "length", "error".
	Reason string `json:"reason,omitempty"`
}

// assistantMessage is the body of a "message_end" event when
// role=="assistant". Pi uses this to deliver the finalized message
// plus the per-message usage. We translate the per-message usage
// into EventUsage when the totals are non-zero.
type assistantMessage struct {
	Role       string           `json:"role"`
	Content    []contentBlock   `json:"content"`
	StopReason string           `json:"stopReason"`
	Usage      *messageUsage    `json:"usage"`
	Model      string           `json:"model"`
	Provider   string           `json:"provider"`
	Timestamp  int64            `json:"timestamp"`
}

// contentBlock is one element of assistantMessage.Content. Pi's
// content block is a discriminated union; we capture the fields we
// actually translate.
type contentBlock struct {
	// Type is "text" | "thinking" | "toolCall".
	Type string `json:"type"`

	// Text is the block text for "text" and "thinking" blocks.
	Text string `json:"text,omitempty"`

	// Thinking is the block text for "thinking" blocks (alternative
	// field name across Pi versions; both are accepted).
	Thinking string `json:"thinking,omitempty"`

	// ID is the tool call id for "toolCall" blocks.
	ID string `json:"id,omitempty"`

	// Name is the tool name for "toolCall" blocks.
	Name string `json:"name,omitempty"`

	// Arguments is the tool call arguments. Kept as raw to avoid
	// coupling to Pi's evolving schema.
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// messageUsage is the per-message usage block. Pi nests cost under
// "cost" with input/output/cacheRead/cacheWrite/total in dollars.// messageUsage is the per-message usage block. Pi nests cost under
// "cost" with input/output/cacheRead/cacheWrite/total in dollars.
type messageUsage struct {
	Input      int           `json:"input"`
	Output     int           `json:"output"`
	CacheRead  int           `json:"cacheRead"`
	CacheWrite int           `json:"cacheWrite"`
	Total      int           `json:"totalTokens"`
	Cost       *usageCost    `json:"cost"`
}

// usageCost holds the dollar cost breakdown. The bridge only emits
// EventUsage when total > 0 to avoid noise on empty turns.
type usageCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// toolExecutionEnvelope is the body of "tool_execution_start" and
// "tool_execution_end" events. tool_execution_update uses the same
// shape with a partialResult instead of a result.
type toolExecutionStart struct {
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
}

type toolExecutionEnd struct {
	ToolCallID string          `json:"toolCallId"`
	Result     json.RawMessage `json:"result"`
	IsError    bool            `json:"isError"`
}

// compactionStart / compactionEnd are F-49 bridge-abstracted: the
// start event is silently dropped (no Event emitted) and the end
// event becomes one EventCompaction (a marker — the Subtype field
// is gone). One full Pi cycle (start + end) yields exactly one
// EventCompaction on the wire, so the runtime AgentSession counter
// increments by 1 per cycle. See docs/feat/F-49-compaction-counter.md
// §1.3 / §1.7.
type compactionStart struct {
	Reason string `json:"reason"`
}

type compactionEnd struct {
	Reason string `json:"reason"`
	Result struct {
		Aborted bool `json:"aborted"`
	} `json:"result"`
}

// extensionUIRequest is the body of "extension_ui_request" events.
// The F-32 MVP does not forward these to the channel; it auto-replies
// with `cancelled` so Pi does not block. The full schema (select,
// confirm, input, editor, notify, setStatus, setWidget, setTitle) is
// captured as raw to allow future translator extensions.
type extensionUIRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Title  string          `json:"title,omitempty"`
	Options []string       `json:"options,omitempty"`
	Raw    json.RawMessage `json:"-"`
}

// extensionError is logged at warning; not surfaced as EventError.
type extensionError struct {
	ExtensionPath string `json:"extensionPath"`
	Event         string `json:"event"`
	Error         string `json:"error"`
}

// stateUpdate is emitted by pi after a new_session RPC, carrying the
// new sessionId and (optionally) the new model selection. F-34 §3.2.2.
//
// Schema fields are best-effort: if pi adds or removes fields the
// bridge will continue to compile (Go's json.Unmarshal silently
// ignores unknown fields and leaves missing fields at the zero
// value). The bridge only ever relies on SessionID for the runtime
// ResumeID pipeline.
type stateUpdate struct {
	SessionID  string `json:"sessionId"`
	ModelID    string `json:"modelId"`
	ModelName  string `json:"modelName"`
	SessionFile string `json:"sessionFile"`
}
