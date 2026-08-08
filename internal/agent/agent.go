// Package agent defines the abstract Agent interface that wraps any
// AI Coding CLI behind a single structured event stream.
//
// Two layers:
//
//   - Agent       — static metadata + factory for sessions (Name, Mode, Detect, Start).
//   - AgentSession — a running instance: events channel + control plane
//     (SendText, SendPermission, Close).
//
// The Mode selector (ACP / SDK / PTY) lets the Bridge pick a backend
// without leaking protocol details to Session Manager. See
// docs/feat/F-09-agent-abstraction.md and F-21-agent-modes.md.
package agent

import (
	"context"
	"errors"
	"fmt"
)

// Mode identifies how the Bridge should communicate with the agent.
type Mode int

const (
	// ModeACP is the preferred mode: Agent Client Protocol over stdio
	// (JSON-RPC). Provides structured events, permission requests, and
	// tool progress out of the box.
	ModeACP Mode = iota

	// ModeSDK is a vendor-specific SDK (e.g. Claude Code Agent SDK).
	// Used when the CLI does not implement ACP.
	ModeSDK

	// ModePTY is the transparent fallback: spawn the CLI in a PTY and
	// pipe bytes. No structured events; ANSI / spinner / progress
	// indicators appear as raw text in the IM channel.
	ModePTY

	// ModeJSONIO is the v0.2 mode used by Claude Code. It spawns the
	// CLI with --input-format stream-json --output-format stream-json
	// and parses line-delimited JSON events from stdout. Provides
	// structured events (text, tool_use, tool_result, AskUserQuestion,
	// result) without requiring an ACP / SDK implementation. See
	// docs/feat/F-24-claudecode-bridge.md.
	ModeJSONIO
)

// String renders a Mode for logs and error messages.
func (m Mode) String() string {
	switch m {
	case ModeACP:
		return "acp"
	case ModeSDK:
		return "sdk"
	case ModePTY:
		return "pty"
	case ModeJSONIO:
		return "json-io"
	default:
		return fmt.Sprintf("mode(%d)", int(m))
	}
}

// EventKind discriminates the payload attached to an AgentEvent.
type EventKind int

const (
	// EventText is a stream of text bytes from the CLI (PTY mode) or a
	// structured assistant message chunk (ACP / SDK modes).
	//
	// GRANULARITY CONTRACT (F-52): one EventText is ONE COMPLETE
	// SEMANTIC BLOCK — a paragraph the user should see as a single
	// unit. It is NOT a streaming delta.
	//
	// This matters because gateway.Translate maps EventText to
	// OutReply, and OutReply means "append one entry" to the channel's
	// rolling log. A bridge that forwards per-token deltas therefore
	// shatters one sentence into dozens of chat bubbles (and dozens of
	// Feishu card PATCHes). That was the F-52 bug in the pi bridge:
	// aggregate in the bridge, where the wire's own block boundaries
	// are visible.
	//
	// Conformance:
	//   - claudecode — yes, one per assistant content block.
	//   - pi         — yes since F-52; buffers text_delta and emits at
	//                  text_end / the tool boundary.
	//   - acp        — NOT YET; still forwards agent_message_chunk
	//                  deltas verbatim (internal/bridge/acp/session.go).
	//                  Deliberately out of F-52's scope; its block
	//                  boundary is stopReason, not text_end.
	//   - pty        — exempt; raw byte stream with no block structure.
	EventText EventKind = iota

	// EventPermission is a permission request from the agent. The
	// Channel renders it as an interactive UI (e.g. Feishu card with
	// buttons) and the user choice is fed back via SendPermission.
	EventPermission

	// EventToolStart marks the beginning of a tool invocation.
	EventToolStart

	// EventToolEnd marks the end of a tool invocation (success or error).
	EventToolEnd

	// EventDone signals the session has ended cleanly (or with an exit
	// code). After EventDone no further events will be emitted.
	EventDone

	// EventError carries an unrecoverable error during the session.
	EventError

	// EventResult is the assistant's final reply for the turn. In
	// Claude Code stream-json this is the value of `result` on the
	// result event — distinct from EventText (which is mid-stream
	// assistant text). Channels typically render it with a different
	// icon than the rolling-log entries.
	//
	// ResultEvent carries the per-turn Usage (token counts + cost) on
	// the same field — bridges populate it from the source event's
	// usage / modelUsage instead of emitting a separate EventUsage.
	// Runtime accumulates Usage on receipt before stamping
	// SessionContext, so the footer sees this turn's tokens on the
	// first try.
	EventResult

	// EventCompaction signals a mid-turn context compaction. Bridges
	// emit it when the agent's stream-json result carries
	// subtype "compact" or "compaction" — these are NOT turn-end
	// signals; subsequent assistant / tool events continue the same
	// turn. Channels typically surface a brief "Compacting…" indicator.
	EventCompaction

	// EventAgentConnected carries session bootstrap data (session_id + model)
	// from the agent's system/init event. Channels use it to surface
	// "session <id> · model <name>" in the receipt header.
	EventAgentConnected

	// EventTaskCreate signals the first confirmable task operation
	// (e.g. Claude TaskCreate success). The payload carries the
	// current full task snapshot, not a delta, so that any consumer
	// can render a complete checklist without cross-event correlation.
	EventTaskCreate

	// EventTaskUpdate signals a confirmable task mutation after
	// create (status change, edit, delete). The payload also carries
	// the current full task snapshot so receipts can replace the
	// checklist wholesale.
	EventTaskUpdate
)

// String renders an EventKind for logs.
func (k EventKind) String() string {
	switch k {
	case EventText:
		return "text"
	case EventPermission:
		return "permission"
	case EventToolStart:
		return "tool_start"
	case EventToolEnd:
		return "tool_end"
	case EventDone:
		return "done"
	case EventError:
		return "error"
	case EventResult:
		return "result"
	case EventCompaction:
		return "compaction"
	case EventAgentConnected:
		return "agent_connected"
	case EventTaskCreate:
		return "task_create"
	case EventTaskUpdate:
		return "task_update"
	default:
		return fmt.Sprintf("event(%d)", int(k))
	}
}

// PermissionRequest is the structured payload for EventPermission.
//
// The Bridge exposes the available options (e.g. "once", "session",
// "reject") and the Channel renders them. The user's choice is sent
// back via ResponseCh exactly once; the Bridge consumes it and
// forwards the decision to the agent.
type PermissionRequest struct {
	// Tool is the tool name (e.g. "Bash", "Write", "Edit").
	Tool string

	// Action is a human-readable description of the action being
	// requested (e.g. "Run command: rm -rf build/").
	Action string

	// Options enumerates the user-selectable choices. The first option
	// is treated as the default / safe choice.
	Options []string

	// ResponseCh receives the user's selected option string. Buffer 1,
	// non-blocking — the Bridge reads it once and proceeds.
	ResponseCh chan string
}

// ToolStartEvent carries metadata when a tool invocation begins.
type ToolStartEvent struct {
	// ID is opaque, stable for the lifetime of one invocation. Pair
	// with ToolEndEvent.ID to correlate start/end.
	ID string

	// Name is the tool name (e.g. "Bash", "Read", "Write").
	Name string

	// Args are the raw or structured arguments passed to the tool.
	Args string
}

// ToolEndEvent carries the result of a finished tool invocation.
type ToolEndEvent struct {
	// ID matches the corresponding ToolStartEvent.ID.
	ID string

	// Name mirrors the tool name for symmetry with ToolStartEvent.
	Name string

	// Args are the raw or structured arguments passed to the tool.
	// Bridges populate this from the corresponding tool_use block;
	// it may be empty if the bridge couldn't correlate the result.
	Args string

	// Output is a short textual summary of the tool's result, suitable
	// for the renderer to surface in the rolling log. Bridges should
	// populate this from the tool's stdout / structured result /
	// response payload. The renderer truncates to perEntryMaxBytes
	// before display, so bridges may pass large payloads verbatim
	// without pre-truncating.
	Output string

	// Err is non-nil when the tool failed. When Err is set, Output
	// typically holds nothing (the failure path bypasses the
	// payload); channels may use either field for display.
	Err error
}

// DoneEvent is the terminal payload for a clean session end.
//
// EventDone carries two related but distinct lifecycle signals:
//
//   - For one-shot bridges (Claude Code stream-json, PTY) EventDone
//     means "the process is done; no more events will follow."
//     The bridge closes the events channel after emitting EventDone.
//   - For long-lived bridges (Pi --mode rpc) EventDone means
//     "the current turn settled; the process is still alive and
//     may produce more events on the next user turn." The events
//     channel stays open until process exit or Close().
//
// Channels and the ChatSession read pump can therefore rely on
// "channel closed" as the universal "session is over" signal and
// use the Reason field (when non-empty) to disambiguate turn-end
// from process-end.
//
// Usage rides on DoneEvent (F-52 universal-prompt-end design) so
// the runtime can read per-turn stats from EITHER EventResult or
// EventDone uniformly — EventResult is emitted only when there is a
// text payload, but EventDone fires every turn, including empty /
// aborted ones. ResultEvent.Usage remains populated for callers
// that already read from the result-bearing event, but the runtime's
// accumulation path reads from Done.Usage (single source of truth).
// See docs/feat/F-52-pi-stream-aggregation.md.
type DoneEvent struct {
	// ExitCode follows Unix convention: 0 = success, non-zero = error.
	// -1 indicates an abnormal termination (e.g. PTY EOF without a
	// child exit code).
	ExitCode int

	// Reason is an optional, bridge-defined tag describing why
	// EventDone was emitted. Empty string means "use the bridge
	// default" (process exit for one-shot bridges). Bridges that
	// multiplex turns over a single process set Reason to
	// "settled" (or another agreed value) so callers can tell a
	// turn-end EventDone from a process-end one. See
	// docs/feat/F-32-pi-rpc-bridge.md §3.
	Reason string

	// Usage is the per-turn token usage observed on the same wire
	// event as the bridge's turn-end signal. Bridges populate this
	// on the SAME DoneEvent they emit — for one-shot bridges this
	// happens at process exit; for long-lived bridges (Pi) this
	// happens at every settled turn. The runtime is a passive
	// pass-through: it does NOT aggregate across turns and does NOT
	// fold Usage into any AgentSession state. Channel adapters read
	// Usage directly from the OutboundMessage (populated by
	// gateway.Translate from Done.Usage / Result.Usage) and render
	// it as footer Line 2. nil is a valid "no usage reported" value
	// (zero-usage turn, synthetic assistant message, etc.) — the
	// footer omits Line 2 in that case.
	Usage *UsageEvent
}

// ErrorEvent carries an unrecoverable error from the session.
type ErrorEvent struct {
	Err error
}

// ResultEvent is the payload for EventResult — the assistant's final
// reply for the turn. Bridges populate Text from the stream-json
// result event's `result` field; DurationMs / IsError / Subtype are
// pass-through metadata for the channel to surface alongside the text
// (e.g. "📝 <text> (12.3s)").
//
// Usage is the per-turn token usage that the bridge observed on the
// same wire event that delivered Text (Claude Code's `result.usage`
// + `result.modelUsage`; Pi's `message_end.usage`). Bridges populate
// this on the SAME ResultEvent rather than emitting a separate
// EventUsage — the data is contextually attached to the turn's
// result, not a peer event. Runtime accumulates Usage on receipt;
// channels fold it into the SessionContext footer. nil is a valid
// "no usage reported" value (the bridge may legitimately observe a
// zero-usage turn, e.g. a synthetic assistant message).
type ResultEvent struct {
	// Text is the final assistant reply. May be empty when the turn
	// ended with an error; channels typically still emit an EventResult
	// so the header line can flip to an error state.
	Text string

	// DurationMs is the wall-clock duration of the turn in
	// milliseconds (Claude Code: result.duration_ms).
	DurationMs int64

	// IsError is true when the turn ended abnormally (Claude Code:
	// result.is_error). When set, channels typically render the
	// ResultEvent with an error icon.
	IsError bool

	// Subtype is the result event's subtype (Claude Code: e.g.
	// "success", "error_max_turns", "compact", "compaction"). Bridges
	// already convert "compact" / "compaction" into EventCompaction
	// before the ResultEvent, so any Subtype seen here is a real
	// terminal subtype.
	Subtype string

	// Usage is the per-turn token usage observed on the same wire
	// event as Text. See struct doc above. Populated by bridges;
	// consumed by the runtime's newEventHandler via
	// agent.ResultEvent.Usage before stamping SessionContext.
	Usage *UsageEvent
}

// UsageEvent is the turn's token usage statistics, packaged inside
// ResultEvent (replaces the standalone EventUsage kind removed in the
// footer's "calc-then-reply" cleanup). All four counts default to
// zero when missing; channels decide how to surface (e.g. "1.2k
// tokens (in 800 · out 400) · $0.012").
//
// CostUSD is optional; bridges populate it from
// `result.modelUsage[<model>].costUSD` when present. Zero means
// "unknown / not reported" — channels must NOT render "$0.00".
//
// ContextWindowPct (see struct field below) is the per-turn
// context-fill percentage, bridge-computed via the Doc 1 formula:
//
//	pct = (InputTokens + OutputTokens + CacheCreation + CacheRead)
//	     / contextWindow * 100
//
// `contextWindow` is a bridge-local value: claudecode reads it
// from `modelUsage[<model>].contextWindow`, pi reads it from
// `get_state.data.model.contextWindow`. The window value itself
// never crosses the bridge struct boundary (F-54) — bridges
// compute pct and store only the percentage here.
//
// i.e. exact wire fields divided by API-reported window — no
// client-side model table needed. The runtime does NOT recompute
// or overwrite this; the channel footer renders it verbatim as
// the "X%" segment. See
// docs/feat/F-45-session-footer.md §1.5 / §1.6 and
// docs/feat/F-54-pi-contextwindow-from-get-state.md.
type UsageEvent struct {
	// InputTokens is the non-cached input token count.
	InputTokens int

	// OutputTokens is the generated output token count this turn.
	OutputTokens int

	// CacheCreationInputTokens is the input tokens that wrote to
	// the prompt cache this turn (Claude Code:
	// cache_creation_input_tokens).
	CacheCreationInputTokens int

	// CacheReadInputTokens is the input tokens served from the
	// prompt cache this turn (Claude Code:
	// cache_read_input_tokens).
	CacheReadInputTokens int

	// CostUSD is the optional per-turn cost in USD; 0 when unknown.
	CostUSD float64

	// ContextWindowPct is the per-turn context-fill percentage
	// (0–100), bridge-computed via the Doc 1 formula in the
	// struct doc. The runtime does NOT recompute or overwrite
	// this; the channel footer renders it verbatim.
	ContextWindowPct float64
}

// UsageInfo is the **per-turn snapshot** form of UsageEvent — what
// bridges emit on EventResult / EventDone and what the channel
// footer reads from SessionContext.Usage (see
// docs/feat/F-45-session-footer.md).
//
// IMPORTANT: this struct is NOT cumulative. Each event carries its
// own snapshot; the runtime does not sum across turns, and
// AgentSession no longer persists any cross-turn totals. A new
// bridge event is the only way a new value flows in.
//
// Lives in the agent package (not gateway) because the type is
// referenced by both agent (events) and chatsession (AgentSession).
// gateway re-exports UsageInfo as a type alias for ABI
// compatibility with the typed `Usage *UsageInfo` field on
// OutboundMessage (translate.go:158).
//
// The 4 token fields are independent counters — IN and CacheRead
// are NOT additive (Anthropic API exposes them as separate fields;
// InputTokens is non-cached input only, not the sum).
type UsageInfo struct {
	// InputTokens is the non-cached input token count from this
	// turn's LLM call (Anthropic API: input_tokens field).
	// Cache hits are NOT included — see CacheReadInputTokens.
	InputTokens int

	// OutputTokens is the generated output token count.
	OutputTokens int

	// CacheCreationInputTokens is the input tokens that wrote
	// to the prompt cache (Anthropic API:
	// cache_creation_input_tokens).
	CacheCreationInputTokens int

	// CacheReadInputTokens is the input tokens served from the
	// prompt cache (Anthropic API: cache_read_input_tokens).
	CacheReadInputTokens int

	// CostUSD is the per-turn cost in USD reported by the API
	// (Claude Code: result.total_cost_usd). Forwarded verbatim —
	// the client never computes cost. 0 when the API didn't report.
	CostUSD float64

	// ContextWindowPct is the per-turn context-fill percentage
	// (0-100), bridge-computed via the Doc 1 formula
	// (input+output+cache_creation+cache_read)/contextWindow*100.
	// Bridges populate from the same wire data that produces
	// modelUsage.<model>.contextWindow; the runtime passes it
	// through verbatim and the channel footer renders it as the
	// "X%" segment. 0 means "not reported" - the footer omits
	// X% rather than showing 0%.
	ContextWindowPct float64
}

// CompactionEvent is the payload for EventCompaction — a marker
// signalling that the agent completed a context-compaction cycle.
//
// F-49: bridges MUST emit exactly one EventCompaction per completed
// cycle. Pi suppresses its transient `compaction_start` event so a
// single Pi cycle yields one EventCompaction (on `compaction_end`);
// Claude Code emits one event per cycle naturally (result subtype
// "compact" / "compaction"). The runtime handler treats every
// EventCompaction identically and bumps the AgentSession's
// compaction counter + resets the per-cycle token stats (see
// docs/feat/F-49-compaction-counter.md §1.3).
//
// The struct is intentionally empty: previously it carried a Subtype
// string used by the runtime for protocol dispatch; F-49 removes that
// dispatch (bridges digest protocol differences; runtime is
// protocol-agnostic). Channels can rely on `Kind == EventCompaction`
// alone as the discriminator. Add fields here in the future if a
// new payload shape is needed.
type CompactionEvent struct{}

// AgentConnectedEvent is the payload for EventAgentConnected — session bootstrap data
// from the agent's system/init event. Bridges populate the
// agent-specific fields (AgentName, Workspace) from their own start
// config; the stream-json system event provides SessionID + Model.
// Channels use this payload to surface the receipt card's foot
// note (Agent · name | cwd · workspace | tokens · count).
type AgentConnectedEvent struct {
	// SessionID is the agent's opaque session id. Used for `--resume`
	// on subsequent runs; channels may surface it for debugging.
	SessionID string

	// Model is the model the agent selected (Claude Code:
	// system/init.model).
	Model string

	// AgentName is the human-friendly name of the running agent
	// (registry key, e.g. "claude" or a binding alias like "main").
	// Bridges populate this from Agent.Name() at start time; it is
	// stable for the lifetime of the session.
	AgentName string

	// Workspace is the absolute path of the working directory the
	// agent process is running in. Bridges populate this from
	// StartConfig.Workspace. Channels surface it as "cwd" in the
	// receipt foot note.
	Workspace string

	// Branch is the git branch of the workspace, captured at
	// session start by running `git -C workspace symbolic-ref
	// --short HEAD`. Empty when the workspace is not a git
	// repo or git is unavailable; the receipt's foot note
	// omits the branch segment in that case. Channels surface
	// it as the third "branch" segment of the
	// "Agent | repo | branch | tokens" foot note.
	Branch string
}

// TaskStatus is the abstract lifecycle stage of a single task in
// an agent's per-turn checklist. It is intentionally generic: any
// agent that exposes a "task list" / "todo list" primitive has
// at least pending / in_progress / completed semantics, and the
// Gateway / Channel layers must never see provider-specific
// status strings. Bridges normalise provider values into this
// enum in bridge/* before emitting a typed task event.
type TaskStatus int

const (
	// TaskPending is the default state for a freshly created task
	// that has not started running.
	TaskPending TaskStatus = iota
	// TaskInProgress marks the task the agent is currently working
	// on. The receipt may show an ActiveForm suffix ("... ·
	// writing unit tests…") to give the user a live status hint.
	TaskInProgress
	// TaskCompleted marks a task the agent has finished. The
	// receipt renders a struck-through / check-glyph variant; the
	// task may still appear in the checklist as a historical row
	// until the bridge removes it.
	TaskCompleted
	// TaskDeleted is a transient signal: the bridge parses a
	// provider-native delete, removes the task from its session
	// state, and re-emits a full snapshot where the deleted id is
	// no longer present. The Gateway / Channel must not see a
	// TaskItem with Status == TaskDeleted — by contract the
	// snapshot's Items only contains the live tasks.
	TaskDeleted
)

// String renders a TaskStatus for log lines.
func (s TaskStatus) String() string {
	switch s {
	case TaskPending:
		return "pending"
	case TaskInProgress:
		return "in_progress"
	case TaskCompleted:
		return "completed"
	case TaskDeleted:
		return "deleted"
	}
	return "task(unknown)"
}

// TaskItem is one row in the per-turn checklist. ID is the
// provider-assigned stable identifier (e.g. Claude's `Task #1`);
// bridges MUST populate it so follow-up updates can correlate by
// ID. Subject is the user-visible label. ActiveForm is the
// optional present-continuous phrase the agent emits while the
// task is in progress.
type TaskItem struct {
	ID         string
	Subject    string
	ActiveForm string
	Status     TaskStatus
}

// TaskListEvent is the typed payload for EventTaskCreate and
// EventTaskUpdate. Items is the full current snapshot of the
// provider session's task list (NOT a delta). An empty Items
// slice is a valid "clear the checklist" signal — channels may
// choose to render an empty section or hide it entirely.
type TaskListEvent struct {
	Items []TaskItem
}

// AgentEvent is the wire format on the AgentSession.Events() channel.
//
// Exactly one payload field is meaningful per Kind:
//
//	EventText       -> Text
//	EventPermission -> Permission
//	EventToolStart  -> ToolStart
//	EventToolEnd    -> ToolEnd
//	EventDone       -> Done
//	EventError      -> Error
//	EventResult     -> Result
//	EventUsage      -> Usage
//	EventCompaction -> (no payload — empty marker struct)
//	EventAgentConnected  -> Connected
//	EventTaskCreate -> TaskList
//	EventTaskUpdate -> TaskList
type AgentEvent struct {
	Kind EventKind

	// Text is the payload for EventText. Other Kinds leave it empty.
	Text string

	Permission *PermissionRequest
	ToolStart  *ToolStartEvent
	ToolEnd    *ToolEndEvent
	Done       *DoneEvent
	Error      *ErrorEvent
	Result     *ResultEvent
	Usage      *UsageEvent
	Connected  *AgentConnectedEvent

	// TaskList is the payload for EventTaskCreate / EventTaskUpdate.
	// Every event carries a full snapshot of the current task list
	// (not a delta) so consumers can replace the rendered checklist
	// wholesale. An Items slice with length 0 is a valid "clear the
	// checklist" signal.
	TaskList *TaskListEvent
}

// StartConfig is the per-session configuration handed to Agent.Start.
type StartConfig struct {
	// Workspace is the working directory the agent will operate in. It
	// is the session's immutable binding (created by /cwd, consumed by
	// /run).
	Workspace string

	// Args are extra argv items appended after the agent's own
	// defaults. v0.1 typically empty.
	Args []string

	// Env is extra environment variables (key=value form). v0.1
	// typically empty; users configure ANTHROPIC_API_KEY etc. in
	// their shell.
	Env []string

	// PermissionMode is an agent-specific permission-mode override.
	// Empty string means "use the agent's default". Bridges that
	// support a `--permission-mode` flag (Claude Code) translate this
	// into the corresponding CLI value; bridges that don't support
	// the knob silently ignore it. v0.3 ships only the Claude Code
	// bridge; the field is here so future agents can also opt in
	// without changing the Start signature.
	PermissionMode string

	// ResumeID is the agent's own session id, captured from the
	// previous run's init event (e.g. Claude Code's
	// `system/init.session_id`). Bridges that support resume (Claude
	// Code) translate this into their native flag (e.g. `--resume
	// <id>`); bridges that don't (ACP / Pi / PTY) silently ignore it.
	// Empty means "no --resume; start a fresh session".
	ResumeID string
}

// Agent is the static description of a CLI wrapper plus a factory for
// sessions. Implementations are expected to be safe for concurrent use
// after Start returns; the registry stores values by reference.
type Agent interface {
	// Name is the unique identifier used in config and the registry.
	Name() string

	// Mode tells the Bridge which backend to instantiate.
	Mode() Mode

	// Command is the spawn recipe's executable: the CLI binary name
	// (resolved via PATH at Start time) or an absolute path. Surfaced
	// by `nightme agents` so users can see what /run would spawn.
	Command() string

	// Args returns a defensive copy of the spawn recipe's default
	// argv (after the binary). Callers may not mutate the returned
	// slice; per-session overrides arrive separately via StartConfig.
	Args() []string

	// Detect verifies the agent is runnable (binary on PATH, SDK
	// available, etc.). Called before Start; an error aborts session
	// creation with a clear "X not found" message to the user.
	Detect() error

	// Start spawns (or attaches to) the agent and returns a live
	// AgentSession. The caller must Close() the session when done.
	Start(ctx context.Context, cfg StartConfig) (AgentSession, error)
}

// ContentBlockType discriminates the payload shape on a ContentBlock.
type ContentBlockType string

const (
	// ContentText is a plain-text segment. Text field is set.
	ContentText ContentBlockType = "text"

	// ContentImage is an image the agent can see. Path (absolute
	// filesystem path) and MediaType (MIME, e.g. "image/png") are
	// set. Implementations that support vision (Claude Code
	// stream-json in content-array mode) base64-encode and inline
	// the image; implementations without vision fall back to
	// emitting the path so the agent can read it with its file
	// tools.
	ContentImage ContentBlockType = "image"

	// ContentFile is any non-image file the agent can read (PDF,
	// source code, audio, video, etc.). Path is set; MediaType is
	// optional (advisory only — implementations that stream the
	// binary use it, others ignore it).
	ContentFile ContentBlockType = "file"
)

// ContentBlock is one element of a structured user turn. A turn
// contains zero or more blocks; implementations decide how to
// express them (see AgentSession.SendBlocks).
//
// Exactly one of Text / (Path, MediaType) is meaningful per block,
// based on Type:
//
//	ContentText  -> Text
//	ContentImage -> Path + MediaType
//	ContentFile  -> Path (+ optional MediaType)
type ContentBlock struct {
	Type ContentBlockType

	// Text is the segment for ContentText blocks. Empty for other
	// block types.
	Text string

	// Path is the absolute filesystem path for ContentImage /
	// ContentFile blocks. Empty for ContentText. Implementations
	// that stream the binary (e.g. Claude Code stream-json
	// content-array) read from this path at send time.
	Path string

	// MediaType is the MIME type for ContentImage (required for
	// vision-streaming implementations; e.g. "image/png",
	// "image/jpeg") and advisory for ContentFile. Empty for
	// ContentText.
	MediaType string
}

// AgentSession is the live, per-session handle. Session Manager drives
// it via the Events channel and the control methods.
type AgentSession interface {
	// Events streams AgentEvent values until the session ends. The
	// channel is closed by the implementation only when the
	// underlying process (or transport) terminates -- NOT after
	// every EventDone. Long-lived bridges that multiplex many
	// turns over a single process (e.g. Pi --mode rpc) emit
	// EventDone at the end of each turn and keep the channel
	// open until process exit or Close(). Channels and ChatSession
	// rely on the channel being closed as the universal
	// "session is over" signal; DoneEvent.Reason disambiguates
	// turn-end from process-end.
	Events() <-chan AgentEvent

	// PID returns the OS process id of the underlying child, or 0
	// when the session has no process (e.g. SDK backends that do
	// not spawn one). The Session Manager caches this value for
	// /run reconnect logic and for the registry.
	PID() int

	// SendText delivers plain-text user input. It is a convenience
	// wrapper around SendBlocks with a single ContentText block.
	// Implementations that support rich content MUST also implement
	// SendBlocks; callers that need image/file attachments must
	// use SendBlocks directly.
	//
	// In PTY mode the text is written as bytes to the child's
	// stdin. In ACP / SDK / JSON-IO modes it is structured into a
	// single-element content array.
	SendText(text string) error

	// SendBlocks delivers a structured user turn. Implementations
	// decide how to render each block:
	//
	//   - PTY: each ContentText block is written verbatim; each
	//     ContentImage / ContentFile block is rendered as the
	//     agent's file-reference syntax (Claude Code TUI: "@<path>"
	//     on its own line). Blocks are concatenated with "\n"
	//     separators so a single turn arrives atomically.
	//   - Claude Code stream-json: blocks are encoded into a
	//     content-array with text and base64-inlined image blocks
	//     (Anthropic API format).
	//   - ACP / SDK: blocks are encoded into the protocol's
	//     content-array shape.
	//
	// Implementations MUST handle an empty blocks slice as a no-op
	// (returns nil). Image / file blocks whose Path does not exist
	// are a per-implementation choice: most log a warning and drop
	// the block; some return an error.
	SendBlocks(ctx context.Context, blocks []ContentBlock) error

	// SendPermission responds to the most recent EventPermission. The
	// argument is the option string the user chose. Only meaningful in
	// ACP/SDK modes; PTY mode writes it verbatim to stdin.
	SendPermission(resp string) error

	// New resets the conversation context on the running session.
	// The underlying process (or transport, for long-lived bridges)
	// stays alive. Events() stays open. PID stays the same.
	// Subsequent SendText / SendBlocks operate on the fresh conversation.
	//
	// Bridge-specific implementations (F-34):
	//   - claudecode: writeLine("/clear")       // stdin slash command
	//   - pi:         send {"type":"new_session"} RPC
	//   - acp:        send "session/new" JSON-RPC over the existing transport
	//
	// After New returns, the bridge MUST emit a fresh EventAgentConnected carrying
	// the new SessionID; the runtime's AgentEventBus subscriber captures
	// it via SetResumeID and persists (cmd/nightme/run.go newEventHandler).
	New(ctx context.Context) error

	// Close terminates the session and releases resources. Idempotent.
	Close() error
}

// Errors surfaced by the registry.
var (
	// ErrUnknownAgent is returned by Registry.Get when no agent with
	// the requested name has been registered.
	ErrUnknownAgent = errors.New("agent: unknown agent")

	// ErrRestartRequired is returned by AgentSession.New when the
	// bridge cannot perform an in-place conversation reset (no
	// protocol-level /clear or equivalent). The wrapper layer
	// (chatsession.AgentSession.New) catches this sentinel and
	// falls back to a kill-and-respawn via the configured Spawner.
	// Returning nil here would be wrong: callers must distinguish
	// "successfully reset in-place" from "needs full restart".
	ErrRestartRequired = errors.New("agent: bridge requires restart for reset")
)

// sentinelErr is a small helper so tests can match errors with
// errors.Is without depending on the package-level variable identity.
type sentinelErr struct{ name string }

func (e *sentinelErr) Error() string { return "agent: " + e.name }

// duplicateRegistrationExists is returned by Registry.Register when
// an agent with the same name is already registered. Latest-wins
// semantics: the new instance replaces the old one. The bool returned
// by Register reports whether a replacement happened (useful for tests).
type duplicateError struct{ name string }

func (e *duplicateError) Error() string {
	return "agent: duplicate registration for " + e.name
}
